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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// parseDir reads one directory's non-test Go files, comments included, in path
// order so that regenerating produces the same page twice.
//
// It deliberately does not type-check. Everything these pages need is written
// literally in the syntax — a metric's name, an Event reason's value, a label
// set — and a type-checked load would want the whole dependency graph built to
// arrive at the same strings.
func parseDir(dir string) ([]*ast.File, error) {
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, entry := range names {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, nil
}

// stringConsts collects every string constant in a set of files, so that a
// label name or an Event reason written once as a constant and referenced
// elsewhere can be resolved back to its value.
func stringConsts(files []*ast.File) map[string]string {
	consts := map[string]string{}
	for _, file := range files {
		forEachConst(file, func(name *ast.Ident, value ast.Expr, _ *ast.CommentGroup) {
			if literal, ok := literalString(value); ok {
				consts[name.Name] = literal
			}
		})
	}
	return consts
}

// forEachConst visits every named constant with a value, handing over the
// documentation attached to it — which is the spec's own comment where it has
// one, and the block's where the constants share a note.
func forEachConst(file *ast.File, visit func(name *ast.Ident, value ast.Expr, doc *ast.CommentGroup)) {
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.CONST {
			continue
		}
		for _, spec := range genDecl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok || len(valueSpec.Names) != len(valueSpec.Values) {
				continue
			}
			for i, name := range valueSpec.Names {
				visit(name, valueSpec.Values[i], valueSpec.Doc)
			}
		}
	}
}

// stringValue resolves a string expression: a literal, a constant reference, or
// the concatenation of either, which is how the longer help texts are written.
func stringValue(expr ast.Expr, consts map[string]string) string {
	switch node := expr.(type) {
	case *ast.BasicLit:
		if value, ok := literalString(node); ok {
			return value
		}
	case *ast.Ident:
		return consts[node.Name]
	case *ast.BinaryExpr:
		if node.Op == token.ADD {
			return stringValue(node.X, consts) + stringValue(node.Y, consts)
		}
	case *ast.ParenExpr:
		return stringValue(node.X, consts)
	}
	return ""
}

// stringSlice reads a []string{...} literal, resolving constant references.
func stringSlice(expr ast.Expr, consts map[string]string) []string {
	composite, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(composite.Elts))
	for _, elt := range composite.Elts {
		if value := stringValue(elt, consts); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func literalString(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return value, true
}

// docLines returns a doc comment as Markdown-ready lines: comment markers
// stripped, the author's own indentation kept.
func docLines(group *ast.CommentGroup) []string {
	if group == nil {
		return nil
	}
	return trimBlankEdges(strings.Split(strings.TrimRight(group.Text(), "\n"), "\n"))
}

// dropLeadingName removes the "Name is ..." opening a Go doc comment has to
// start with. The identifier it repeats is unexported and is not what the page
// calls the thing, so leaving it in reads as a mistake.
func dropLeadingName(lines []string, name string) []string {
	if len(lines) == 0 {
		return lines
	}
	for _, verb := range []string{" is ", " are ", " holds ", " counts ", " reports "} {
		prefix := name + verb
		if !strings.HasPrefix(lines[0], prefix) {
			continue
		}
		rest := strings.TrimPrefix(lines[0], prefix)
		if rest == "" {
			continue
		}
		out := slices.Clone(lines)
		out[0] = strings.ToUpper(rest[:1]) + rest[1:]
		return out
	}
	return lines
}

// substituteIdents rewrites the Go identifiers a doc comment uses to refer to
// its neighbours into the names the page calls them by — a metric's series
// name, an Event's reason string. Without it the prose that ties these pages
// together ("the other half of lastObservation") names something the reader
// cannot find anywhere.
//
// Only camelCased identifiers are rewritten. A metric named `transitions` is
// also an ordinary English word in the sentences around it, and there is no
// way to tell the two apart from the syntax; leaving those alone costs a link
// and misreading one costs a sentence.
func substituteIdents(text string, names map[string]string) string {
	idents := make([]string, 0, len(names))
	for name := range names {
		if strings.ToLower(name) != name {
			idents = append(idents, name)
		}
	}
	if len(idents) == 0 {
		return text
	}
	slices.Sort(idents)
	slices.SortFunc(idents, func(a, b string) int { return len(b) - len(a) })
	pattern := regexp.MustCompile(`\b(` + strings.Join(idents, "|") + `)\b`)
	return pattern.ReplaceAllStringFunc(text, func(match string) string {
		return "`" + names[match] + "`"
	})
}
