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
	"fmt"
	"reflect"
	"slices"
	"strings"
	"text/template"
	"text/template/parse"
)

// TemplateFault is one sentence naming something in a template that Render can
// never satisfy, whatever the provider observes.
//
// It is phrased to follow the field path it was found in, so a caller writes
// "spec.actions[0].notification.message" + " " + fault and gets a sentence.
type TemplateFault string

// CheckTemplate reports everything in text that Render could never produce for
// an Automation whose spec.when.state names stateKeys.
//
// This exists because the narrowing of Context.State is invisible from the
// template. .State is the observed value of the keys this Automation matched
// on and nothing else, missingkey=error turns anything else into a failed
// render, and a failed render happens at send time — on a transition that may
// be weeks away and is exactly the moment the message was wanted. Both halves
// of the question are static: the keys are in the spec and the references are
// in the template, so the answer is available when the Automation is written.
//
// It is deliberately one-sided. Everything it reports is something Render
// cannot do; anything it cannot decide it stays quiet about, because a false
// accusation here reports a working Automation as broken. Three things are
// therefore let through:
//
//   - the body of a range or with block, where dot is rebound to something this
//     package cannot know. $ still refers to the Context, so $.State.isp inside
//     a range over .State is checked.
//   - a chain rooted anywhere but dot or $ — {{ $x.State.isp }} after an
//     assignment, or (pipeline).Field.
//   - {{ index .State "isp" }}, which returns the empty string rather than
//     failing, and so is not the trap this is here to catch.
func CheckTemplate(text string, stateKeys []string) []TemplateFault {
	if text == "" {
		return nil
	}
	// Parsed exactly as Render parses it, funcs included, so an unknown
	// function is caught here for the same reason a syntax error is: both make
	// the send fail rather than the write.
	parsed, err := template.New("action").Option("missingkey=error").Funcs(templateFuncs).Parse(text)
	if err != nil {
		return []TemplateFault{TemplateFault(fmt.Sprintf("does not parse: %s", parseErrorText(err)))}
	}

	if parsed.Tree == nil || parsed.Root == nil {
		return nil
	}
	walker := &templateWalker{stateKeys: stateKeys, seen: map[TemplateFault]bool{}}
	walker.walk(parsed.Root, true)
	return walker.faults
}

// parseErrorText drops the name Render gives the template, which is an internal
// detail and reads as noise in a status message. What is left is the line
// number and the complaint.
func parseErrorText(err error) string {
	return strings.TrimPrefix(err.Error(), "template: action:")
}

// templateWalker collects faults while descending a parsed template.
type templateWalker struct {
	stateKeys []string
	faults    []TemplateFault
	// seen keeps a template that reads the same missing key three times from
	// saying so three times.
	seen map[TemplateFault]bool
}

// walk descends one node. dotIsContext says whether the current dot is still
// the Context that Render passes to Execute; inside a range or with body it is
// not, and field references there cannot be judged.
func (w *templateWalker) walk(node parse.Node, dotIsContext bool) {
	switch typed := node.(type) {
	case *parse.ListNode:
		if typed == nil {
			return
		}
		for _, child := range typed.Nodes {
			w.walk(child, dotIsContext)
		}
	case *parse.ActionNode:
		w.walk(typed.Pipe, dotIsContext)
	case *parse.PipeNode:
		if typed == nil {
			return
		}
		for _, command := range typed.Cmds {
			w.walk(command, dotIsContext)
		}
	case *parse.CommandNode:
		for _, arg := range typed.Args {
			w.walk(arg, dotIsContext)
		}
	case *parse.FieldNode:
		if dotIsContext {
			w.check("."+strings.Join(typed.Ident, "."), typed.Ident)
		}
	case *parse.ChainNode:
		// (pipeline).Field. Only the pipeline is judged: what it evaluates to
		// is a question for the type checker text/template does not have.
		w.walk(typed.Node, dotIsContext)
	case *parse.VariableNode:
		// $ is whatever Execute was handed, which is the Context wherever the
		// reference appears. Any other variable was assigned something this
		// package cannot follow.
		if typed.Ident[0] == "$" && len(typed.Ident) > 1 {
			w.check("$."+strings.Join(typed.Ident[1:], "."), typed.Ident[1:])
		}
	case *parse.IfNode:
		// if does not rebind dot; range and with do, and their else branch runs
		// with the outer one either way.
		w.branch(&typed.BranchNode, dotIsContext, dotIsContext)
	case *parse.RangeNode:
		w.branch(&typed.BranchNode, dotIsContext, false)
	case *parse.WithNode:
		w.branch(&typed.BranchNode, dotIsContext, false)
	case *parse.TemplateNode:
		w.walk(typed.Pipe, dotIsContext)
	}
}

func (w *templateWalker) branch(branch *parse.BranchNode, outerDot, bodyDot bool) {
	w.walk(branch.Pipe, outerDot)
	w.walk(branch.List, bodyDot)
	w.walk(branch.ElseList, outerDot)
}

// check follows one .Foo.Bar chain down the Context type and reports the first
// step that cannot be taken.
//
// The type is read by reflection rather than from a list kept alongside it, so
// a field added to Context is understood here without anybody remembering to
// come back.
func (w *templateWalker) check(reference string, idents []string) {
	current := reflect.TypeFor[Context]()
	for i, ident := range idents {
		reached := reference[:strings.Index(reference, ".")] + "." + strings.Join(idents[:i+1], ".")
		switch current.Kind() {
		case reflect.Struct:
			field, ok := current.FieldByName(ident)
			if !ok {
				w.add(fmt.Sprintf("reads %s, which is not part of the template context; it offers %s",
					reached, contextFields))
				return
			}
			current = field.Type
		case reflect.Map:
			// Context has one map and it is State, whose keys are exactly the
			// keys in spec.when.state. This is the trap the whole file exists
			// for, so the message names the key and both ways out of it.
			if !slices.Contains(w.stateKeys, ident) {
				w.add(fmt.Sprintf(
					"references state key %q, which this automation does not match on (it matches on %s); "+
						"add %s to spec.when.state or remove the reference",
					ident, strings.Join(w.stateKeys, ", "), ident))
				return
			}
			current = current.Elem()
		default:
			w.add(fmt.Sprintf("reads %s, but %s is a %s and has nothing under it",
				reference, reached[:strings.LastIndex(reached, ".")], current.Kind()))
			return
		}
	}
}

func (w *templateWalker) add(detail string) {
	fault := TemplateFault(detail)
	if w.seen[fault] {
		return
	}
	w.seen[fault] = true
	w.faults = append(w.faults, fault)
}

// contextFields is what a template may read, listed for the message that says a
// reference is not one of them. Derived from the type for the same reason the
// walk is.
var contextFields = describeContextFields()

func describeContextFields() string {
	contextType := reflect.TypeFor[Context]()
	names := make([]string, 0, contextType.NumField())
	for field := range contextType.Fields() {
		names = append(names, "."+field.Name)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}
