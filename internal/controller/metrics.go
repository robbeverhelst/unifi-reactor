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
	"context"

	"k8s.io/apimachinery/pkg/api/meta"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/metrics"
)

// updateStatus writes an Automation's status and publishes the same facts to
// the metrics registry.
//
// The two travel together deliberately. Reactor's status block and its metrics
// answer the same question from opposite ends — one per resource, one across
// the fleet — and the cheapest way to keep them from ever disagreeing is to
// give them one call site rather than two. Every path that reports a decision
// goes through here.
func (r *AutomationReconciler) updateStatus(ctx context.Context, automation *reactorv1alpha1.Automation) error {
	metrics.AutomationEvaluated(automation.Namespace, automation.Name,
		automation.Status.Matching,
		meta.IsStatusConditionTrue(automation.Status.Conditions, conditionReady))
	return r.Status().Update(ctx, automation)
}
