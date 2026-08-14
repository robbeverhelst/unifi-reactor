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
	"errors"
	"fmt"
	"strings"
	"text/template"
)

// maxRendered bounds what one template can produce, so a body cannot be grown
// into a way of pushing bulk data out of the cluster or of exhausting memory.
const maxRendered = 8 << 10

// errRenderTooLong is returned once a template exceeds maxRendered.
var errRenderTooLong = errors.New("rendered content is too long")

// Context is everything a template may read.
//
// The choice of what is in here is a security decision, not a convenience one.
// Templates render into a request body, which goes to an operator-allowed
// destination — so what matters is that nothing here is knowledge the author of
// the Automation does not already hold. Every field is either the Automation's
// own identity or provider state it declared an interest in by naming the key
// in spec.when. There is no way to reach another Automation's state, a Secret,
// the environment, or the filesystem, and no function is registered that could.
//
// Field names are provider-agnostic on purpose: a notification action has no
// idea what a key called wan means, and must not.
type Context struct {
	// Automation is "namespace/name".
	Automation string
	Namespace  string
	Name       string
	// Provider is the state provider the condition watches, e.g. "unifi".
	Provider string
	// Matching is true when this fired on entering the condition.
	Matching bool
	// Key, From and To describe the transition that flipped Matching. Key is
	// empty when no single key can be named.
	Key  string
	From string
	To   string
	// State is the observed value of every key this Automation watches.
	State map[string]string
	// Time is when the transition was recorded, RFC 3339.
	Time string
}

// templateFuncs is the entire function surface a template gets. json is here
// because the alternative — telling people to hand-quote a state value into a
// JSON body — breaks the moment a value contains a quote, and a broken body is
// how an injection starts.
var templateFuncs = template.FuncMap{
	"json": func(value any) (string, error) {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", fmt.Errorf("json: %w", err)
		}
		return string(encoded), nil
	},
}

// Render evaluates one template against a transition.
//
// missingkey=error is set so that a typo in a state key fails loudly at the
// moment the notification would have been sent, rather than silently
// delivering the word "no value" to whoever was meant to be told something.
func Render(text string, data Context) (string, error) {
	if text == "" {
		return "", nil
	}
	parsed, err := template.New("action").Option("missingkey=error").Funcs(templateFuncs).Parse(text)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}

	out := &boundedBuilder{limit: maxRendered}
	if err := parsed.Execute(out, data); err != nil {
		if errors.Is(err, errRenderTooLong) {
			return "", fmt.Errorf("template rendered more than %d bytes", maxRendered)
		}
		return "", fmt.Errorf("rendering template: %w", err)
	}
	return out.builder.String(), nil
}

// boundedBuilder stops a template mid-execution rather than after the fact, so
// a runaway range never allocates the output it would have produced.
type boundedBuilder struct {
	builder strings.Builder
	limit   int
}

func (b *boundedBuilder) Write(p []byte) (int, error) {
	if b.builder.Len()+len(p) > b.limit {
		return 0, errRenderTooLong
	}
	return b.builder.Write(p)
}
