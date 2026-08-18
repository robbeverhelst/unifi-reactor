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

// reversalIntents is what every Automation referencing a target says that
// target's normal level is: what each would hand it back to once nothing claims
// it any more.
//
// It is deliberately taken over EVERY referencing Automation, matching or not,
// where the reversals folded at release are taken only over the ones not
// currently claiming. The two answer different questions. That fold asks what
// should happen now; this asks what the specs declare, which is answerable at
// any moment and is answerable long before the moment it matters.
//
// A peer out of force still counts. Suspending an Automation does not withdraw
// its opinion about a workload's normal size — resuming it brings the same
// contradiction straight back — and one being deleted contributes its reversal
// to the release that deletion causes.
func reversalIntents(
	peers []*reactorv1alpha1.Automation,
	key targetKey,
	baseline *int64,
) []engine.Intent {
	intents := make([]engine.Intent, 0, len(peers))
	for _, peer := range peers {
		if intent, ok := reversalFor(peer, key, baseline); ok {
			intents = append(intents, intent)
		}
	}
	return intents
}

// disagreementOver reports the intents when they do not all name the same
// level, and nothing when they do.
//
// Everything or nothing, rather than only the outvoted ones: a disagreement is
// a fact about a set of specs and not about any one of them, and naming only
// the losers would read as "these were overruled" when what happened is that
// two people wrote down different answers to the same question. Sorted by
// claimant so a status field and an Event stay stable across reconciles that
// gathered the peers in a different order.
func disagreementOver(handler targetHandler, intents []engine.Intent) []reactorv1alpha1.ReversalIntent {
	if len(intents) < 2 {
		return nil
	}
	if !slices.ContainsFunc(intents, func(i engine.Intent) bool { return i.Level != intents[0].Level }) {
		return nil
	}

	reported := make([]reactorv1alpha1.ReversalIntent, 0, len(intents))
	for _, intent := range intents {
		reported = append(reported, reactorv1alpha1.ReversalIntent{
			Claimant: intent.Claimant,
			Desired:  int32(intent.Level),
			Level:    handler.describe(intent.Level),
		})
	}
	slices.SortFunc(reported, func(a, b reactorv1alpha1.ReversalIntent) int {
		return strings.Compare(a.Claimant, b.Claimant)
	})
	return reported
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
	// preview is what would happen here if this Automation were in force,
	// computed only when it deliberately is not.
	preview *reactorv1alpha1.TargetPreview
	// withheld reports an outcome that was resolved and then not written,
	// because the whole install is running as a dry run.
	withheld bool
	// disagreement is every Automation's declared reversal level for this
	// target, when they do not all declare the same one. Empty when they agree.
	disagreement []reactorv1alpha1.ReversalIntent
	// managedBy names the controller Reactor declined to fight for this target.
	managedBy string
}

// stance is how the Automation being reconciled takes part in one target's
// arbitration this pass.
//
// The first two fields are not the same question, and the difference is the
// whole of a dry run: matching says the condition holds, claiming says that
// condition currently counts for anything. An Automation held out of force by a
// policy can be matching without claiming, which is exactly the state a preview
// describes.
type stance struct {
	// matching is whether this Automation's condition currently holds.
	matching bool
	// claiming is whether that condition puts a claim on the target. False
	// while the Automation is out of force, however it got there.
	claiming bool
	// preview asks for the counterfactual alongside the outcome: what
	// arbitration would resolve to if this Automation's claim did count. Set
	// for an Automation deliberately out of force, and not for one being
	// deleted — that one has no future to describe.
	preview bool
}

// reconcileTarget resolves one target across every Automation referencing it
// and writes the outcome. It is called on every reconcile, not only on
// transitions: another Automation entering or leaving its state changes what
// this target should be without anything about this one changing.
func (r *AutomationReconciler) reconcileTarget(
	ctx context.Context,
	key targetKey,
	self *reactorv1alpha1.Automation,
	s stance,
) (targetOutcome, error) {
	log := logf.FromContext(ctx)
	outcome := targetOutcome{ref: key.String()}

	// Bounded per action, so a target that has stopped answering fails and is
	// retried rather than holding this reconcile — and, with concurrent
	// reconciles, rather than starving every other Automation.
	ctx, cancel := context.WithTimeout(ctx, timeoutFor(self, key, s.claiming))
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
		matching := s.claiming
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

	if level, ok := selfLevel(self, key, s.claiming, baseline); ok {
		value := int32(level)
		outcome.desired = &value
	}

	// Asked of every peer and on every reconcile, not only of the ones
	// currently reversing and not only at release. It is a comparison over
	// intents already in hand, and the point of asking now is that the answer
	// stops being useful the moment the workload has come back at the wrong
	// number.
	outcome.disagreement = disagreementOver(handler, reversalIntents(peers, key, baseline))
	if len(outcome.disagreement) > 0 {
		metrics.ReversalDisagreement()
	}

	// Before the preview, and that ordering is load-bearing. A target another
	// controller drives is one this Automation would decline rather than claim,
	// so previewing a level for it would answer a question with something that
	// is not what would happen. managedBy is the whole of the answer there.
	manager, err := r.foreignManagerOf(ctx, handler, key)
	if err != nil {
		return outcome, err
	}
	if manager != "" {
		outcome.managedBy = manager
		return r.declineTarget(ctx, handler, target, outcome, baseline, recorded)
	}

	if s.preview {
		outcome.preview = r.previewFor(ctx, handler, self, key, target, claims, baseline)
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
		if r.DryRun {
			return withhold(outcome), nil
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
		if r.DryRun {
			return withhold(outcome), nil
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

// declineTarget is what Reactor does about a target another controller drives.
//
// Never claimed: nothing is written at all, not even the annotations. Recording
// a baseline here would be worse than useless — the value it captured would be
// one the other controller is actively changing, so the number a later release
// restored would mean nothing, and claimed-by would name an owner Reactor is
// not.
//
// Already claimed: hand it back, to the baseline rather than to the arbitrated
// reversal. This is not a release because nothing claims the target any more;
// it is Reactor abdicating one it cannot hold, and "put it back where I found
// it" is the only honest answer to that.
//
// The claimed case is the one that would be worst to get wrong. An HPA appearing
// over a workload Reactor is holding at 0 is precisely where declining silently
// would strand it: an HPA does not scale a workload up from zero, so refusing
// to write would leave it at 0 with neither controller willing to move it. From
// the next reconcile the baseline is gone, so the target reads as never claimed
// and is simply one Reactor does not touch.
func (r *AutomationReconciler) declineTarget(
	ctx context.Context,
	handler targetHandler,
	target *unstructured.Unstructured,
	outcome targetOutcome,
	baseline *int64,
	recorded bool,
) (targetOutcome, error) {
	metrics.ArbitrationResolved(metrics.OutcomeDeclined)
	if !recorded || r.DryRun {
		return outcome, nil
	}

	outcome.level = describeLevel(handler, baseline)
	changed, err := releaseTarget(ctx, r.Client, handler, target, baseline)
	outcome.changed = changed
	if changed {
		logf.FromContext(ctx).Info("handed target back to another controller",
			"target", outcome.ref, "managedBy", outcome.managedBy, "level", outcome.level)
	}
	return outcome, err
}

// withhold marks an outcome that was resolved and then not written.
//
// A global dry run runs the arbitration exactly as it otherwise would and stops
// one line short of the write, which is what makes the report it produces worth
// reading: status.targets[].effective is the value the target would have, not a
// guess at one.
func withhold(outcome targetOutcome) targetOutcome {
	outcome.withheld = true
	metrics.ArbitrationResolved(metrics.OutcomeWithheld)
	return outcome
}

// previewFor answers "what would this do?" without doing it.
//
// Arbitration is a pure function of the claims that hold, so the counterfactual
// is the same fold run once more with this Automation's claim added to it. No
// write happens and nothing is read that a claim would not have read anyway,
// which is why a preview costs the reconcile that was going to happen regardless
// — and why it is reported continuously rather than asked for out of band.
//
// It deliberately previews the condition holding, whether or not it currently
// does. The question someone writes an automation to answer is what happens
// during the power cut, and they are writing it on an afternoon when the power
// is fine. While the condition does hold, the two readings are the same answer.
func (r *AutomationReconciler) previewFor(
	ctx context.Context,
	handler targetHandler,
	self *reactorv1alpha1.Automation,
	key targetKey,
	target *unstructured.Unstructured,
	claims []engine.Intent,
	baseline *int64,
) *reactorv1alpha1.TargetPreview {
	preview := &reactorv1alpha1.TargetPreview{
		OnExit: r.previewOnExit(ctx, handler, self, key, target, baseline),
	}

	claim, ok := claimFor(self, key, true)
	if !ok {
		// Nothing to add to the fold: this Automation asks for no level here,
		// so putting it in force would change nothing about this target.
		return preview
	}
	desired := int32(claim.Level)
	preview.Desired = &desired

	// Cloned rather than appended in place: claims is the slice the live
	// resolution below is taken over, and growing it here would be a
	// counterfactual leaking into the real answer.
	resolution, resolved := engine.Resolve(append(slices.Clone(claims), claim))
	if !resolved {
		return preview
	}
	effective := int32(resolution.Level)
	preview.Effective = &effective
	preview.Level = handler.describe(resolution.Level)
	if resolution.Level != claim.Level {
		preview.DeferredBy = withoutClaimant(resolution.Winners, claim.Claimant)
	}
	current, _ := engine.Resolve(claims)
	preview.WouldDefer = newlyDeferred(current.Deferred, resolution.Deferred, claim.Claimant)
	return preview
}

// previewOnExit says what this Automation would hand a target back to once its
// condition stopped holding.
//
// Under reversal Baseline on a target nothing has ever claimed there is no
// recorded baseline to name, because the baseline is captured by the claim that
// would not have happened yet. What such a claim would capture is whatever the
// target is at now, so that is what the preview reports — the one read here
// that a claim would also have made.
//
// Reading can fail, and a preview is never allowed to be the reason a reconcile
// does: an unreadable level is left unsaid rather than raised.
func (r *AutomationReconciler) previewOnExit(
	ctx context.Context,
	handler targetHandler,
	self *reactorv1alpha1.Automation,
	key targetKey,
	target *unstructured.Unstructured,
	baseline *int64,
) string {
	if baseline == nil && self.Spec.EffectiveReversal() == reactorv1alpha1.ReversalBaseline {
		found, err := handler.read(ctx, r.Client, target)
		if err != nil {
			return ""
		}
		baseline = &found
	}
	intent, ok := reversalFor(self, key, baseline)
	if !ok {
		return describeLevel(handler, nil)
	}
	return handler.describe(intent.Level)
}

// newlyDeferred names the claimants that stop getting what they want because of
// one added claim. A peer already outvoted by a third automation is deferred
// either way, and naming it here would credit this claim with something it did
// not do.
func newlyDeferred(before, after []string, self string) []string {
	out := make([]string, 0, len(after))
	for _, claimant := range after {
		if claimant == self || slices.Contains(before, claimant) {
			continue
		}
		out = append(out, claimant)
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
		return automation.Status.IsMatching()
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

	changed, err := recordClaim(ctx, c, handler, target, claimants)
	if err != nil {
		return changed, err
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
// recordClaim writes the claim annotations, and records the baseline if this is
// the first claim on this target.
//
// The baseline is the only thing that knows what a workload was before Reactor
// touched it, so recording it wrong is not cosmetic: the outage ends and the
// workload comes back at the level Reactor itself set. Two things make that
// easy to get wrong, and both are handled here.
//
// The test for "already recorded" reads the caller's SNAPSHOT of the target,
// while the level being recorded comes from a live read of the scale. Two
// Automations sharing a target reconcile concurrently — maxConcurrentReconciles
// is 4 — so the second one can still see no baseline in its snapshot while the
// live level is already the one the first one applied, and record that.
//
// So the write that records a baseline carries an optimistic lock: if the
// target moved between the snapshot and the write, the patch is refused rather
// than believed. On that conflict the target is re-read once and the claim is
// written again — by then the baseline the other reconcile recorded is visible,
// and this one leaves it alone. A second conflict is returned, because at that
// point something other than this race is happening and a reconcile that says
// so is better than one that keeps trying.
func recordClaim(
	ctx context.Context,
	c client.Client,
	handler targetHandler,
	target *unstructured.Unstructured,
	claimants []string,
) (bool, error) {
	for attempt := range 2 {
		annotations := map[string]string{annotationClaimedBy: strings.Join(claimants, ",")}
		_, recorded := target.GetAnnotations()[handler.baseline]
		if !recorded {
			// Recorded once, on the transition from unclaimed to claimed.
			// Writing it again later would capture the value Reactor itself
			// set — after a controller restart mid-outage that means recording
			// 0 as the baseline, and the workload never comes back.
			found, err := handler.read(ctx, c, target)
			if err != nil {
				return false, err
			}
			annotations[handler.baseline] = handler.format(found)
			annotations[annotationClaimedAt] = metav1.Now().UTC().Format(time.RFC3339)
		}

		changed, err := setAnnotations(ctx, c, target, annotations, !recorded)
		switch {
		case err == nil:
			return changed, nil
		case recorded || !errors.IsConflict(err) || attempt > 0:
			return changed, fmt.Errorf("recording the claim on %s: %w", describeObject(target), err)
		}

		// Someone else claimed this target first. Re-read so the baseline they
		// recorded — from a level nobody had changed yet — is the one that
		// stands.
		if err := c.Get(ctx, client.ObjectKeyFromObject(target), target); err != nil {
			return false, fmt.Errorf("re-reading %s after a claim conflict: %w", describeObject(target), err)
		}
	}
	return false, fmt.Errorf("recording the claim on %s: too many conflicts", describeObject(target))
}

func setAnnotations(
	ctx context.Context,
	c client.Client,
	target *unstructured.Unstructured,
	want map[string]string,
	guard bool,
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
	return patchAnnotations(ctx, c, target, next, dirty, guard)
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
	// No lock: removing a claim is idempotent, and a release built from a
	// slightly stale snapshot still removes exactly the annotations a claim
	// writes. It is recording a value that must not be got wrong, not clearing
	// one.
	return patchAnnotations(ctx, c, target, next, dirty, false)
}

// guard adds an optimistic lock, so a patch built from a stale snapshot is
// refused rather than applied. It is used where writing the wrong thing is
// worse than failing: recording a target's baseline.
func patchAnnotations(
	ctx context.Context,
	c client.Client,
	target *unstructured.Unstructured,
	next map[string]string,
	dirty bool,
	guard bool,
) (bool, error) {
	if !dirty {
		return false, nil
	}
	patch := client.MergeFrom(target.DeepCopy())
	if guard {
		patch = client.MergeFromWithOptions(target.DeepCopy(), client.MergeFromWithOptimisticLock{})
	}
	target.SetAnnotations(next)
	if err := c.Patch(ctx, target, patch); err != nil {
		return false, err
	}
	return true, nil
}
