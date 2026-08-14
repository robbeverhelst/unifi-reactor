/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package actions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// The action types this package executes. They are edge actions: each fires on
// its own Automation's transition, owns no target and arbitrates with nothing.
const (
	TypeHTTPRequest = "http.request"
	TypeNtfy        = "notification.ntfy"
	TypeDiscord     = "notification.discord"
	TypeSlack       = "notification.slack"
	// TypeHomeAssistant calls one Home Assistant service. It is a shape over
	// http.request rather than a second transport: the same allowlist, the same
	// dialer floor, the same Secret rules. What it adds is that the path is
	// built from a domain and a service rather than written out, so the action
	// says what it is and cannot become a general request to an allowed host.
	TypeHomeAssistant = "homeassistant.service"
	// TypeQBittorrentPause and TypeQBittorrentResume pause and resume every
	// torrent on one qBittorrent instance.
	//
	// They are named as verbs rather than as one type with a paused flag, and
	// that is the whole design argument in the naming: an adjective is a level
	// and gets arbitrated, a verb is an occurrence and does not. Pausing is a
	// level in the world and an occurrence here, because the target is not a
	// Kubernetes object and there is nowhere to record the baseline a release
	// would need. See the README.
	TypeQBittorrentPause  = "qbittorrent.pause"
	TypeQBittorrentResume = "qbittorrent.resume"
)

// IsOutbound reports whether an action type leaves the cluster, and so whether
// it is one this package sends.
func IsOutbound(actionType string) bool {
	switch actionType {
	case TypeHTTPRequest, TypeNtfy, TypeDiscord, TypeSlack, TypeHomeAssistant,
		TypeQBittorrentPause, TypeQBittorrentResume:
		return true
	}
	return false
}

const (
	// retryAttempts is how many times a request declared safe to repeat is
	// tried before it is given up on. Retries happen inside one reconcile, not
	// across reconciles: a later reconcile has no new transition, so re-firing
	// there would be a duplicate rather than a retry.
	retryAttempts = 3
	// retryDelay is the pause before the second attempt, doubled for the third.
	retryDelay = time.Second
	// maxResponseBody is how much of a response is read before the rest is
	// discarded. It is read only to let the connection be reused and is never
	// logged, recorded or surfaced: a response can echo a request back,
	// credentials included.
	maxResponseBody = 4 << 10
	// dialTimeout and responseHeaderTimeout keep a black-holed destination from
	// consuming the whole per-attempt budget in the TCP handshake.
	dialTimeout           = 5 * time.Second
	responseHeaderTimeout = 10 * time.Second
	// userAgent identifies Reactor to whatever it calls, so an operator reading
	// an access log on the far end can tell what this traffic is.
	userAgent = "unifi-reactor"
	// headerCookie carries a session cookie back to the service that issued it,
	// and is the only header this package sets from a response.
	headerCookie = "Cookie"
	// logoutTimeout bounds ending a session. It is short and deliberately not
	// the action's own timeout: the action has already happened, and this is
	// tidying up after it rather than part of it.
	logoutTimeout = 3 * time.Second
	// DefaultTimeout bounds one attempt at an outbound action when the
	// Automation does not set spec.actions[].timeoutSeconds. It is shorter than
	// the desired-state default because an edge action runs inside a reconcile
	// and may be tried three times, and because nothing downstream corrects it
	// if it hangs.
	DefaultTimeout = 10 * time.Second
)

// Request is one outbound call, fully resolved: credentials already merged in
// from a Secret, body already rendered.
type Request struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
	// Retryable declares that repeating this request is harmless. False means
	// exactly one attempt — the at-most-once policy non-idempotent edge actions
	// get, because a duplicate of an unknown POST is not noise, it is a second
	// side effect.
	Retryable bool
	// Timeout bounds one attempt. In a session it bounds each leg separately.
	Timeout time.Duration
	// Session, when set, is a login performed immediately before this request
	// and torn down immediately after it. Nil for everything that authenticates
	// with a static credential, which is almost everything.
	Session *Session
}

// Session is a login exchange wrapped around one request, for a service that
// authenticates with a cookie it issues rather than with a token you hold.
//
// It exists because the rule everywhere else here — a credential is never held
// longer than the request that uses it — is not automatically true of a
// session. A session cookie is a bearer of the same authority as the password
// that produced it, so caching one across reconciles would be exactly the thing
// this package does not do with the password itself.
//
// So there is no cache and no session store. The login happens inside the one
// action, the cookie lives in a local variable for the two or three requests
// that need it, and Logout ends it on the server rather than leaving it to
// expire. The cost is one extra round trip per action; the benefit is that the
// rule holds as written, on both ends of the connection.
//
// The cookie is never logged, never put in status, never attached to an Event
// and never reaches a template. Nothing but the origin escapes this package.
type Session struct {
	// Login establishes the session. Its response is read for one cookie and
	// otherwise discarded like any other.
	Login Request

	// Cookie is the name of the cookie the login is expected to set. A login
	// that answers 200 and sets no such cookie is a failure, not a success:
	// that is how qBittorrent reports a rejected username or password.
	Cookie string

	// Logout ends the session. It is best effort — the action has already
	// happened by the time it runs, and a session Reactor could not close is
	// not a reason to report the action as failed. A zero URL skips it.
	Logout Request
}

// Result describes a delivered request. It carries no response body and no full
// URL by construction, so it is safe to put anywhere an operator can read.
type Result struct {
	// Origin is the destination as scheme, host and port.
	Origin string
	// Status is the HTTP status code returned.
	Status int
	// Attempts is how many tries it took.
	Attempts int32
}

// Doer sends one outbound request. The controller depends on this rather than
// on Client so that tests can exercise the edge-action path without a network.
type Doer interface {
	// Enabled reports whether any destination is allowed at all.
	Enabled() bool
	// Do sends the request, retrying only when the request says it may be.
	Do(ctx context.Context, req Request) (Result, error)
}

// Client is the real Doer.
type Client struct {
	policy Policy
	http   *http.Client
}

// NewClient builds a Doer bound to one destination policy.
//
// The transport is this package's own rather than http.DefaultTransport,
// because the address floor is enforced in the dialer: that is the only place
// where the address actually connected to is known, which is what makes it hold
// against a hostname that resolves somewhere else than it appears to.
func NewClient(policy Policy) *Client {
	dialer := &net.Dialer{Timeout: dialTimeout, Control: policy.controlAddress}
	return &Client{
		policy: policy,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:           dialer.DialContext,
				ResponseHeaderTimeout: responseHeaderTimeout,
				// Redirects are not followed, so this only ever holds one
				// connection per destination for the life of an action.
				MaxIdleConnsPerHost: 1,
			},
			// A redirect is a destination the allowlist never approved, chosen
			// by whatever answered — the classic way an approved host is used
			// to reach a blocked one.
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				return fmt.Errorf("refusing to follow a redirect to %s", originOf(req.URL))
			},
		},
	}
}

func (c *Client) Enabled() bool { return !c.policy.Empty() }

// Do sends the request, checking the destination first and retrying only a
// request that has declared itself safe to repeat.
//
// A request carrying a Session is one unit: an attempt is the whole
// login-act-logout exchange, and a retry logs in again rather than reusing a
// cookie from the attempt that failed. That is the conservative reading — the
// attempt may have failed precisely because the session was no longer good —
// and it is what keeps a retry from being the one place a session outlives the
// request it was made for.
func (c *Client) Do(ctx context.Context, req Request) (Result, error) {
	target, origin, err := c.policy.Check(req.URL)
	if err != nil {
		return Result{Origin: origin}, err
	}
	if req.Session == nil {
		return c.repeat(ctx, origin, req.Retryable, func(ctx context.Context) (int, error) {
			return c.attempt(ctx, target, req)
		})
	}

	// Every leg is checked against the allowlist on its own. They come from one
	// base address today, so this is belt and braces — but a session whose
	// login went somewhere the allowlist never approved is exactly the shape of
	// bug that would be invisible until it mattered.
	endpoints, err := c.policy.checkSession(req.Session)
	if err != nil {
		return Result{Origin: origin}, err
	}
	return c.repeat(ctx, origin, req.Retryable, func(ctx context.Context) (int, error) {
		return c.runSession(ctx, target, req, endpoints)
	})
}

// repeat runs one attempt at a time until it succeeds, stops being worth
// repeating, or runs out of attempts.
func (c *Client) repeat(
	ctx context.Context,
	origin string,
	retryable bool,
	attempt func(context.Context) (int, error),
) (Result, error) {
	attempts := 1
	if retryable {
		attempts = retryAttempts
	}

	var lastErr error
	for try := 1; try <= attempts; try++ {
		if try > 1 {
			if err := wait(ctx, retryDelay<<(try-2)); err != nil {
				return Result{Origin: origin, Attempts: int32(try - 1)}, lastErr
			}
		}
		status, err := attempt(ctx)
		result := Result{Origin: origin, Status: status, Attempts: int32(try)}
		if err == nil {
			return result, nil
		}
		lastErr = fmt.Errorf("%s: %w", origin, err)
		if !transient(err) {
			return result, lastErr
		}
	}
	return Result{Origin: origin, Attempts: int32(attempts)}, lastErr
}

// wait sleeps unless the caller's context runs out first.
func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// attempt makes one request and classifies the outcome.
func (c *Client) attempt(ctx context.Context, target *url.URL, req Request) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeoutOf(req))
	defer cancel()

	response, err := c.send(ctx, target, req)
	if err != nil {
		return 0, err
	}
	defer drain(response)
	return classify(response)
}

// send performs one request. The context must already carry the attempt's
// timeout, because the caller owns the response — and so the reading of its
// body, which the deadline has to outlive.
func (c *Client) send(ctx context.Context, target *url.URL, req Request) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, target.String(), bytes.NewReader(req.Body))
	if err != nil {
		// Only reachable for a malformed method, which the CRD enum prevents.
		return nil, errors.New("could not build the request")
	}
	httpReq.Header = req.Header.Clone()
	if httpReq.Header == nil {
		httpReq.Header = http.Header{}
	}
	httpReq.Header.Set("User-Agent", userAgent)

	response, err := c.http.Do(httpReq)
	if err != nil {
		return nil, sanitize(err)
	}
	return response, nil
}

// classify turns a status code into the outcome of an attempt, deciding what
// is worth another try.
func classify(response *http.Response) (int, error) {
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return response.StatusCode, nil
	case response.StatusCode == http.StatusTooManyRequests, response.StatusCode >= 500:
		return response.StatusCode, transientError{err: fmt.Errorf("responded %s", response.Status)}
	default:
		return response.StatusCode, fmt.Errorf("responded %s", response.Status)
	}
}

// drain reads a bounded prefix of a response so the connection can be reused,
// and closes it. Nothing read here is kept: a response can echo a request back,
// credentials included.
func drain(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBody))
	_ = response.Body.Close()
}

func timeoutOf(req Request) time.Duration {
	if req.Timeout <= 0 {
		return DefaultTimeout
	}
	return req.Timeout
}

// sessionEndpoints are the checked login and logout URLs of one session.
// logout is nil when the session declares none.
type sessionEndpoints struct{ login, logout *url.URL }

// checkSession validates every leg of a session against the allowlist before
// any of them is dialled.
func (p Policy) checkSession(session *Session) (sessionEndpoints, error) {
	if session.Cookie == "" {
		return sessionEndpoints{}, errors.New("a session names no cookie to look for")
	}
	if session.Login.Session != nil {
		// Not reachable from any action here. It is refused rather than
		// recursed into, because a login that needs a login is a configuration
		// mistake and not a thing worth supporting.
		return sessionEndpoints{}, errors.New("a session login may not itself need a session")
	}

	login, _, err := p.Check(session.Login.URL)
	if err != nil {
		return sessionEndpoints{}, err
	}
	endpoints := sessionEndpoints{login: login}
	if session.Logout.URL != "" {
		logout, _, err := p.Check(session.Logout.URL)
		if err != nil {
			return sessionEndpoints{}, err
		}
		endpoints.logout = logout
	}
	return endpoints, nil
}

// runSession logs in, performs the request with the session cookie, and ends
// the session whatever the request did.
func (c *Client) runSession(
	ctx context.Context,
	target *url.URL,
	req Request,
	endpoints sessionEndpoints,
) (int, error) {
	cookie, err := c.login(ctx, endpoints.login, *req.Session)
	if err != nil {
		return 0, err
	}
	defer c.logout(ctx, endpoints.logout, *req.Session, cookie)

	authenticated := req
	authenticated.Header = req.Header.Clone()
	if authenticated.Header == nil {
		authenticated.Header = http.Header{}
	}
	authenticated.Header.Set(headerCookie, cookie)
	return c.attempt(ctx, target, authenticated)
}

// login performs the login leg and returns the session cookie as a Cookie
// header value.
//
// A 200 that sets no cookie is a failure, and not a transient one: that is how
// qBittorrent reports a wrong username or password, and a credential does not
// get better by asking again.
func (c *Client) login(ctx context.Context, target *url.URL, session Session) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeoutOf(session.Login))
	defer cancel()

	response, err := c.send(ctx, target, session.Login)
	if err != nil {
		return "", fmt.Errorf("logging in: %w", err)
	}
	defer drain(response)
	if _, err := classify(response); err != nil {
		return "", fmt.Errorf("logging in: %w", err)
	}

	for _, cookie := range response.Cookies() {
		if cookie.Name != session.Cookie || cookie.Value == "" {
			continue
		}
		// Rebuilt with the name and value only. A cookie parsed from a
		// Set-Cookie header serializes back as a Set-Cookie header, attributes
		// and all, which is not what belongs in a Cookie request header.
		return (&http.Cookie{Name: cookie.Name, Value: cookie.Value}).String(), nil
	}
	return "", fmt.Errorf(
		"the login was accepted but set no %q cookie, which is how a rejected username or password is reported",
		session.Cookie)
}

// logout ends the session, best effort.
//
// It runs on a context detached from the caller's, because by the time it runs
// the action's own deadline may be spent — and a session left open on the far
// end is precisely what this exists to prevent. Its failure is not reported:
// the action has already happened, and a session Reactor could not close is not
// a reason to tell an operator the action did not work.
func (c *Client) logout(ctx context.Context, target *url.URL, session Session, cookie string) {
	if target == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), logoutTimeout)
	defer cancel()

	request := session.Logout
	request.Header = request.Header.Clone()
	if request.Header == nil {
		request.Header = http.Header{}
	}
	request.Header.Set(headerCookie, cookie)

	response, err := c.send(ctx, target, request)
	if err != nil {
		return
	}
	drain(response)
}

// transientError marks a failure worth another attempt — but only for a request
// that said repeating it is harmless.
type transientError struct{ err error }

func (t transientError) Error() string { return t.err.Error() }
func (t transientError) Unwrap() error { return t.err }

func transient(err error) bool {
	var t transientError
	return errors.As(err, &t)
}

// sanitize strips the URL out of a transport error. net/http wraps every
// failure in a *url.Error carrying the full request URL, which for a webhook is
// the credential — so an unsanitized error reaching a log line, a status field
// or an Event would publish it.
func sanitize(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return transientError{err: errors.New("timed out")}
	}
	if errors.Is(err, context.Canceled) {
		return errors.New("cancelled")
	}
	return transientError{err: err}
}

// originOf renders a URL as scheme, host and port, dropping the path and query.
func originOf(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = defaultPort(u.Scheme)
	}
	return fmt.Sprintf("%s://%s:%s", u.Scheme, u.Hostname(), port)
}
