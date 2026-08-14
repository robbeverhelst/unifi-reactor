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
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	qbitUser     = "reactor"
	qbitPassword = "hunter2"
	// qbitSID is what the stub issues. A real one is opaque and short-lived;
	// what matters here is that it comes back on the next request and on
	// nothing that leaves this package.
	qbitSID = "sid-example"
)

// qbitStub is a stand-in for a qBittorrent WebUI. It records the order it was
// called in and what each call carried, which is the whole contract: log in,
// act with the cookie, log out.
type qbitStub struct {
	mu sync.Mutex
	// calls is the path of each request, in order.
	calls []string
	// cookies is the Cookie header each request carried, in the same order.
	cookies []string
	// bodies is the body each request carried, in the same order.
	bodies []string
	// rejectLogin answers the login the way qBittorrent answers a wrong
	// password: 200, no cookie, the word Fails.
	rejectLogin bool
	// failAction is a status to answer the pause or resume with. Zero means 200.
	failAction int
	// failActionOnce answers failAction only for the first action call.
	failActionOnce bool
	actionCalls    int
}

func (s *qbitStub) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	s.calls = append(s.calls, r.URL.Path)
	s.cookies = append(s.cookies, r.Header.Get(headerCookie))
	s.bodies = append(s.bodies, string(body))

	switch {
	case strings.HasSuffix(r.URL.Path, "/auth/login"):
		if s.rejectLogin {
			_, _ = w.Write([]byte("Fails."))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: qBittorrentCookie, Value: qbitSID, Path: "/", HttpOnly: true})
		_, _ = w.Write([]byte("Ok."))
	case strings.HasSuffix(r.URL.Path, "/auth/logout"):
		w.WriteHeader(http.StatusOK)
	default:
		s.actionCalls++
		if s.failAction != 0 && (!s.failActionOnce || s.actionCalls == 1) {
			w.WriteHeader(s.failAction)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func (s *qbitStub) seen() ([]string, []string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...),
		append([]string(nil), s.cookies...),
		append([]string(nil), s.bodies...)
}

// qbitRequest builds the exchange under test against a stub's base URL.
func qbitRequest(t *testing.T, actionType, base string) Request {
	t.Helper()
	req, err := QBittorrentRequest(actionType, base, qbitUser, qbitPassword, 5*time.Second)
	if err != nil {
		t.Fatalf("QBittorrentRequest = %v", err)
	}
	return req
}

func TestQBittorrentLogsInActsAndLogsOut(t *testing.T) {
	webui := &qbitStub{}
	server, _ := stub(t, webui.handler)

	result, err := localClient(t).Do(context.Background(),
		qbitRequest(t, TypeQBittorrentPause, server.URL))
	if err != nil {
		t.Fatalf("Do = %v", err)
	}
	if result.Status != http.StatusOK || result.Attempts != 1 {
		t.Fatalf("result = %+v, want one attempt and a 200", result)
	}

	calls, cookies, bodies := webui.seen()
	want := []string{"/api/v2/auth/login", "/api/v2/torrents/pause", "/api/v2/auth/logout"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
	}

	if cookies[0] != "" {
		t.Fatalf("the login carried a cookie %q; there is no session yet", cookies[0])
	}
	sessionCookie := qBittorrentCookie + "=" + qbitSID
	if cookies[1] != sessionCookie || cookies[2] != sessionCookie {
		t.Fatalf("cookies = %v, want the session on the action and the logout", cookies)
	}
	if !strings.Contains(bodies[0], "username="+qbitUser) || !strings.Contains(bodies[0], "password=") {
		t.Fatalf("login body = %q, want form-encoded credentials", bodies[0])
	}
	if bodies[1] != qBittorrentAll {
		t.Fatalf("action body = %q, want %q", bodies[1], qBittorrentAll)
	}
}

func TestQBittorrentResumeHitsTheResumeEndpoint(t *testing.T) {
	webui := &qbitStub{}
	server, _ := stub(t, webui.handler)

	if _, err := localClient(t).Do(context.Background(),
		qbitRequest(t, TypeQBittorrentResume, server.URL)); err != nil {
		t.Fatalf("Do = %v", err)
	}
	calls, _, _ := webui.seen()
	if calls[1] != "/api/v2/torrents/resume" {
		t.Fatalf("acted on %q, want the resume endpoint", calls[1])
	}
}

// qBittorrent answers a wrong username or password with 200 and no cookie, not
// with a 401. The absence of the cookie is therefore the authentication check,
// and it must not be retried: a rejected credential does not get better.
func TestQBittorrentTreatsALoginWithNoCookieAsAFailure(t *testing.T) {
	webui := &qbitStub{rejectLogin: true}
	server, _ := stub(t, webui.handler)

	_, err := localClient(t).Do(context.Background(),
		qbitRequest(t, TypeQBittorrentPause, server.URL))
	if err == nil {
		t.Fatal("a login that set no session cookie must be reported as a failure")
	}
	if !strings.Contains(err.Error(), qBittorrentCookie) {
		t.Fatalf("error = %v, want it to name the missing cookie", err)
	}

	calls, _, _ := webui.seen()
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want the login only: nothing was acted on and nothing was retried", calls)
	}
}

// The exchange is the unit of retry. A transient failure re-runs the login too,
// rather than reusing a cookie from the attempt that just failed.
func TestQBittorrentRetriesTheWholeExchange(t *testing.T) {
	webui := &qbitStub{failAction: http.StatusBadGateway, failActionOnce: true}
	server, _ := stub(t, webui.handler)

	result, err := localClient(t).Do(context.Background(),
		qbitRequest(t, TypeQBittorrentPause, server.URL))
	if err != nil {
		t.Fatalf("Do = %v", err)
	}
	if result.Attempts != 2 {
		t.Fatalf("attempts = %d, want the second attempt to have succeeded", result.Attempts)
	}

	calls, _, _ := webui.seen()
	logins := 0
	for _, call := range calls {
		if strings.HasSuffix(call, "/auth/login") {
			logins++
		}
	}
	if logins != 2 {
		t.Fatalf("logins = %d in %v, want one per attempt", logins, calls)
	}
}

// The session is ended on the far end even when the action failed, because the
// point of not caching it is that it does not outlive the request either here
// or there.
func TestQBittorrentLogsOutAfterAFailedAction(t *testing.T) {
	webui := &qbitStub{failAction: http.StatusForbidden}
	server, _ := stub(t, webui.handler)

	if _, err := localClient(t).Do(context.Background(),
		qbitRequest(t, TypeQBittorrentPause, server.URL)); err == nil {
		t.Fatal("a 403 must be reported as a failure")
	}

	calls, _, _ := webui.seen()
	if len(calls) == 0 || !strings.HasSuffix(calls[len(calls)-1], "/auth/logout") {
		t.Fatalf("calls = %v, want the session ended anyway", calls)
	}
}

// Nothing derived from the response — the cookie above all — may reach an
// error, which is where a credential would escape this package.
func TestQBittorrentErrorsCarryNoSession(t *testing.T) {
	webui := &qbitStub{failAction: http.StatusForbidden}
	server, _ := stub(t, webui.handler)

	_, err := localClient(t).Do(context.Background(),
		qbitRequest(t, TypeQBittorrentPause, server.URL))
	if err == nil {
		t.Fatal("expected a failure")
	}
	if strings.Contains(err.Error(), qbitSID) || strings.Contains(err.Error(), qbitPassword) {
		t.Fatalf("the error carried a credential: %v", err)
	}
}

func TestQBittorrentNeedsBothCredentials(t *testing.T) {
	for _, credentials := range [][2]string{{"", qbitPassword}, {qbitUser, ""}, {"", ""}} {
		_, err := QBittorrentRequest(
			TypeQBittorrentPause, "http://qbittorrent.example.com:8080",
			credentials[0], credentials[1], time.Second)
		if err == nil {
			t.Fatalf("credentials %q/%q were accepted", credentials[0], credentials[1])
		}
	}
}

func TestQBittorrentRefusesAnUnknownAction(t *testing.T) {
	if _, err := QBittorrentRequest(
		TypeHTTPRequest, "http://qbittorrent.example.com:8080",
		qbitUser, qbitPassword, time.Second); err == nil {
		t.Fatal("an action type this integration does not implement was accepted")
	}
}

// Every leg is checked against the allowlist, not only the one that acts.
func TestSessionLegsAreCheckedAgainstTheAllowlist(t *testing.T) {
	webui := &qbitStub{}
	server, _ := stub(t, webui.handler)

	request := qbitRequest(t, TypeQBittorrentPause, server.URL)
	request.Session.Login.URL = "https://elsewhere.example.com/api/v2/auth/login"

	client := NewClient(mustPolicy(t, server.URL))
	if _, err := client.Do(context.Background(), request); err == nil {
		t.Fatal("a login pointing off the allowlist must be refused")
	}
	if calls, _, _ := webui.seen(); len(calls) != 0 {
		t.Fatalf("calls = %v, want nothing dialled", calls)
	}
}
