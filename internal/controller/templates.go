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

package controller

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/actions"
)

// maxFaultsReported bounds how many broken references one condition message
// carries. A message the API server refuses for length would turn a template
// somebody can fix into a status write that never lands, and the first few name
// the mistake well enough that the rest are the same mistake again.
const maxFaultsReported = 5

// templatedField is one template an Automation carries, with the path a reader
// would look for it under in their own YAML.
type templatedField struct {
	path string
	text string
}

// templatesOf lists every template on an Automation that Reactor would render.
//
// It follows buildRequest rather than the presence of a block, so a notification
// block left on an action of some other type is not reported: nothing renders
// it, so nothing about it can fail.
func templatesOf(automation *reactorv1alpha1.Automation) []templatedField {
	var fields []templatedField
	collect := func(block string, list []reactorv1alpha1.Action) {
		for i, action := range list {
			at := fmt.Sprintf("spec.%s[%d]", block, i)
			switch action.Type {
			case actions.TypeHTTPRequest:
				if action.Request != nil {
					fields = append(fields, templatedField{at + ".request.body", action.Request.Body})
				}
			case actions.TypeHomeAssistant:
				if action.HomeAssistant != nil {
					fields = append(fields, templatedField{at + ".homeAssistant.data", action.HomeAssistant.Data})
				}
			case actions.TypeQBittorrentPause, actions.TypeQBittorrentResume:
				// Nothing to template: the action takes no message.
			default:
				if isDesiredState(action.Type) || actions.IsConsole(action.Type) || action.Notification == nil {
					continue
				}
				fields = append(fields,
					templatedField{at + ".notification.title", action.Notification.Title},
					templatedField{at + ".notification.message", action.Notification.Message})
			}
		}
	}
	// Both blocks, because an onExit notification is rendered against the same
	// narrowed context its sibling is and fails in exactly the same way — later,
	// on the recovery nobody was watching for.
	collect("actions", automation.Spec.Actions)
	collect("onExit", automation.Spec.OnExit)
	return fields
}

// templateFaults is every reference in this Automation's templates that could
// never render, each one a sentence naming where it is and what to do.
//
// This is the whole of #89's fix, and the cheap half of the two the issue
// sketches: the keys are in spec.when.state and the references are in the
// templates, so a mismatch is decidable when the Automation is written rather
// than on the transition weeks later that the message was wanted for.
func templateFaults(automation *reactorv1alpha1.Automation) []string {
	if automation.Spec.When == nil {
		return nil
	}
	keys := slices.Sorted(maps.Keys(automation.Spec.When.State))

	var faults []string
	for _, field := range templatesOf(automation) {
		for _, fault := range actions.CheckTemplate(field.text, keys) {
			faults = append(faults, fmt.Sprintf("%s %s", field.path, fault))
		}
	}
	return faults
}

// reportBrokenTemplates sets Ready=False when a template on this Automation can
// never render, and reports whether it did so the caller can leave the Ready
// condition alone.
//
// It outranks every other Ready reason, and the ordering is the point: the
// others describe the world, which may change on its own, and this one
// describes the object, which will not. It is also the only one that can be
// known before a single observation has arrived, which is what moves the
// failure from send time to write time.
//
// What it deliberately does not do is stop the Automation. A broken message is
// the report of a reaction, not the reaction: if the same Automation also
// scales something, the scale is the thing that had to happen, and refusing to
// reconcile targets over a typo in a notification would turn a missed message
// into a missed failover. That is the same rule a failed delivery already
// follows.
func (r *AutomationReconciler) reportBrokenTemplates(
	automation *reactorv1alpha1.Automation,
	faults []string,
) bool {
	if len(faults) == 0 {
		return false
	}
	reported := faults
	if len(reported) > maxFaultsReported {
		reported = append(slices.Clone(faults[:maxFaultsReported]),
			fmt.Sprintf("and %d more like it", len(faults)-maxFaultsReported))
	}
	message := strings.Join(reported, "; ")

	r.eventOnNewReason(automation, conditionReady, corev1.EventTypeWarning,
		reasonTemplateWillNotRender, actionEvaluate,
		"a template on this automation can never render, so the action holding it would fail "+
			"at send time rather than here: %s", message)
	r.setCondition(automation, conditionReady, metav1.ConditionFalse, reasonTemplateWillNotRender, message)
	return true
}
