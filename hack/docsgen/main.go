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

// docsgen renders the Reference section of the documentation site from the
// places that already hold the reference: the API's Go doc comments and
// kubebuilder markers, the chart's values.yaml, the metric definitions, and
// the controller's Event and condition reasons.
//
// Nothing under docs/src/content/docs/reference is edited by hand. `make docs`
// regenerates it and CI fails when the result differs from what was committed,
// so a field added in a hurry cannot leave the page describing it behind.
//
//	go run ./hack/docsgen -root . -crd-markdown <file crd-ref-docs wrote>
//
// The Automation page is a rewrite of what crd-ref-docs produced rather than a
// second implementation of it: hack/gen-reference.sh runs that tool and hands
// the result here to be given front matter and heading levels the site can use.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	root := flag.String("root", ".", "repository root")
	crdMarkdown := flag.String("crd-markdown", "", "Markdown file crd-ref-docs wrote for the API types")
	flag.Parse()

	if err := run(*root, *crdMarkdown); err != nil {
		fmt.Fprintln(os.Stderr, "docsgen:", err)
		os.Exit(1)
	}
}

func run(root, crdMarkdown string) error {
	if crdMarkdown == "" {
		return fmt.Errorf("-crd-markdown is required; run hack/gen-reference.sh rather than this tool directly")
	}

	automation, err := renderAutomation(crdMarkdown)
	if err != nil {
		return fmt.Errorf("automation reference: %w", err)
	}
	values, err := renderValues(filepath.Join(root, "charts", "reactor", "values.yaml"))
	if err != nil {
		return fmt.Errorf("values reference: %w", err)
	}
	metrics, err := renderMetrics(filepath.Join(root, "internal", "metrics"))
	if err != nil {
		return fmt.Errorf("metrics reference: %w", err)
	}
	events, err := renderEvents(filepath.Join(root, "internal", "controller"))
	if err != nil {
		return fmt.Errorf("events reference: %w", err)
	}
	pages := []page{automation, values, metrics, events}

	dir := filepath.Join(root, "docs", "src", "content", "docs", "reference")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, rendered := range pages {
		if err := rendered.write(dir); err != nil {
			return err
		}
	}
	return nil
}

// page is one file under the site's Reference section.
type page struct {
	slug        string
	title       string
	description string
	// source names the file this page was generated from, in the banner, so
	// that somebody who lands here from a search knows where to make a change.
	source string
	body   string
}

// write emits the page with Starlight front matter.
//
// editUrl is false on every one of them: the edit link would otherwise point at
// a generated file, and an edit accepted there would be reverted by the next
// `make docs` without anybody noticing.
func (p page) write(dir string) error {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %q\n", p.title)
	fmt.Fprintf(&b, "description: %q\n", p.description)
	b.WriteString("editUrl: false\n")
	b.WriteString("tableOfContents:\n  maxHeadingLevel: 2\n")
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, ":::note[Generated from source]\nThis page is generated from `%s` by `make docs`. "+
		"Change it there — CI fails when this file and its source disagree.\n:::\n\n", p.source)
	b.WriteString(strings.TrimRight(p.body, "\n"))
	b.WriteString("\n")

	return os.WriteFile(filepath.Join(dir, p.slug+".md"), []byte(b.String()), 0o644)
}
