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
	"maps"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/engine"
	"github.com/robbeverhelst/unifi-reactor/internal/metrics"
)

// targetKey identifies a target workload across every Automation referencing
// it. Arbitration is per target, so this is the unit claims are grouped by.
type targetKey struct {
	Kind      string
	Namespace string
	Name      string
}

func (k targetKey) String() string {
	if k.Namespace == "" {
		return k.Kind + "/" + k.Name
	}
	return k.Kind + "/" + k.Namespace + "/" + k.Name
}

// targetKeyFor resolves an action's target, defaulting the namespace to the
// Automation's own — the same defaulting the API documents.
//
// A cluster-scoped kind gets no namespace at all, not even a defaulted one. The
// CRD rejects a namespace on one, and this is the second half of that rule: a
// Node addressed inside a namespace is not a different Node, it is a read that
// fails, and the failure would be reported as an unreachable target rather than
// as the mistake it is.
func targetKeyFor(automation *reactorv1alpha1.Automation, action reactorv1alpha1.Action) (targetKey, bool) {
	if action.Target == nil {
		return targetKey{}, false
	}
	namespace := action.Target.Namespace
	switch {
	case clusterScopedKind(action.Target.Kind):
		namespace = ""
	case namespace == "":
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
		if !isDesiredState(action.Type) {
			continue
		}
		if k, ok := targetKeyFor(automation, action); !ok || k != key {
			continue
		}
		if level, ok := levelOfAction(action); ok {
			return level, true
		}
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

// baselineOf reads the recorded pre-claim level off a target. recorded reports
// whether Reactor has ever claimed this target, which is what distinguishes
// "release it back to where it was" from "never touched it, leave it alone".
// A value that cannot be parsed still counts as recorded: the target is
// released, it simply cannot contribute a baseline reversal.
//
// Which annotation holds it is the handler's business, because the level is
// only a replica count for the kinds whose level is a count.
func baselineOf(handler targetHandler, obj *unstructured.Unstructured) (level *int64, recorded bool) {
	raw, ok := obj.GetAnnotations()[handler.baseline]
	if !ok {
		return nil, false
	}
	parsed, err := handler.parse(raw)
	if err != nil {
		return nil, true
	}
	return &parsed, true
}

// permissionHint names what an unreachable target would have needed. The two
// ways it happens have different fixes, and the person reading this cannot see
// the operator's RBAC: a namespaced target is a scope problem, while a node is
// a feature the install has to opt into and would otherwise look like a bug in
// the automation rather than a value that was never set.
func permissionHint(kind string) string {
	if clusterScopedKind(kind) {
		return "node actions are opt-in: install with rbac.allowNodeActions=true"
	}
	return "cross-namespace targets need cluster-wide permissions"
}

// outOfScope reports whether a target could not be read because Reactor is not
// allowed to see it, in either of the two ways that happens.
//
// A cluster-wide install is refused by the API server and gets a Forbidden. A
// namespaced install never asks: its cache is restricted to the one namespace
// it may watch, so the cached client rejects the read itself, with an error
// that carries no status and would otherwise be reported as an unexplained
// failure to reach the target. Both mean the same thing to the person reading
// the status, so both get the same sentence.
func outOfScope(err error) bool {
	return errors.IsForbidden(err) || strings.Contains(err.Error(), "unknown namespace for the cache")
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
	// level says effective in the units of the action that set it, because a
	// bare number stops explaining itself once a level is a switch.
	level string
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

	// Bounded per action, so a target that has stopped answering fails and is
	// retried rather than holding this reconcile — and, with concurrent
	// reconciles, rather than starving every other Automation.
	ctx, cancel := context.WithTimeout(ctx, timeoutFor(self, key, selfMatching))
	defer cancel()

	handler, err := handlerFor(key.Kind)
	if err != nil {
		return outcome, err
	}

	peers, err := r.referencingAutomations(ctx, key, self)
	if err != nil {
		return outcome, err
	}

	target := newTarget(handler)
	name := types.NamespacedName{Namespace: key.Namespace, Name: key.Name}
	if err := r.Get(ctx, name, target); err != nil {
		if outOfScope(err) {
			return outcome, fmt.Errorf("target %s not reachable with current RBAC (%s): %w",
				key, permissionHint(key.Kind), err)
		}
		return outcome, fmt.Errorf("getting target %s: %w", key, err)
	}
	baseline, recorded := baselineOf(handler, target)

	var claims, reversals []engine.Intent
	for _, peer := range peers {
		matching := selfMatching
		if claimantOf(peer) != claimantOf(self) {
			// A peer that is being deleted or is suspended has stopped
			// claiming, exactly as this Automation does when it is the one
			// being deleted or suspended. Its reversal still counts, which is
			// what restores a workload when the Automation holding it down is
			// removed or paused mid-outage.
			matching = peer.InForce() && r.matchingOf(peer)
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
		outcome.level = handler.describe(resolution.Level)
		if outcome.desired != nil && *outcome.desired != value {
			outcome.deferredBy = withoutClaimant(resolution.Winners, claimantOf(self))
		}
		metrics.ArbitrationResolved(outcomeOf(outcome))
		changed, err := claimTarget(ctx, r.Client, handler, target, resolution, claims)
		outcome.changed = changed
		return outcome, err

	case recorded:
		// Claimed before, claimed by nobody now: apply the agreed reversal and
		// stop asserting a value for this target at all.
		var level *int64
		if release, ok := engine.Resolve(reversals); ok {
			level = &release.Level
		}
		metrics.ArbitrationResolved(metrics.OutcomeReleased)
		changed, err := releaseTarget(ctx, r.Client, handler, target, level)
		outcome.changed = changed
		if changed {
			log.Info("released target", "target", key.String(), "level", describeLevel(handler, level))
		}
		return outcome, err

	default:
		// Never claimed. Reactor asserts nothing, so the user is free to change
		// this workload by hand.
		return outcome, nil
	}
}

// describeLevel renders a level that may be absent, which is what a release
// with every claimant on reversal None looks like.
func describeLevel(handler targetHandler, level *int64) string {
	if level == nil {
		return "left as found"
	}
	return handler.describe(*level)
}

// timeoutFor bounds one attempt at a target, taken from the action this
// Automation reaches it through.
func timeoutFor(self *reactorv1alpha1.Automation, key targetKey, matching bool) time.Duration {
	for _, action := range actionsFor(self, matching) {
		if !isDesiredState(action.Type) || action.TimeoutSeconds == nil {
			continue
		}
		if k, ok := targetKeyFor(self, action); ok && k == key {
			return time.Duration(*action.TimeoutSeconds) * time.Second
		}
	}
	return defaultActionTimeout
}

// actionTypeFor names the desired-state action this Automation reaches a target
// through, so an execution is counted under the type that performed it rather
// than under whichever type happens to be the only one implemented.
func actionTypeFor(self *reactorv1alpha1.Automation, key targetKey, matching bool) string {
	for _, action := range actionsFor(self, matching) {
		if !isDesiredState(action.Type) {
			continue
		}
		if k, ok := targetKeyFor(self, action); ok && k == key {
			return action.Type
		}
	}
	// Reached only through a target this Automation names on the other side of
	// the transition, e.g. a reversal to baseline with no onExit entry. The
	// kind decides it, because a kind has exactly one desired-state action that
	// reaches it.
	if handler, err := handlerFor(key.Kind); err == nil {
		return handler.actionType
	}
	return actionKubernetesScale
}

// actionsFor is the side of an Automation currently in play: what it asks for
// while its condition holds, and what it wants back once it does not.
func actionsFor(self *reactorv1alpha1.Automation, matching bool) []reactorv1alpha1.Action {
	if matching {
		return self.Spec.Actions
	}
	return self.Spec.OnExit
}

// outcomeOf reports how a claimed target resolved from this Automation's point
// of view: it got what it wanted, or a more restrictive peer is holding it.
func outcomeOf(outcome targetOutcome) string {
	if len(outcome.deferredBy) > 0 {
		return metrics.OutcomeDeferred
	}
	return metrics.OutcomeClaimed
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

// claimTarget marks the target as claimed and writes the resolved level.
//
// The annotations go first and in their own write. The baseline is the record
// of what the target was before Reactor touched it, so capturing it has to be
// durable before the value it describes is overwritten: a crash between the two
// leaves a target still at its own value with the baseline already recorded,
// which the next reconcile corrects. The other order loses the baseline
// permanently.
func claimTarget(
	ctx context.Context,
	c client.Client,
	handler targetHandler,
	target *unstructured.Unstructured,
	resolution engine.Resolution,
	claims []engine.Intent,
) (bool, error) {
	claimants := make([]string, 0, len(claims))
	for _, claim := range claims {
		claimants = append(claimants, claim.Claimant)
	}
	slices.Sort(claimants)

	annotations := map[string]string{annotationClaimedBy: strings.Join(claimants, ",")}
	if _, recorded := target.GetAnnotations()[handler.baseline]; !recorded {
		// Recorded once, on the transition from unclaimed to claimed. Writing
		// it again later would capture the value Reactor itself set — after a
		// controller restart mid-outage that means recording 0 as the
		// baseline, and the workload never comes back.
		found, err := handler.read(ctx, c, target)
		if err != nil {
			return false, err
		}
		annotations[handler.baseline] = handler.format(found)
		annotations[annotationClaimedAt] = metav1.Now().UTC().Format(time.RFC3339)
	}
	changed, err := setAnnotations(ctx, c, target, annotations)
	if err != nil {
		return changed, fmt.Errorf("recording the claim on %s: %w", describeObject(target), err)
	}

	written, err := handler.apply(ctx, c, target, resolution.Level)
	if err != nil {
		return changed, err
	}
	if !written {
		return changed, nil
	}
	logf.FromContext(ctx).Info("claimed target",
		"target", describeObject(target),
		"level", handler.describe(resolution.Level), "claimedBy", claimants)
	return true, nil
}

// releaseTarget applies the agreed reversal, if any, and removes Reactor's
// annotations so that nothing asserts a value for this target any more.
//
// The reverse order to claiming, for the same reason: the annotations are what
// says the target is still held, so they come off only once the value they
// describe has been restored.
func releaseTarget(
	ctx context.Context,
	c client.Client,
	handler targetHandler,
	target *unstructured.Unstructured,
	level *int64,
) (bool, error) {
	changed := false
	if level != nil {
		written, err := handler.apply(ctx, c, target, *level)
		if err != nil {
			return false, err
		}
		changed = written
	}

	cleared, err := clearAnnotations(ctx, c, target)
	if err != nil {
		return changed, fmt.Errorf("releasing %s: %w", describeObject(target), err)
	}
	return changed || cleared, nil
}

// setAnnotations adds or updates the annotations given and touches no other,
// reporting whether that changed anything so callers can skip a write that
// would produce an empty patch.
//
// Adding only. A claim refreshes claimed-by on every reconcile but passes the
// baseline exactly once, on the reconcile that first took the target — so
// anything that treated "not in this map" as "remove it" would erase the
// baseline fifteen seconds after recording it, and the release would then find
// nothing to restore.
func setAnnotations(
	ctx context.Context,
	c client.Client,
	target *unstructured.Unstructured,
	want map[string]string,
) (bool, error) {
	current := target.GetAnnotations()
	next := maps.Clone(current)
	if next == nil {
		next = map[string]string{}
	}
	dirty := false
	for annotation, value := range want {
		if current[annotation] != value {
			next[annotation] = value
			dirty = true
		}
	}
	return patchAnnotations(ctx, c, target, next, dirty)
}

// clearAnnotations removes every annotation a claim writes, which is what stops
// Reactor asserting anything about a target at all.
func clearAnnotations(ctx context.Context, c client.Client, target *unstructured.Unstructured) (bool, error) {
	current := target.GetAnnotations()
	next := maps.Clone(current)
	dirty := false
	for _, annotation := range claimAnnotations {
		if _, present := next[annotation]; present {
			delete(next, annotation)
			dirty = true
		}
	}
	return patchAnnotations(ctx, c, target, next, dirty)
}

func patchAnnotations(
	ctx context.Context,
	c client.Client,
	target *unstructured.Unstructured,
	next map[string]string,
	dirty bool,
) (bool, error) {
	if !dirty {
		return false, nil
	}
	patch := client.MergeFrom(target.DeepCopy())
	target.SetAnnotations(next)
	if err := c.Patch(ctx, target, patch); err != nil {
		return false, err
	}
	return true, nil
}
