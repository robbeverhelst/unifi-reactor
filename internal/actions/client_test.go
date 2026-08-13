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
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// localClient is a Client permitted to reach a stub server on the loopback
// interface, which the address floor otherwise refuses. Nothing outside this
// package can build one: no configuration reaches allowLoopback.
func localClient(t *testing.T) *Client {
	t.Helper()
	policy := mustPolicy(t, allowAny)
	policy.allowLoopback = true
	return NewClient(policy)
}

// stub is a server that counts requests and answers with whatever is asked of
// it. Every test here talks to it and to nothing else — no test in this repo
// reaches a real service.
func stub(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func TestDoSendsBodyAndHeaders(t *testing.T) {
	var gotBody, gotAuth, gotAgent string
	server, calls := stub(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody, gotAuth, gotAgent = string(body), r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusNoContent)
	})

	result, err := localClient(t).Do(context.Background(), Request{
		Method:  http.MethodPost,
		URL:     server.URL + "/hook",
		Header:  http.Header{"Authorization": []string{"Bearer example"}},
		Body:    []byte(`{"text":"wan moved to backup"}`),
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Do = %v", err)
	}
	if result.Status != http.StatusNoContent || result.Attempts != 1 {
		t.Fatalf("result = %+v, want one attempt and a 204", result)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want exactly one", calls.Load())
	}
	if gotBody != `{"text":"wan moved to backup"}` || gotAuth != "Bearer example" {
		t.Fatalf("body = %q, authorization = %q", gotBody, gotAuth)
	}
	if gotAgent != userAgent {
		t.Fatalf("user agent = %q, want %q", gotAgent, userAgent)
	}
	if strings.Contains(result.Origin, "/hook") {
		t.Fatalf("origin %q leaked the path", result.Origin)
	}
}

func TestRetryableRequestRetriesA5xx(t *testing.T) {
	var seen atomic.Int32
	server, _ := stub(t, func(w http.ResponseWriter, _ *http.Request) {
		if seen.Add(1) < retryAttempts {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	result, err := localClient(t).Do(context.Background(), Request{
		Method: http.MethodPost, URL: server.URL, Retryable: true, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Do = %v", err)
	}
	if result.Attempts != retryAttempts {
		t.Fatalf("attempts = %d, want %d", result.Attempts, retryAttempts)
	}
}

func TestNonRetryableRequestIsAttemptedExactlyOnce(t *testing.T) {
	server, calls := stub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})

	result, err := localClient(t).Do(context.Background(), Request{
		Method: http.MethodPost, URL: server.URL, Retryable: false, Timeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("a 502 must be reported as a failure")
	}
	if calls.Load() != 1 || result.Attempts != 1 {
		t.Fatalf("calls = %d, attempts = %d; a non-idempotent request must be sent at most once",
			calls.Load(), result.Attempts)
	}
}

func TestA4xxIsNotRetriedEvenWhenRetryable(t *testing.T) {
	server, calls := stub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	if _, err := localClient(t).Do(context.Background(), Request{
		Method: http.MethodPost, URL: server.URL, Retryable: true, Timeout: 5 * time.Second,
	}); err == nil {
		t.Fatal("a 403 must be reported as a failure")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1: a rejected credential does not get better by asking again", calls.Load())
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	final, finalCalls := stub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	redirector, _ := stub(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	})

	if _, err := localClient(t).Do(context.Background(), Request{
		Method: http.MethodGet, URL: redirector.URL, Timeout: 5 * time.Second,
	}); err == nil {
		t.Fatal("a redirect must be refused: it names a destination the allowlist never approved")
	}
	if finalCalls.Load() != 0 {
		t.Fatal("the redirect target was reached")
	}
}

func TestErrorsNeverQuoteTheURL(t *testing.T) {
	// A server that hangs up mid-request is the case where net/http's own error
	// text carries the full URL, credential path and all.
	server, _ := stub(t, func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hijacker.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})

	_, err := localClient(t).Do(context.Background(), Request{
		Method: http.MethodPost, URL: server.URL + secretPath, Timeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("expected a transport failure")
	}
	if strings.Contains(err.Error(), secretPath) {
		t.Fatalf("the error quoted the URL path, which for a webhook is the credential: %v", err)
	}
}

func TestTimeoutIsPerAttempt(t *testing.T) {
	server, _ := stub(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	})

	start := time.Now()
	_, err := localClient(t).Do(context.Background(), Request{
		Method: http.MethodPost, URL: server.URL, Timeout: 100 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected the attempt to time out")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %s, want the request abandoned at its timeout", elapsed)
	}
}

func TestDisabledClientRefusesWithoutDialing(t *testing.T) {
	server, calls := stub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	client := NewClient(Policy{})
	if client.Enabled() {
		t.Fatal("a client with no allowed destinations must report itself disabled")
	}
	if _, err := client.Do(context.Background(), Request{
		Method: http.MethodPost, URL: server.URL, Timeout: time.Second,
	}); err == nil {
		t.Fatal("expected the request to be refused")
	}
	if calls.Load() != 0 {
		t.Fatal("a refused destination must not be contacted")
	}
}
