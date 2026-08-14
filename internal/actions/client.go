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
)

// IsOutbound reports whether an action type leaves the cluster, and so whether
// it is one this package sends.
func IsOutbound(actionType string) bool {
	switch actionType {
	case TypeHTTPRequest, TypeNtfy, TypeDiscord, TypeSlack, TypeHomeAssistant:
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
	// Timeout bounds one attempt.
	Timeout time.Duration
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
func (c *Client) Do(ctx context.Context, req Request) (Result, error) {
	target, origin, err := c.policy.Check(req.URL)
	if err != nil {
		return Result{Origin: origin}, err
	}

	attempts := 1
	if req.Retryable {
		attempts = retryAttempts
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			if err := wait(ctx, retryDelay<<(attempt-2)); err != nil {
				return Result{Origin: origin, Attempts: int32(attempt - 1)}, lastErr
			}
		}
		status, err := c.attempt(ctx, target, req)
		result := Result{Origin: origin, Status: status, Attempts: int32(attempt)}
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
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, target.String(), bytes.NewReader(req.Body))
	if err != nil {
		// Only reachable for a malformed method, which the CRD enum prevents.
		return 0, errors.New("could not build the request")
	}
	httpReq.Header = req.Header.Clone()
	if httpReq.Header == nil {
		httpReq.Header = http.Header{}
	}
	httpReq.Header.Set("User-Agent", userAgent)

	response, err := c.http.Do(httpReq)
	if err != nil {
		return 0, sanitize(err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBody))
		_ = response.Body.Close()
	}()

	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return response.StatusCode, nil
	case response.StatusCode == http.StatusTooManyRequests, response.StatusCode >= 500:
		return response.StatusCode, transientError{err: fmt.Errorf("responded %s", response.Status)}
	default:
		return response.StatusCode, fmt.Errorf("responded %s", response.Status)
	}
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
