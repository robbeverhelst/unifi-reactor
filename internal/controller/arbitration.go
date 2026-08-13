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
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/engine"
)

const (
	// annotationBaselineReplicas records what a target was set to before
	// Reactor first claimed it. It lives on the target rather than in an
	// Automation's status because it has to outlive both the Automation and
	// Reactor itself: it is the only thing that can answer "what was this
	// before?" after the operator is uninstalled.
	annotationBaselineReplicas = "reactor.robbeverhelst.com/baseline-replicas"
	// annotationClaimedBy names the Automations currently holding the target.
	// Advisory: refreshed on every claim, never read back as truth. It exists
	// so that describing a Deployment explains why it is scaled to zero.
	annotationClaimedBy = "reactor.robbeverhelst.com/claimed-by"
	// annotationClaimedAt records when the current claim began.
	annotationClaimedAt = "reactor.robbeverhelst.com/claimed-at"
)

// targetKey identifies a target workload across every Automation referencing
// it. Arbitration is per target, so this is the unit claims are grouped by.
type targetKey struct {
	Kind      string
	Namespace string
	Name      string
}

func (k targetKey) String() string {
	return k.Kind + "/" + k.Namespace + "/" + k.Name
}

// targetKeyFor resolves an action's target, defaulting the namespace to the
// Automation's own — the same defaulting the API documents.
func targetKeyFor(automation *reactorv1alpha1.Automation, action reactorv1alpha1.Action) (targetKey, bool) {
	if action.Target == nil {
		return targetKey{}, false
	}
	namespace := action.Target.Namespace
	if namespace == "" {
		namespace = automation.Namespace
	}
	return targetKey{Kind: action.Target.Kind, Namespace: namespace, Name: action.Target.Name}, true
}

// claimantOf is the identity an Automation claims targets under, and the one
// reported in status and in the claimed-by annotation.
func claimantOf(automation *reactorv1alpha1.Automation) string {
	return automation.Namespace + "/" + automation.Name
}

// targetsOf lists every target an Automation references, deduplicated and in a
// stable order so repeated reconciles produce identical status.
func targetsOf(automation *reactorv1alpha1.Automation) []targetKey {
	var keys []targetKey
	for _, actions := range [][]reactorv1alpha1.Action{automation.Spec.Actions, automation.Spec.OnExit} {
		for _, action := range actions {
			key, ok := targetKeyFor(automation, action)
			if !ok || !isDesiredState(action.Type) || slices.Contains(keys, key) {
				continue
			}
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b targetKey) int { return strings.Compare(a.String(), b.String()) })
	return keys
}

// references reports whether an Automation has anything to say about a target,
// in either direction. An Automation that only mentions it in onExit still
// participates: it has an opinion about what should happen once nothing claims
// the target.
func references(automation *reactorv1alpha1.Automation, key targetKey) bool {
	return slices.Contains(targetsOf(automation), key)
}

// levelFor returns the level a set of actions asks for on one target.
func levelFor(automation *reactorv1alpha1.Automation, actions []reactorv1alpha1.Action, key targetKey) (int64, bool) {
	for _, action := range actions {
		if !isDesiredState(action.Type) || action.Replicas == nil {
			continue
		}
		if k, ok := targetKeyFor(automation, action); !ok || k != key {
			continue
		}
		return int64(*action.Replicas), true
	}
	return 0, false
}

// claimFor returns what one Automation asks of a target while its condition
// holds. An Automation whose condition does not hold makes no claim at all —
// that is what lets another Automation's claim keep the target down without
// anything having to suppress this one's reversal.
func claimFor(automation *reactorv1alpha1.Automation, key targetKey, matching bool) (engine.Intent, bool) {
	if !matching {
		return engine.Intent{}, false
	}
	level, ok := levelFor(automation, automation.Spec.Actions, key)
	if !ok {
		return engine.Intent{}, false
	}
	return engine.Intent{Claimant: claimantOf(automation), Level: level}, true
}

// reversalFor returns what one Automation wants for a target once nothing
// claims it any more. Unlike a claim this is consulted only at release time,
// so a reversal never competes with a live claim — it cannot raise a workload
// another Automation is still holding down.
func reversalFor(
	automation *reactorv1alpha1.Automation,
	key targetKey,
	baseline *int64,
) (engine.Intent, bool) {
	claimant := claimantOf(automation)
	switch automation.Spec.EffectiveReversal() {
	case reactorv1alpha1.ReversalNone:
		return engine.Intent{}, false
	case reactorv1alpha1.ReversalBaseline:
		if baseline == nil {
			return engine.Intent{}, false
		}
		return engine.Intent{Claimant: claimant, Level: *baseline}, true
	default:
		level, ok := levelFor(automation, automation.Spec.OnExit, key)
		if !ok {
			return engine.Intent{}, false
		}
		return engine.Intent{Claimant: claimant, Level: level}, true
	}
}

// baselineOf reads the recorded pre-claim value off a target. recorded reports
// whether Reactor has ever claimed this target, which is what distinguishes
// "release it back to where it was" from "never touched it, leave it alone".
// A value that cannot be parsed still counts as recorded: the target is
// released, it simply cannot contribute a baseline reversal.
func baselineOf(deployment *appsv1.Deployment) (level *int64, recorded bool) {
	raw, ok := deployment.Annotations[annotationBaselineReplicas]
	if !ok {
		return nil, false
	}
	parsed, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return nil, true
	}
	return &parsed, true
}

// targetOutcome is one target's arbitration, from the point of view of the
// Automation being reconciled.
type targetOutcome struct {
	ref string
	// desired is what this Automation alone wants right now.
	desired *int32
	// effective is what arbitration resolved across every claimant, or nil
	// while nothing claims the target.
	effective *int32
	// deferredBy names the claimants holding the target away from desired.
	deferredBy []string
	// changed reports whether this reconcile actually wrote to the target.
	changed bool
}

// reconcileTarget resolves one target across every Automation referencing it
// and writes the outcome. It is called on every reconcile, not only on
// transitions: another Automation entering or leaving its state changes what
// this target should be without anything about this one changing.
func (r *AutomationReconciler) reconcileTarget(
	ctx context.Context,
	key targetKey,
	self *reactorv1alpha1.Automation,
	selfMatching bool,
) (targetOutcome, error) {
	log := logf.FromContext(ctx)
	outcome := targetOutcome{ref: key.String()}

	peers, err := r.referencingAutomations(ctx, key, self)
	if err != nil {
		return outcome, err
	}

	var deployment appsv1.Deployment
	name := types.NamespacedName{Namespace: key.Namespace, Name: key.Name}
	if err := r.Get(ctx, name, &deployment); err != nil {
		if errors.IsForbidden(err) {
			return outcome, fmt.Errorf(
				"target %s not reachable with current RBAC (cross-namespace targets need cluster-wide permissions): %w",
				key, err)
		}
		return outcome, fmt.Errorf("getting target %s: %w", key, err)
	}
	baseline, recorded := baselineOf(&deployment)

	var claims, reversals []engine.Intent
	for _, peer := range peers {
		matching := selfMatching
		if claimantOf(peer) != claimantOf(self) {
			matching = r.matchingOf(peer)
		}
		if claim, ok := claimFor(peer, key, matching); ok {
			claims = append(claims, claim)
			continue
		}
		if matching {
			continue
		}
		if reversal, ok := reversalFor(peer, key, baseline); ok {
			reversals = append(reversals, reversal)
		}
	}

	if level, ok := selfLevel(self, key, selfMatching, baseline); ok {
		value := int32(level)
		outcome.desired = &value
	}

	resolution, claimed := engine.Resolve(claims)
	switch {
	case claimed:
		value := int32(resolution.Level)
		outcome.effective = &value
		if outcome.desired != nil && *outcome.desired != value {
			outcome.deferredBy = withoutClaimant(resolution.Winners, claimantOf(self))
		}
		changed, err := r.claimTarget(ctx, &deployment, resolution, claims)
		outcome.changed = changed
		return outcome, err

	case recorded:
		// Claimed before, claimed by nobody now: apply the agreed reversal and
		// stop asserting a value for this target at all.
		var level *int32
		if release, ok := engine.Resolve(reversals); ok {
			value := int32(release.Level)
			level = &value
		}
		changed, err := r.releaseTarget(ctx, &deployment, level)
		outcome.changed = changed
		if changed {
			log.Info("released target", "target", key.String(), "replicas", level)
		}
		return outcome, err

	default:
		// Never claimed. Reactor asserts nothing, so the user is free to scale
		// this workload by hand.
		return outcome, nil
	}
}

// selfLevel is what the Automation being reconciled wants for a target, which
// is its claim while matching and its reversal while not.
func selfLevel(
	self *reactorv1alpha1.Automation,
	key targetKey,
	matching bool,
	baseline *int64,
) (int64, bool) {
	if matching {
		return levelFor(self, self.Spec.Actions, key)
	}
	intent, ok := reversalFor(self, key, baseline)
	return intent.Level, ok
}

func withoutClaimant(claimants []string, self string) []string {
	out := make([]string, 0, len(claimants))
	for _, claimant := range claimants {
		if claimant != self {
			out = append(out, claimant)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// referencingAutomations gathers every Automation with an opinion about a
// target. self is substituted in explicitly rather than taken from the cache,
// which may hold a stale copy or — just after creation — not hold it at all.
func (r *AutomationReconciler) referencingAutomations(
	ctx context.Context,
	key targetKey,
	self *reactorv1alpha1.Automation,
) ([]*reactorv1alpha1.Automation, error) {
	var list reactorv1alpha1.AutomationList
	if err := r.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing automations sharing target %s: %w", key, err)
	}

	peers := []*reactorv1alpha1.Automation{self}
	for i := range list.Items {
		peer := &list.Items[i]
		if peer.Spec.When == nil || claimantOf(peer) == claimantOf(self) {
			continue
		}
		if references(peer, key) {
			peers = append(peers, peer)
		}
	}
	return peers, nil
}

// matchingOf reports whether a peer's condition currently holds. It is
// recomputed from the store rather than read from status, so a peer that has
// not reconciled since the last observation still contributes the right claim.
// When the provider is not reporting every key that peer needs, its last
// recorded matching stands — the same "hold, don't guess" rule the reconciler
// applies to itself.
func (r *AutomationReconciler) matchingOf(automation *reactorv1alpha1.Automation) bool {
	assessment := r.evaluate(automation)
	if !assessment.known || len(assessment.missing) > 0 {
		return automation.Status.Matching
	}
	return assessment.matching
}

// claimTarget writes the resolved value and marks the target as claimed.
func (r *AutomationReconciler) claimTarget(
	ctx context.Context,
	deployment *appsv1.Deployment,
	resolution engine.Resolution,
	claims []engine.Intent,
) (bool, error) {
	patch := client.MergeFrom(deployment.DeepCopy())
	dirty := false

	if _, recorded := deployment.Annotations[annotationBaselineReplicas]; !recorded {
		// Recorded once, on the transition from unclaimed to claimed. Writing
		// it again later would capture the value Reactor itself set — after a
		// controller restart mid-outage that means recording 0 as the
		// baseline, and the workload never comes back.
		baseline := int32(0)
		if deployment.Spec.Replicas != nil {
			baseline = *deployment.Spec.Replicas
		}
		dirty = setAnnotation(deployment, annotationBaselineReplicas, strconv.Itoa(int(baseline))) || dirty
		dirty = setAnnotation(deployment, annotationClaimedAt, metav1.Now().UTC().Format(time.RFC3339)) || dirty
	}

	claimants := make([]string, 0, len(claims))
	for _, claim := range claims {
		claimants = append(claimants, claim.Claimant)
	}
	slices.Sort(claimants)
	dirty = setAnnotation(deployment, annotationClaimedBy, strings.Join(claimants, ",")) || dirty

	replicas := int32(resolution.Level)
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != replicas {
		deployment.Spec.Replicas = &replicas
		dirty = true
	}
	if !dirty {
		return false, nil
	}

	if err := r.Patch(ctx, deployment, patch); err != nil {
		return false, fmt.Errorf("claiming %s/%s at %d replicas: %w",
			deployment.Namespace, deployment.Name, replicas, err)
	}
	logf.FromContext(ctx).Info("claimed target",
		"target", fmt.Sprintf("deployment/%s/%s", deployment.Namespace, deployment.Name),
		"replicas", replicas, "claimedBy", claimants)
	return true, nil
}

// releaseTarget applies the agreed reversal, if any, and removes Reactor's
// annotations so that nothing asserts a value for this target any more.
func (r *AutomationReconciler) releaseTarget(
	ctx context.Context,
	deployment *appsv1.Deployment,
	level *int32,
) (bool, error) {
	patch := client.MergeFrom(deployment.DeepCopy())
	dirty := false

	if level != nil && (deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != *level) {
		value := *level
		deployment.Spec.Replicas = &value
		dirty = true
	}
	for _, annotation := range []string{annotationBaselineReplicas, annotationClaimedBy, annotationClaimedAt} {
		if _, present := deployment.Annotations[annotation]; present {
			delete(deployment.Annotations, annotation)
			dirty = true
		}
	}
	if !dirty {
		return false, nil
	}

	if err := r.Patch(ctx, deployment, patch); err != nil {
		return false, fmt.Errorf("releasing %s/%s: %w", deployment.Namespace, deployment.Name, err)
	}
	return true, nil
}

// setAnnotation sets one annotation and reports whether that changed anything,
// so callers can skip writes that would produce an empty patch.
func setAnnotation(deployment *appsv1.Deployment, key, value string) bool {
	if deployment.Annotations == nil {
		deployment.Annotations = map[string]string{}
	}
	if deployment.Annotations[key] == value {
		return false
	}
	deployment.Annotations[key] = value
	return true
}
