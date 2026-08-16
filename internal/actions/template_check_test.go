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
	"strings"
	"testing"
)

// matchedKeys is the spec.when.state of the Automation these cases are written
// against: it matches on the uplink key alone, the way the guide's first
// example does, and never on the carrier behind it.
var matchedKeys = []string{matchedKey}

const (
	matchedKey = "wan"
	// unmatchedKey is what a fault naming the carrier key reads like.
	unmatchedKey = `state key "isp"`
)

func TestCheckTemplateAcceptsWhatRenderCanProduce(t *testing.T) {
	// Every one of these renders against transitionContext(), so nothing here
	// may be reported: a false accusation reports a working Automation as
	// broken, which is worse than the trap this check exists for.
	for _, text := range []string{
		"",
		"WAN failed over",
		"{{ .Key }} moved from {{ .From }} to {{ .To }} at {{ .Time }}",
		"{{ .Automation }} {{ .Namespace }} {{ .Name }} {{ .Provider }} {{ .Matching }}",
		"uplink is now {{ .State.wan }}",
		`{"wan": {{ json .State.wan }}}`,
		"{{ .State }}",
		`{{ index .State "isp" }}`,
		"{{ if .Matching }}up{{ else }}down{{ end }}",
		"{{ range $key, $value := .State }}{{ $key }}={{ $value }} {{ end }}",
		"{{ range $key, $value := .State }}{{ $.State.wan }}{{ end }}",
		"{{ with .State.wan }}{{ . }}{{ else }}{{ .To }}{{ end }}",
		"{{ $uplink := .State }}{{ $uplink.isp }}",
	} {
		if faults := CheckTemplate(text, matchedKeys); len(faults) > 0 {
			t.Errorf("CheckTemplate(%q) = %v, want nothing", text, faults)
		}
	}

	if _, err := Render("{{ range $key, $value := .State }}{{ $.State.wan }}{{ end }}",
		transitionContext()); err != nil {
		// The accepting cases are only worth anything if Render agrees, so the
		// one with the most moving parts is rendered here too.
		t.Fatalf("Render = %v", err)
	}
}

func TestCheckTemplateNamesAnUnmatchedStateKey(t *testing.T) {
	// The trap from #89: isp is observed, is visible in status.observedState,
	// and is not in this Automation's .State because this Automation did not
	// ask for it. The message has to name it and say what to do.
	faults := CheckTemplate("WAN failed over to {{ .State.isp }}", matchedKeys)
	if len(faults) != 1 {
		t.Fatalf("CheckTemplate = %v, want one fault", faults)
	}
	for _, want := range []string{unmatchedKey, "does not match on", "it matches on wan", "add isp to spec.when.state"} {
		if !strings.Contains(string(faults[0]), want) {
			t.Errorf("fault %q does not mention %q", faults[0], want)
		}
	}
}

func TestCheckTemplateCatchesWhatRenderWouldFailOn(t *testing.T) {
	for _, testCase := range []struct {
		text string
		want string
	}{
		{"{{ .State.wam }}", `state key "wam"`},
		{"{{ .Stat.wan }}", "reads .Stat, which is not part of the template context"},
		{"{{ .Password }}", "not part of the template context"},
		{"{{ .State.wan.speed }}", "nothing under it"},
		{"{{ .Key.Length }}", "reads .Key.Length, but .Key is a string"},
		{"{{ .State.isp", "does not parse"},
		{"{{ upper .State.wan }}", "does not parse"},
		{`{{ index .State "wan" }}{{ .State.isp }}`, unmatchedKey},
		{"{{ if .State.isp }}x{{ end }}", unmatchedKey},
		{"{{ range $k, $v := .State }}{{ $.State.isp }}{{ end }}", unmatchedKey},
		{"{{ with .State.isp }}{{ . }}{{ end }}", unmatchedKey},
		{"{{ json .State.isp }}", unmatchedKey},
	} {
		faults := CheckTemplate(testCase.text, matchedKeys)
		if len(faults) == 0 {
			t.Errorf("CheckTemplate(%q) reported nothing, want %q", testCase.text, testCase.want)
			continue
		}
		if !strings.Contains(string(faults[0]), testCase.want) {
			t.Errorf("CheckTemplate(%q) = %q, want it to mention %q", testCase.text, faults[0], testCase.want)
		}
		// Whatever the check reports, Render must actually fail on it —
		// otherwise this reports a working Automation as broken.
		if _, err := Render(testCase.text, transitionContext()); err == nil {
			t.Errorf("Render(%q) succeeded, so the fault %q is wrong", testCase.text, faults[0])
		}
	}
}

func TestCheckTemplateReadsTheElseBranchInTheOuterDot(t *testing.T) {
	// A range rebinds dot for its body and not for its else, which runs when
	// the range had nothing to walk. Render succeeds on this today only because
	// .State is never empty in practice, so the branch is never taken — the
	// reference in it is still one that can never resolve, and saying so costs
	// nothing.
	faults := CheckTemplate("{{ range $k, $v := .State }}{{ else }}{{ .State.isp }}{{ end }}", matchedKeys)
	if len(faults) != 1 || !strings.Contains(string(faults[0]), unmatchedKey) {
		t.Fatalf("CheckTemplate = %v, want the reference in the else branch reported", faults)
	}
}

func TestCheckTemplateReportsEachFaultOnce(t *testing.T) {
	faults := CheckTemplate("{{ .State.isp }} and again {{ .State.isp }}, plus {{ .State.uptime }}", matchedKeys)
	if len(faults) != 2 {
		t.Fatalf("CheckTemplate = %v, want the repeated key reported once and the second key reported", faults)
	}
}

func TestCheckTemplateKnowsEveryContextField(t *testing.T) {
	// The message that lists what a template may read is derived from the
	// Context type, so a field added there needs nothing done here.
	for _, field := range []string{".Automation", ".Namespace", ".Name", ".Provider",
		".Matching", ".Key", ".From", ".To", ".State", ".Time"} {
		if !strings.Contains(contextFields, field) {
			t.Errorf("the context field list %q leaves out %s", contextFields, field)
		}
	}
}
