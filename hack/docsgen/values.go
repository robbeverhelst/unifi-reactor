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
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// The chart's values are documented as prose above each key, in the file people
// actually read while configuring. helm-docs would want every one of those
// blocks rewritten into its "# --" form, which buys this page nothing and costs
// values.yaml the thing that makes it worth reading. So the comment blocks are
// read as they are: yaml.v3 hands each key the comment written above it, and
// everything below is about turning that into a page.

// safeKey matches a key that can be written into a dotted Helm path without
// ambiguity. A key holding a dot or a wildcard — unifi.debounce.keys has
// "ups.battery" and "device.*" — cannot, so its parent is rendered whole
// instead of being walked into and misreported as `keys.ups.battery`.
var safeKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

type valueEntry struct {
	path  string
	depth int
	doc   string
	// value is nil for a group: a mapping this walk descended into, whose
	// children carry the documentation.
	value *yaml.Node
}

func renderValues(path string) (page, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return page{}, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return page{}, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return page{}, fmt.Errorf("%s: expected a single mapping document", path)
	}

	var entries []valueEntry
	walkValues("", 1, doc.Content[0], &entries)
	if len(entries) == 0 {
		return page{}, fmt.Errorf("%s: no values found", path)
	}

	var b strings.Builder
	b.WriteString("Every value the chart takes, with the default it ships and the note written above it in " +
		"`values.yaml`. A key with no note is one whose name says all there is to say.\n\n")
	b.WriteString("Values are grouped as they are nested. A mapping nobody has written a note about — and a " +
		"mapping whose own keys come from your network rather than from the chart, like " +
		"`unifi.debounce.keys` — is shown whole, as the YAML it defaults to.\n")

	for _, entry := range entries {
		fmt.Fprintf(&b, "\n%s `%s`\n", strings.Repeat("#", headingLevel(entry.depth)), entry.path)
		if entry.value != nil {
			b.WriteString("\n" + renderDefault(entry.value) + "\n")
		}
		if entry.doc != "" {
			b.WriteString("\n" + entry.doc + "\n")
		}
	}

	return page{
		slug:  "values",
		title: "Chart values",
		description: "Every value the Reactor Helm chart takes, its default, and the note written above it " +
			"in values.yaml.",
		source: "charts/reactor/values.yaml",
		body:   b.String(),
	}, nil
}

// headingLevel keeps the top-level groups at h2 — the only level in the page's
// table of contents — and everything below them out of it.
func headingLevel(depth int) int {
	return min(depth+1, 4)
}

func walkValues(prefix string, depth int, node *yaml.Node, out *[]valueEntry) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		path := key.Value
		if prefix != "" {
			path = prefix + "." + key.Value
		}
		entry := valueEntry{
			path:  path,
			depth: depth,
			doc:   renderComment(commentBody(key.HeadComment, "#")),
			value: value,
		}
		if descend(value) {
			entry.value = nil
			*out = append(*out, entry)
			walkValues(path, depth+1, value, out)
			continue
		}
		*out = append(*out, entry)
	}
}

// descend reports whether a mapping is a group of documented settings rather
// than a value in its own right. It is both halves or neither: keys that can be
// addressed individually, and something written down about at least one of them.
func descend(node *yaml.Node) bool {
	if node.Kind != yaml.MappingNode || len(node.Content) == 0 {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if !safeKey.MatchString(node.Content[i].Value) {
			return false
		}
	}
	return documented(node)
}

func documented(node *yaml.Node) bool {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if strings.TrimSpace(node.Content[i].HeadComment) != "" {
			return true
		}
		if child := node.Content[i+1]; child.Kind == yaml.MappingNode && documented(child) {
			return true
		}
	}
	return false
}

// renderDefault writes what the chart ships for one value: inline for anything
// that fits on a line, and as YAML for anything that does not.
func renderDefault(node *yaml.Node) string {
	if node.Kind == yaml.ScalarNode {
		if node.Value == "" {
			return "Default: `\"\"`"
		}
		return "Default: `" + node.Value + "`"
	}
	if len(node.Content) == 0 {
		if node.Kind == yaml.SequenceNode {
			return "Default: `[]`"
		}
		return "Default: `{}`"
	}

	// The comments inside belong to the keys above, which this page has already
	// printed; carrying them into the block would print them twice.
	stripComments(node)
	encoded, err := yaml.Marshal(node)
	if err != nil {
		return "Default: `" + node.Value + "`"
	}
	return "Default:\n\n```yaml\n" + strings.TrimRight(string(encoded), "\n") + "\n```"
}

func stripComments(node *yaml.Node) {
	node.HeadComment, node.LineComment, node.FootComment = "", "", ""
	for _, child := range node.Content {
		stripComments(child)
	}
}
