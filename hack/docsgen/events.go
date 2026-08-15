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
	"strings"
)

// The reasons are read from the call sites rather than from the constant block,
// and that is the whole point of doing it this way. Whether a reason reaches an
// Event or a status condition — and whether it is Normal or Warning — is not
// written anywhere in its declaration; it is decided where it is raised, in
// three different files. Listing the constants would produce a page that looks
// complete and quietly omits the axis operators actually filter on.

// callShape maps one of the reconciler's reporting helpers to the argument
// positions its parts sit in. A signature change moves these, and the drift
// check is what turns that into a failing build rather than a page that has
// silently stopped finding anything — helped by renderEvents refusing to write
// a page with nothing on it.
type callShape struct {
	eventType int
	condition int
	status    int
	reason    int
	action    int
	message   int
}

var callShapes = map[string]callShape{
	// event(automation, eventType, reason, action, note, args...)
	"event": {eventType: 1, reason: 2, action: 3, message: 4, condition: -1, status: -1},
	// eventOnNewReason(automation, conditionType, eventType, reason, action, note, args...)
	"eventOnNewReason": {condition: 1, eventType: 2, reason: 3, action: 4, message: 5, status: -1},
	// setCondition(automation, conditionType, status, reason, message)
	"setCondition": {condition: 1, status: 2, reason: 3, message: 4, eventType: -1, action: -1},
}

type reasonDoc struct {
	value      string
	constName  string
	doc        []string
	eventTypes []string
	actions    []string
	// reportedOn holds "Ready=False" and the like: the condition and the status
	// it is set to, paired as they are written.
	reportedOn []string
	messages   []string
}

func renderEvents(dir string) (page, error) {
	files, err := parseDir(dir)
	if err != nil {
		return page{}, err
	}
	collector := newReasonCollector(files)
	for _, file := range files {
		collector.walkFile(file)
	}

	events := collector.sorted(collector.events)
	conditions := collector.sorted(collector.conditions)
	if len(events) == 0 || len(conditions) == 0 {
		return page{}, fmt.Errorf("%s: found %d Event and %d condition reasons; the reconciler's reporting "+
			"helpers have moved and hack/docsgen/events.go has to follow", dir, len(events), len(conditions))
	}

	// A reason that is both raised as an Event and reported on a condition gets
	// a section in each, so the anchors are handed out in the order the page
	// prints its headings rather than guessed at from the reason's name.
	anchors := newSlugger()
	eventAnchors := reserveAnchors(anchors, "Event reasons", events)
	conditionAnchors := reserveAnchors(anchors, "Condition reasons", conditions)

	var b strings.Builder
	b.WriteString("What `kubectl describe automation` and `kubectl get events` will say, and what each " +
		"`status.conditions[].reason` means. [Events and status](/operations/events/) covers how to read " +
		"them; this is the whole list.\n\n")
	b.WriteString("Warning is reserved for something an operator has to act on. A held state, a deferred " +
		"claim and a reversal are all Normal: they are the design working.\n")

	b.WriteString("\n## Event reasons\n\n")
	b.WriteString("| Reason | Type | Action |\n| --- | --- | --- |\n")
	for _, reason := range events {
		fmt.Fprintf(&b, "| [`%s`](#%s) | %s | %s |\n",
			reason.value, eventAnchors[reason.value], join(reason.eventTypes), code(reason.actions))
	}
	writeReasonSections(&b, events, collector.names(), func(reason reasonDoc) string {
		return fmt.Sprintf("**Type:** %s &nbsp;·&nbsp; **Action:** %s",
			join(reason.eventTypes), code(reason.actions))
	})

	b.WriteString("\n## Condition reasons\n\n")
	b.WriteString("What an Automation reports in `status.conditions[].reason`. `Ready` is whether it is " +
		"valid and reconciling; `Applied` is whether what it wants is what its targets have. The two are " +
		"separate because an Automation can be perfectly healthy and still not be the one deciding a " +
		"target's value.\n\n")
	b.WriteString("| Reason | Reported on |\n| --- | --- |\n")
	for _, reason := range conditions {
		fmt.Fprintf(&b, "| [`%s`](#%s) | %s |\n",
			reason.value, conditionAnchors[reason.value], code(reason.reportedOn))
	}
	writeReasonSections(&b, conditions, collector.names(), func(reason reasonDoc) string {
		return "**Reported on:** " + code(reason.reportedOn)
	})

	return page{
		slug:  "events",
		title: "Events and condition reasons",
		description: "Every Event reason Reactor raises, whether it is Normal or Warning, and every reason " +
			"an Automation reports on its Ready and Applied conditions.",
		source: "internal/controller",
		body:   b.String(),
	}, nil
}

// reserveAnchors takes the anchors one section's headings will be given, in
// the order the page emits them.
func reserveAnchors(anchors *slugger, section string, reasons []reasonDoc) map[string]string {
	anchors.slug(section)
	taken := make(map[string]string, len(reasons))
	for _, reason := range reasons {
		taken[reason.value] = anchors.slug(reason.value)
	}
	return taken
}

func writeReasonSections(b *strings.Builder, reasons []reasonDoc, names map[string]string,
	properties func(reasonDoc) string,
) {
	for _, reason := range reasons {
		fmt.Fprintf(b, "\n### `%s`\n\n%s\n", reason.value, properties(reason))
		if prose := renderComment(dropLeadingName(reason.doc, reason.constName)); prose != "" {
			b.WriteString("\n" + substituteIdents(prose, names) + "\n")
		}
		if len(reason.messages) == 0 {
			continue
		}
		b.WriteString("\nMessage:\n\n")
		for _, message := range reason.messages {
			fmt.Fprintf(b, "- %s\n", escapeCell(message))
		}
	}
}

func join(values []string) string {
	if len(values) == 0 {
		return "—"
	}
	return strings.Join(values, " / ")
}

func code(values []string) string {
	if len(values) == 0 {
		return "—"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, "`"+value+"`")
	}
	return strings.Join(quoted, " / ")
}
