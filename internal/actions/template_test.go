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

// stateBackup is the observed value the transition under test moves to.
const stateBackup = "backup"

func transitionContext() Context {
	return Context{
		Automation: "media/pause-on-backup-wan",
		Namespace:  "media",
		Name:       "pause-on-backup-wan",
		Provider:   "unifi",
		Matching:   true,
		Key:        "wan",
		From:       "primary",
		To:         stateBackup,
		State:      map[string]string{"wan": stateBackup},
		Time:       "2026-08-13T02:41:07Z",
	}
}

func TestRenderSubstitutesTheTransition(t *testing.T) {
	got, err := Render("{{ .Automation }}: {{ .Key }} {{ .From }} -> {{ .To }}", transitionContext())
	if err != nil {
		t.Fatalf("Render = %v", err)
	}
	want := "media/pause-on-backup-wan: wan primary -> backup"
	if got != want {
		t.Fatalf("Render = %q, want %q", got, want)
	}
}

func TestRenderReadsObservedState(t *testing.T) {
	for _, form := range []string{"{{ .State.wan }}", `{{ index .State "wan" }}`} {
		got, err := Render(form, transitionContext())
		if err != nil {
			t.Fatalf("Render(%s) = %v", form, err)
		}
		if got != stateBackup {
			t.Fatalf("Render(%s) = %q, want %s", form, got, stateBackup)
		}
	}
}

func TestRenderFailsOnAnUnknownStateKey(t *testing.T) {
	// A typo must fail at the moment the notification would have gone out,
	// rather than quietly delivering the words "no value" to whoever was
	// supposed to be told something. This is what missingkey=error buys, and it
	// only covers the field form — the index builtin returns the zero value for
	// a key that is not there, which the README says out loud.
	if _, err := Render("{{ .State.wam }}", transitionContext()); err == nil {
		t.Fatal("an unknown state key must be an error")
	}
}

func TestRenderFailsOnAnUnknownField(t *testing.T) {
	if _, err := Render("{{ .Password }}", transitionContext()); err == nil {
		t.Fatal("a field that does not exist must be an error, not an empty string")
	}
}

func TestJSONFuncQuotesAValueSafely(t *testing.T) {
	data := transitionContext()
	data.To = `back"up` + "\n" + `{"injected":true}`

	rendered, err := Render(`{"text": {{ json .To }}}`, data)
	if err != nil {
		t.Fatalf("Render = %v", err)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(rendered), &decoded); err != nil {
		t.Fatalf("the rendered body is not valid JSON: %v (%s)", err, rendered)
	}
	if decoded["text"] != data.To {
		t.Fatalf("text = %q, want the value carried through unchanged", decoded["text"])
	}
}

func TestRenderIsBounded(t *testing.T) {
	// A template must not be able to grow a body into a bulk transfer out of
	// the cluster, and must fail while rendering rather than after allocating
	// what it would have sent.
	data := transitionContext()
	data.To = strings.Repeat("y", maxRendered)
	if _, err := Render("{{ .To }}{{ .To }}", data); err == nil {
		t.Fatal("a template rendering past the bound must fail")
	}
}

func TestRenderOfAnEmptyTemplateIsEmpty(t *testing.T) {
	got, err := Render("", transitionContext())
	if err != nil || got != "" {
		t.Fatalf("Render(\"\") = %q, %v", got, err)
	}
}
