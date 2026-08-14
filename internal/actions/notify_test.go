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
	"strings"
	"testing"
)

const (
	testTitle   = "WAN failover"
	testMessage = "wan moved from primary to backup"
)

func TestNtfyPutsTheMessageInTheBodyAndTheTitleInAHeader(t *testing.T) {
	payload, err := NotificationPayload(TypeNtfy, testTitle, testMessage)
	if err != nil {
		t.Fatalf("NotificationPayload = %v", err)
	}
	if string(payload.Body) != testMessage {
		t.Fatalf("body = %q, want the message verbatim", payload.Body)
	}
	if payload.Header.Get("X-Title") != testTitle {
		t.Fatalf("X-Title = %q", payload.Header.Get("X-Title"))
	}
	if payload.Header.Get(headerContentType) != contentTypeText {
		t.Fatalf("content type = %q", payload.Header.Get(headerContentType))
	}
}

func TestNtfyTitleCannotSplitTheRequest(t *testing.T) {
	payload, err := NotificationPayload(TypeNtfy, "hi\r\nX-Forwarded-For: 192.0.2.1", testMessage)
	if err != nil {
		t.Fatalf("NotificationPayload = %v", err)
	}
	title := payload.Header.Get("X-Title")
	if strings.ContainsAny(title, "\r\n") {
		t.Fatalf("X-Title = %q still carries a line break", title)
	}
}

func TestDiscordAndSlackSendJSON(t *testing.T) {
	for actionType, field := range map[string]string{TypeDiscord: "content", TypeSlack: "text"} {
		payload, err := NotificationPayload(actionType, testTitle, testMessage)
		if err != nil {
			t.Fatalf("NotificationPayload(%s) = %v", actionType, err)
		}
		if payload.Header.Get(headerContentType) != contentTypeJSON {
			t.Errorf("%s content type = %q", actionType, payload.Header.Get(headerContentType))
		}
		var decoded map[string]string
		if err := json.Unmarshal(payload.Body, &decoded); err != nil {
			t.Fatalf("%s body is not JSON: %v", actionType, err)
		}
		want := testTitle + "\n" + testMessage
		if decoded[field] != want {
			t.Errorf("%s %s = %q, want %q", actionType, field, decoded[field], want)
		}
	}
}

func TestJSONTransportsEncodeRatherThanFormat(t *testing.T) {
	// A message containing JSON structure must arrive as text, not as
	// structure — the difference between a quoted value and an injection.
	payload, err := NotificationPayload(TypeSlack, "", `","channel":"#general`)
	if err != nil {
		t.Fatalf("NotificationPayload = %v", err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(payload.Body, &decoded); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if len(decoded) != 1 || decoded["text"] != `","channel":"#general` {
		t.Fatalf("decoded = %#v, want a single text field carrying the message verbatim", decoded)
	}
}

func TestNotificationBodyIsClipped(t *testing.T) {
	payload, err := NotificationPayload(TypeDiscord, "", strings.Repeat("z", maxNotificationBody+500))
	if err != nil {
		t.Fatalf("NotificationPayload = %v", err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(payload.Body, &decoded); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if len(decoded["content"]) > maxNotificationBody {
		t.Fatalf("content is %d bytes, want it clipped to %d", len(decoded["content"]), maxNotificationBody)
	}
}

func TestUnknownTransportIsRejected(t *testing.T) {
	if _, err := NotificationPayload("notification.telegram", "", testMessage); err == nil {
		t.Fatal("an unshipped transport must be rejected rather than sent somewhere by guesswork")
	}
}
