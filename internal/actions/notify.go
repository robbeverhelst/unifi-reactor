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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode"
)

const (
	headerContentType = "Content-Type"
	contentTypeJSON   = "application/json"
	contentTypeText   = "text/plain; charset=utf-8"
	// maxNotificationBody clips a composed message to something every transport
	// accepts. Discord rejects a content field over 2000 characters outright,
	// and a notification that fails to send because it was too long is the one
	// failure mode this feature exists to avoid.
	maxNotificationBody = 1900
	// maxTitleHeader bounds the ntfy title, which travels as a header and so
	// has to stay short and printable.
	maxTitleHeader = 200
)

// Payload is the transport-shaped body of a notification.
type Payload struct {
	Body   []byte
	Header http.Header
}

// NotificationPayload turns a rendered title and message into the body one
// transport wants.
//
// Every transport here is a plain POST whose destination is a URL that is
// itself the credential, which is why the URL is never expressible in an
// Automation and always comes from a Secret. Telegram is deliberately absent:
// its bot token sits in the URL path alongside a separate chat id, which does
// not fit that shape without a second credential field, and shipping one
// transport badly is worse than shipping three well.
func NotificationPayload(actionType, title, message string) (Payload, error) {
	header := http.Header{}
	switch actionType {
	case TypeNtfy:
		// ntfy publishes the message as the raw body and takes the title from a
		// header, which is why the title is stripped to printable ASCII: a
		// newline in a header value is a request-splitting primitive, and Go
		// would reject the request rather than send it.
		header.Set(headerContentType, contentTypeText)
		if safe := headerSafe(title); safe != "" {
			header.Set("X-Title", safe)
		}
		return Payload{Body: []byte(clip(message, maxNotificationBody)), Header: header}, nil

	case TypeDiscord:
		return jsonPayload(header, "content", compose(title, message))

	case TypeSlack:
		return jsonPayload(header, "text", compose(title, message))
	}
	return Payload{}, fmt.Errorf("unsupported notification type %q", actionType)
}

// jsonPayload builds the body with encoding/json rather than by formatting a
// string, so a message containing a quote or a brace is data rather than
// structure however it was templated.
func jsonPayload(header http.Header, field, value string) (Payload, error) {
	body, err := json.Marshal(map[string]string{field: clip(value, maxNotificationBody)})
	if err != nil {
		return Payload{}, fmt.Errorf("encoding the notification body: %w", err)
	}
	header.Set(headerContentType, contentTypeJSON)
	return Payload{Body: body, Header: header}, nil
}

// compose folds a title into the message for transports that have no title of
// their own.
func compose(title, message string) string {
	if title == "" {
		return message
	}
	return title + "\n" + message
}

// clip truncates on a rune boundary so a cut never produces invalid UTF-8.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	runes := []rune(s)
	for len(string(runes)) > limit {
		runes = runes[:len(runes)-1]
	}
	return string(runes)
}

// headerSafe reduces a value to what may travel in a header: printable ASCII,
// bounded in length.
func headerSafe(value string) string {
	var out strings.Builder
	for _, r := range value {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			continue
		}
		out.WriteRune(r)
		if out.Len() >= maxTitleHeader {
			break
		}
	}
	return strings.TrimSpace(out.String())
}
