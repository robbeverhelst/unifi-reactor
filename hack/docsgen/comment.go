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
	"regexp"
	"strings"
)

// slugger reproduces the anchors the site gives headings, so that a link
// written on the same page lands on the right one. It mirrors github-slugger,
// which is what rehype-slug — and therefore Starlight — uses: lower-case, drop
// everything that is not a word character or a space, join words with hyphens,
// and number a repeat rather than let it collide.
type slugger struct {
	seen map[string]int
}

var notSlug = regexp.MustCompile(`[^\p{L}\p{N}\- _]+`)

func newSlugger() *slugger {
	return &slugger{seen: map[string]int{}}
}

func (s *slugger) slug(text string) string {
	base := strings.ToLower(notSlug.ReplaceAllString(text, ""))
	base = strings.ReplaceAll(strings.TrimSpace(base), " ", "-")
	count := s.seen[base]
	s.seen[base] = count + 1
	if count == 0 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, count)
}

// commentBody strips the comment markers from a block and returns its lines
// with the leading marker and one following space removed, so that whatever
// indentation the author used inside the comment survives.
func commentBody(raw, marker string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		trimmed := strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(trimmed, marker) {
			// A line inside a comment block that carries no marker is not
			// something either source produces; keeping it verbatim beats
			// dropping text nobody would then know was missing.
			out = append(out, line)
			continue
		}
		trimmed = strings.TrimPrefix(trimmed, marker)
		out = append(out, strings.TrimPrefix(trimmed, " "))
	}
	return trimBlankEdges(out)
}

// renderComment turns a stripped comment block into Markdown.
//
// The two sources this reads from — values.yaml and Go doc comments — wrap
// prose to a column and use the same two conventions for everything that is not
// prose: an indented run beginning with "- " is a list, and any other indented
// run is an example. Rendering those as what they are is the whole difference
// between a reference page and a wall of run-on text, because Markdown joins
// wrapped lines and would otherwise fold an example into the sentence above it.
func renderComment(lines []string) string {
	var out []string
	for i := 0; i < len(lines); {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			out = append(out, "")
			i++
			continue
		}
		if indentOf(line) >= 2 {
			run := trimBlankEdges(indentedRun(lines[i:]))
			// Both a fence and a list want a blank line between them and the
			// sentence that introduced them.
			if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
				out = append(out, "")
			}
			out = append(out, renderIndentedRun(run)...)
			i += len(run)
			continue
		}
		// A godoc section heading. The page already owns h2, so these sit under
		// whatever heading introduced the comment.
		if strings.HasPrefix(line, "# ") {
			out = append(out, "##"+line)
			i++
			continue
		}
		out = append(out, escapeAngles(line))
		i++
	}
	return strings.Join(trimBlankEdges(out), "\n")
}

// indentedRun returns the leading lines of a block that belong to one indented
// run: every line indented by at least two columns, plus the blank lines
// between them.
func indentedRun(lines []string) []string {
	for i, line := range lines {
		if strings.TrimSpace(line) == "" || indentOf(line) >= 2 {
			continue
		}
		return lines[:i]
	}
	return lines
}

func renderIndentedRun(run []string) []string {
	if len(run) == 0 {
		return nil
	}
	common := commonIndent(run)
	dedented := make([]string, 0, len(run)+2)
	for _, line := range run {
		dedented = append(dedented, strings.TrimPrefix(line, strings.Repeat(" ", common)))
	}
	if strings.HasPrefix(dedented[0], "- ") {
		for i, line := range dedented {
			dedented[i] = escapeAngles(line)
		}
		return dedented
	}
	// An example: a YAML fragment, a kubectl invocation, a query. Fenced
	// without a language because the two sources mix all three, and a fence is
	// also what keeps the placeholder angle brackets in them literal.
	return append(append([]string{"```"}, dedented...), "```")
}

// escapeAngles makes a "<" outside inline code literal.
//
// Markdown passes raw HTML through, so device.<name> and <switch MAC> — which
// both appear in the prose this renders — would otherwise be parsed as unknown
// elements and vanish from the page entirely. Inline code is left alone: it is
// already literal there, and an entity written inside backticks would show up
// as the entity.
func escapeAngles(line string) string {
	var b strings.Builder
	inCode := false
	for _, r := range line {
		switch {
		case r == '`':
			inCode = !inCode
			b.WriteRune(r)
		case r == '<' && !inCode:
			b.WriteString("&lt;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeCell makes text safe to put in a Markdown table cell.
func escapeCell(text string) string {
	return strings.ReplaceAll(escapeAngles(text), "|", `\|`)
}

func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

func commonIndent(lines []string) int {
	common := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if indent := indentOf(line); common < 0 || indent < common {
			common = indent
		}
	}
	if common < 0 {
		return 0
	}
	return common
}

func trimBlankEdges(lines []string) []string {
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[start:end]
}
