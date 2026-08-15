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
	"maps"
	"regexp"
	"slices"
	"strings"
)

// reasonCollector walks the reconciler and records what each Event and
// condition reason is raised as, and with what message.
type reasonCollector struct {
	consts map[string]string
	docs   map[string][]string

	events     map[string]*reasonDoc
	conditions map[string]*reasonDoc
}

func newReasonCollector(files []*ast.File) *reasonCollector {
	return &reasonCollector{
		consts:     stringConsts(files),
		docs:       constDocs(files),
		events:     map[string]*reasonDoc{},
		conditions: map[string]*reasonDoc{},
	}
}

// names maps each constant back to the string it holds, for the prose that
// refers to its neighbours by Go identifier.
func (c *reasonCollector) names() map[string]string {
	names := map[string]string{}
	for name, value := range c.consts {
		if strings.HasPrefix(name, "reason") {
			names[name] = value
		}
	}
	return names
}

func (c *reasonCollector) sorted(reasons map[string]*reasonDoc) []reasonDoc {
	out := make([]reasonDoc, 0, len(reasons))
	for _, value := range slices.Sorted(maps.Keys(reasons)) {
		out = append(out, *reasons[value])
	}
	return out
}

func (c *reasonCollector) walkFile(file *ast.File) {
	for _, decl := range file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok || funcDecl.Body == nil {
			continue
		}
		aliases := localAliases(funcDecl)
		ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				c.record(call, aliases)
			}
			return true
		})
	}
}

func (c *reasonCollector) record(call *ast.CallExpr, aliases map[string][]string) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	shape, ok := callShapes[selector.Sel.Name]
	if !ok || len(call.Args) <= shape.reason {
		return
	}

	into := c.events
	if shape.eventType < 0 {
		into = c.conditions
	}
	for _, ref := range c.reasonsAt(call.Args[shape.reason], aliases) {
		reason := into[ref.value]
		if reason == nil {
			reason = &reasonDoc{value: ref.value, constName: ref.constName, doc: c.docs[ref.constName]}
			into[ref.value] = reason
		}
		reason.eventTypes = addTag(reason.eventTypes, enumValue(argAt(call, shape.eventType), "EventType"))
		reason.actions = addTag(reason.actions, c.stringArg(call, shape.action))
		reason.reportedOn = addTag(reason.reportedOn, reportedOn(c.stringArg(call, shape.condition),
			enumValue(argAt(call, shape.status), "Condition")))
		if message := messageText(argAt(call, shape.message)); informative(message) {
			reason.messages = addTag(reason.messages, message)
		}
	}
}

func (c *reasonCollector) stringArg(call *ast.CallExpr, index int) string {
	if arg := argAt(call, index); arg != nil {
		return stringValue(arg, c.consts)
	}
	return ""
}

type reasonRef struct {
	value     string
	constName string
}

// reasonsAt resolves the reason a call raises. It is a constant almost
// everywhere; where it is a local — the one helper that picks between entering
// and leaving a state — every constant assigned to that local in the same
// function counts, which is exactly the pair that helper raises.
func (c *reasonCollector) reasonsAt(expr ast.Expr, aliases map[string][]string) []reasonRef {
	if value, ok := literalString(expr); ok {
		return []reasonRef{{value: value}}
	}
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return nil
	}
	if value, ok := c.consts[ident.Name]; ok {
		return []reasonRef{{value: value, constName: ident.Name}}
	}
	refs := make([]reasonRef, 0, len(aliases[ident.Name]))
	for _, name := range aliases[ident.Name] {
		if value, ok := c.consts[name]; ok {
			refs = append(refs, reasonRef{value: value, constName: name})
		}
	}
	return refs
}

// localAliases records which constants each local variable in a function is
// ever assigned.
func localAliases(funcDecl *ast.FuncDecl) map[string][]string {
	aliases := map[string][]string{}
	ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			name, ok := lhs.(*ast.Ident)
			if !ok || i >= len(assign.Rhs) {
				continue
			}
			if value, ok := assign.Rhs[i].(*ast.Ident); ok {
				aliases[name.Name] = addTag(aliases[name.Name], value.Name)
			}
		}
		return true
	})
	return aliases
}

// enumValue reads a corev1.EventTypeWarning or metav1.ConditionFalse reference
// as the word it stands for.
func enumValue(expr ast.Expr, prefix string) string {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || !strings.HasPrefix(selector.Sel.Name, prefix) {
		return ""
	}
	return strings.TrimPrefix(selector.Sel.Name, prefix)
}

// messageText renders the note a call passes, as its author wrote it: a
// literal, a concatenation of literals, or the format string of the Sprintf
// that fills it in. The verbs are left standing — a reader matching what they
// see in `kubectl describe` against this page is better served by the shape of
// the sentence than by a placeholder this tool invented.
func messageText(expr ast.Expr) string {
	switch node := expr.(type) {
	case *ast.BasicLit, *ast.BinaryExpr, *ast.ParenExpr:
		return collapseSpace(stringValue(node, nil))
	case *ast.CallExpr:
		selector, ok := node.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Sprintf" || len(node.Args) == 0 {
			return ""
		}
		return collapseSpace(stringValue(node.Args[0], nil))
	}
	return ""
}

var spaces = regexp.MustCompile(`\s+`)

func collapseSpace(text string) string {
	return strings.TrimSpace(spaces.ReplaceAllString(text, " "))
}

var formatVerb = regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)

// informative drops a message that is all placeholders. "%s %s: %s" is the
// truth about what one Event says and tells a reader nothing; the sentences
// worth printing are the ones with sentences in them.
func informative(message string) bool {
	return len(collapseSpace(formatVerb.ReplaceAllString(message, ""))) >= 20
}

// reportedOn pairs a condition with the status it is reported at, rather than
// listing the two axes apart: a reason set on Ready=True and on Applied=False
// would otherwise read as if all four combinations happened.
func reportedOn(condition, status string) string {
	if condition == "" || status == "" {
		return ""
	}
	return condition + "=" + status
}

func argAt(call *ast.CallExpr, index int) ast.Expr {
	if index < 0 || index >= len(call.Args) {
		return nil
	}
	return call.Args[index]
}

// addTag appends a value once, keeping the order it was first seen in.
func addTag(values []string, value string) []string {
	if value == "" || slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

// constDocs collects the documentation attached to each string constant.
//
// A constant with no comment of its own inherits one that names it: the
// reasons are declared in pairs under a single note explaining both, and
// dropping the second half of every pair would leave the page saying nothing
// about StateExited, TargetReleased and their like.
func constDocs(files []*ast.File) map[string][]string {
	docs := map[string][]string{}
	var undocumented []string
	for _, file := range files {
		forEachConst(file, func(name *ast.Ident, _ ast.Expr, doc *ast.CommentGroup) {
			if lines := docLines(doc); len(lines) > 0 {
				docs[name.Name] = lines
				return
			}
			undocumented = append(undocumented, name.Name)
		})
	}
	for _, name := range undocumented {
		mention := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
		for _, owner := range slices.Sorted(maps.Keys(docs)) {
			if mention.MatchString(strings.Join(docs[owner], " ")) {
				docs[name] = docs[owner]
				break
			}
		}
	}
	return docs
}
