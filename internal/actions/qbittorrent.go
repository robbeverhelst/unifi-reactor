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
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// qBittorrentName is what the integration is called in an error message.
const qBittorrentName = "qBittorrent"

const (
	// qBittorrentCookie is the session cookie the WebUI sets on a successful
	// login. A login that answers 200 and sets no SID is how qBittorrent
	// reports a wrong username or password — it does not use a 401 — so the
	// absence of this cookie is the authentication check.
	qBittorrentCookie = "SID"
	// qBittorrentAll addresses every torrent. It is the only selector offered,
	// and deliberately: narrowing to a category or a tag would mean listing
	// torrents first and reading the response body back into Reactor, which is
	// a capability this package does not have and should not gain — a response
	// is drained and discarded precisely because it can echo a request back.
	qBittorrentAll = "hashes=all"
	// contentTypeForm is what the WebUI's API accepts. It predates its JSON
	// endpoints and every call here is form-encoded.
	contentTypeForm = "application/x-www-form-urlencoded"
)

// The WebUI paths, as segments so they are joined onto a base address rather
// than concatenated with it.
//
// pause and resume are the long-standing names and the ones #21 names.
// qBittorrent 5.0 (WebUI API 2.11) introduced stop and start and deprecated
// these; deprecated is not removed, and an instance that has removed them
// answers 404, which is reported against the Automation with the status in it
// rather than failing silently.
var (
	// qBittorrentV2 is the WebUI API's own prefix, under which everything here
	// lives.
	qBittorrentV2     = []string{"api", "v2"}
	qBittorrentLogin  = webUIPath("auth", "login")
	qBittorrentLogout = webUIPath("auth", "logout")
	qBittorrentPause  = webUIPath("torrents", "pause")
	qBittorrentResume = webUIPath("torrents", "resume")
)

func webUIPath(segments ...string) []string {
	return append(append([]string{}, qBittorrentV2...), segments...)
}

// QBittorrentRequest builds the exchange one pause or resume needs: log in,
// act, log out.
//
// Credentials are required rather than optional. A qBittorrent configured to
// bypass authentication for its subnet can already be driven by http.request —
// one POST, no session — and if that is your setup, that is the honest thing to
// write. The whole justification for a dedicated action is the login round
// trip, so an action that skipped it would be a worse http.request with a
// better name.
//
// Neither the password nor the session it produces is held anywhere: the
// password is read from the Secret for this one call, the cookie lives inside
// Client.Do, and the logout leg ends the session on the far end rather than
// leaving it to expire.
func QBittorrentRequest(actionType, base, username, password string, timeout time.Duration) (Request, error) {
	var path []string
	switch actionType {
	case TypeQBittorrentPause:
		path = qBittorrentPause
	case TypeQBittorrentResume:
		path = qBittorrentResume
	default:
		return Request{}, fmt.Errorf("unsupported qBittorrent action %q", actionType)
	}
	if username == "" || password == "" {
		return Request{}, fmt.Errorf(
			"secret has no %q and %q keys; %s issues a session rather than accepting a token, "+
				"so both are needed. An instance that bypasses authentication is an http.request, not this action",
			SecretKeyUsername, SecretKeyPassword, qBittorrentName)
	}

	action, err := endpointOn(base, qBittorrentName, path...)
	if err != nil {
		return Request{}, err
	}
	login, err := endpointOn(base, qBittorrentName, qBittorrentLogin...)
	if err != nil {
		return Request{}, err
	}
	logout, err := endpointOn(base, qBittorrentName, qBittorrentLogout...)
	if err != nil {
		return Request{}, err
	}

	// Form-encoded rather than formatted, so a password containing an ampersand
	// is a password rather than a second field.
	//
	// No Referer or Origin header is sent. qBittorrent's CSRF protection checks
	// those against its own host when they are present and allows a request
	// that carries neither, which is how every API client talks to it — sending
	// one would be inventing a browser this is not.
	credentials := url.Values{"username": {username}, "password": {password}}

	return Request{
		Method: http.MethodPost,
		URL:    action,
		Header: formHeader(),
		Body:   []byte(qBittorrentAll),
		// Pausing a paused torrent is a no-op and so is resuming a running one,
		// which makes this the one edge action here that can argue itself
		// idempotent rather than assert it. A retry re-runs the whole exchange,
		// login included; a rejected credential is not transient and stops at
		// the first attempt.
		Retryable: true,
		Timeout:   timeout,
		Session: &Session{
			Cookie: qBittorrentCookie,
			Login: Request{
				Method:  http.MethodPost,
				URL:     login,
				Header:  formHeader(),
				Body:    []byte(credentials.Encode()),
				Timeout: timeout,
			},
			Logout: Request{
				Method: http.MethodPost,
				URL:    logout,
				Header: formHeader(),
			},
		},
	}, nil
}

func formHeader() http.Header {
	header := http.Header{}
	header.Set(headerContentType, contentTypeForm)
	return header
}
