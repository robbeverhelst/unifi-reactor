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

package main

import (
	"fmt"
	"os"
	"strings"
)

// crd-ref-docs already reads every doc comment and every kubebuilder marker in
// api/v1alpha1 and writes them out as Markdown. What it does not know is that
// this page lives inside a site that supplies its own title and renders a table
// of contents from h2, so the only work left here is levelling the headings and
// dropping the standalone document's own front page.

func renderAutomation(path string) (page, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return page{}, err
	}
	body, err := levelHeadings(strings.Split(string(data), "\n"))
	if err != nil {
		return page{}, fmt.Errorf("%s: %w", path, err)
	}

	intro := "Every field of the `Automation` custom resource: its type, whether it is required, the values " +
		"it accepts and what it defaults to. Generated from the Go types the CRD itself is generated from, " +
		"so the two cannot disagree.\n\n" +
		"[Your first Automation](/start/first-automation/) is the shortest way in; " +
		"[Actions](/actions/kubernetes/) explains what each action type does.\n\n"

	return page{
		slug:  "automation",
		title: "Automation API",
		description: "Every field of the Automation custom resource: type, required, enum values and " +
			"defaults, generated from the API types.",
		source: "api/v1alpha1",
		body:   intro + body,
	}, nil
}

// levelHeadings drops everything up to and including the package heading — the
// document title, the package index and the package's own doc comment are all
// said better by the page around it — and lifts what remains so each type is an
// h2. Anchors are derived from heading text, so the links crd-ref-docs wrote
// between types keep working.
func levelHeadings(lines []string) (string, error) {
	start := -1
	for i, line := range lines {
		if !strings.HasPrefix(line, "## ") || line == "## Packages" {
			continue
		}
		if start >= 0 {
			return "", fmt.Errorf("expected one API group, found a second at %q", line)
		}
		start = i
	}
	if start < 0 {
		return "", fmt.Errorf("no API group heading found; crd-ref-docs output has changed shape")
	}
	for start < len(lines) && !strings.HasPrefix(lines[start], "### ") {
		start++
	}
	if start == len(lines) {
		return "", fmt.Errorf("no type sections found; crd-ref-docs output has changed shape")
	}

	var out []string
	blank := 0
	for _, line := range lines[start:] {
		switch {
		case strings.HasPrefix(line, "#### "):
			line = "## " + strings.TrimPrefix(line, "#### ")
		case strings.HasPrefix(line, "### "):
			line = "## " + strings.TrimPrefix(line, "### ")
		default:
			line = escapeExceptBreaks(line)
		}
		// crd-ref-docs leaves runs of blank lines where a type has no
		// description; they are invisible in the render and noisy in the diff
		// the drift check prints.
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.Join(trimBlankEdges(out), "\n") + "\n", nil
}

// lineBreak is the one piece of HTML crd-ref-docs emits itself, to keep a
// multi-line doc comment readable inside a table cell.
const lineBreak = "<br />"

// escapeExceptBreaks makes the angle brackets in the doc comments literal
// without disturbing that.
//
// A field documented as `"Bearer <token>"` otherwise loses the placeholder
// entirely: Markdown passes raw HTML through, and <token> is a well-formed tag
// name as far as the parser is concerned.
func escapeExceptBreaks(line string) string {
	const sentinel = "\x00"
	escaped := escapeAngles(strings.ReplaceAll(line, lineBreak, sentinel))
	return strings.ReplaceAll(escaped, sentinel, lineBreak)
}
