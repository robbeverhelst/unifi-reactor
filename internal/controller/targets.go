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
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
)

// The kinds an action may target. The set is closed in the CRD as well, and
// deliberately: it is also the set the chart grants RBAC for, so an open field
// would accept a kind the operator cannot touch and report that as a Forbidden
// at 3am rather than as a rejected write at admission.
const (
	kindDeployment  = "Deployment"
	kindStatefulSet = "StatefulSet"
	kindCronJob     = "CronJob"
	kindNode        = "Node"
)

// fieldSpec is the top of every path below. Spelled once because a typo in it
// reads as "the field is absent", which for a boolean level means "not
// suspended" — a wrong answer rather than an error.
const fieldSpec = "spec"

// A level is an int64 the arbiter orders and never interprets: lower is more
// restrictive, and a shared target resolves to the lowest anybody asked for.
//
// That is the whole reason a boolean action fits the existing arbiter without
// generalising it. A switch is a two-element lattice, and embedding it in the
// integers as "set is 0, clear is 1" is order-preserving, so meet stays min and
// "most restrictive wins" stays "suspended wins" without the engine learning a
// second kind of value. Generalising engine.Resolve over an ordered type
// parameter would buy nothing here either: a target has exactly one kind, so
// two levels of different units never meet in the first place.
const (
	// levelSet is a boolean target's restrictive level: suspended.
	levelSet int64 = 0
	// levelClear is its permissive one: running.
	levelClear int64 = 1
)

const (
	// annotationBaselineReplicas records the replica count a scalable target
	// had before Reactor first claimed it. It lives on the target rather than
	// in an Automation's status because it has to outlive both the Automation
	// and Reactor itself: it is the only thing that can answer "what was this
	// before?" after the operator is uninstalled.
	//
	// It is a compatibility promise, so it keeps meaning exactly a replica
	// count. A target whose level is not a replica count records its baseline
	// under its own annotation instead of borrowing this one, because a
	// v1.0-era reader — a person, a script over `kubectl get -o custom-columns`
	// — is entitled to keep reading "1" here as one replica.
	annotationBaselineReplicas = "reactor.robbeverhelst.com/baseline-replicas"
	// annotationBaselineSuspend records whether a CronJob was suspended before
	// Reactor first claimed it, as "true" or "false".
	annotationBaselineSuspend = "reactor.robbeverhelst.com/baseline-suspend"
	// annotationBaselineUnschedulable records whether a Node was cordoned before
	// Reactor first claimed it, as "true" or "false". It matters more here than
	// anywhere else: a node cordoned by hand for maintenance must come back
	// cordoned, or Reactor's release would quietly undo a human's decision.
	annotationBaselineUnschedulable = "reactor.robbeverhelst.com/baseline-unschedulable"
	// annotationClaimedBy names the Automations currently holding the target.
	// Advisory: refreshed on every claim, never read back as truth. It exists
	// so that describing a target explains why it is scaled to zero.
	annotationClaimedBy = "reactor.robbeverhelst.com/claimed-by"
	// annotationClaimedAt records when the current claim began.
	annotationClaimedAt = "reactor.robbeverhelst.com/claimed-at"
)

// claimAnnotations are every annotation a claim writes, and so every one a
// release has to take back off again.
var claimAnnotations = []string{
	annotationBaselineReplicas,
	annotationBaselineSuspend,
	annotationBaselineUnschedulable,
	annotationClaimedBy,
	annotationClaimedAt,
}

// targetHandler is everything Reactor needs to know about one target kind: how
// to address it, what its level means, and where the pre-claim value is kept.
//
// It exists so that arbitration, the baseline, the release finalizer and the
// pre-delete sweep are written once against "a target with a level" rather than
// once per kind. Adding a kind is an entry in handlers, an entry in the CRD
// enum and a rule in the chart — no new code on any of those four paths.
type targetHandler struct {
	// gvk addresses the target. Reads go through an unstructured object rather
	// than a typed one so that no path here has to enumerate kinds, and
	// uncached, so a target kind costs no informer and needs no list or watch.
	gvk schema.GroupVersionKind

	// clusterScoped marks a kind that has no namespace, so a target ref naming
	// one is never silently defaulted into the Automation's own.
	clusterScoped bool

	// contested marks a kind whose level another controller can own. A
	// HorizontalPodAutoscaler writes exactly the field the scale subresource
	// exposes, so every scalable kind is contested and nothing else here is —
	// nothing autoscales a CronJob's suspend flag or a Node's cordon.
	//
	// It is what keeps the detection off the paths where it could only ever
	// find nothing: an unclaimed CronJob costs no list, and adding a kind
	// nothing else drives costs no thought about this at all.
	contested bool

	// baseline is the annotation this kind's pre-claim level is recorded in.
	baseline string

	// actionType is the desired-state action that reaches this kind. It is
	// reported in metrics for a target this Automation only names on the other
	// side of its transition, where there is no action to read it from.
	actionType string

	// read returns the level the target currently holds. It takes a client
	// because a level does not have to live in the object: a replica count is
	// read through the scale subresource, which is what makes one handler serve
	// every scalable kind rather than one per replicas field path.
	read func(ctx context.Context, c client.Client, obj *unstructured.Unstructured) (int64, error)

	// apply writes level and reports whether that changed anything.
	apply func(ctx context.Context, c client.Client, obj *unstructured.Unstructured, level int64) (bool, error)

	// format and parse move a level in and out of the baseline annotation.
	format func(int64) string
	parse  func(string) (int64, error)

	// describe renders a level for an Event, a log line and status.level.
	describe func(int64) string
}

// handlers is the registry. A kind absent from it cannot be targeted, which is
// also what the CRD enum enforces one layer earlier.
var handlers = map[string]targetHandler{
	kindDeployment:  replicaHandler(appsv1.SchemeGroupVersion.WithKind(kindDeployment)),
	kindStatefulSet: replicaHandler(appsv1.SchemeGroupVersion.WithKind(kindStatefulSet)),
	kindCronJob: switchHandler(
		batchv1.SchemeGroupVersion.WithKind(kindCronJob),
		[]string{fieldSpec, "suspend"},
		annotationBaselineSuspend,
		actionCronJobSuspend,
		"suspended", "running",
	),
	kindNode: clusterScopedHandler(switchHandler(
		corev1.SchemeGroupVersion.WithKind(kindNode),
		[]string{fieldSpec, "unschedulable"},
		annotationBaselineUnschedulable,
		actionKubernetesCordon,
		"cordoned", "schedulable",
	)),
}

// clusterScopedHandler marks a handler's kind as having no namespace.
func clusterScopedHandler(handler targetHandler) targetHandler {
	handler.clusterScoped = true
	return handler
}

// clusterScopedKind reports whether a target kind has no namespace. An
// unrecognised kind is treated as namespaced, which is the safe answer: it will
// fail at handlerFor with a clear message rather than being looked up at the
// wrong scope.
func clusterScopedKind(kind string) bool {
	return handlers[kind].clusterScoped
}

// handlerFor resolves a target kind. It fails rather than defaulting: a kind
// that reached here unrecognised means the CRD enum and this registry have
// drifted apart, and guessing which one is right would write to the wrong
// object.
func handlerFor(kind string) (targetHandler, error) {
	handler, ok := handlers[kind]
	if !ok {
		return targetHandler{}, fmt.Errorf("no handler for target kind %q", kind)
	}
	return handler, nil
}

// newTarget builds the empty object a target is read into.
func newTarget(handler targetHandler) *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(handler.gvk)
	return object
}

// replicaHandler is the level-as-a-count kind: a replica count, ordered so that
// fewer replicas is more restrictive and shedding therefore wins a fold.
//
// It reads and writes through the scale subresource rather than the kind's own
// replicas field, which is the whole reason one handler covers Deployment,
// StatefulSet and anything else scalable: /scale is the interface that says
// "this object has a replica count" without saying where it is kept, so a kind
// costs an entry in handlers, an entry in the CRD enum and an RBAC rule, and no
// code at all. A kind whose replicas live somewhere unusual works for free; a
// kind with no scale subresource is refused by the API server rather than
// half-written.
func replicaHandler(gvk schema.GroupVersionKind) targetHandler {
	path := []string{fieldSpec, "replicas"}
	return targetHandler{
		gvk:        gvk,
		baseline:   annotationBaselineReplicas,
		actionType: actionKubernetesScale,
		// /scale is exactly the interface a HorizontalPodAutoscaler writes
		// through, so the property that makes one handler serve every scalable
		// kind is also what makes every scalable kind contestable.
		contested: true,
		read: func(ctx context.Context, c client.Client, obj *unstructured.Unstructured) (int64, error) {
			scale, err := scaleOf(ctx, c, obj)
			if err != nil {
				return 0, err
			}
			value, found, err := unstructured.NestedInt64(scale.Object, path...)
			if err != nil {
				return 0, fmt.Errorf("reading the scale of %s: %w", describeObject(obj), err)
			}
			if !found {
				// The API server defaults it, so this is only reachable for a
				// kind whose scale reports none. One is the value such an object
				// behaves as, and is what a release should restore.
				return 1, nil
			}
			return value, nil
		},
		apply: func(ctx context.Context, c client.Client, obj *unstructured.Unstructured, level int64) (bool, error) {
			scale, err := scaleOf(ctx, c, obj)
			if err != nil {
				return false, err
			}
			current, _, err := unstructured.NestedInt64(scale.Object, path...)
			if err != nil {
				return false, fmt.Errorf("reading the scale of %s: %w", describeObject(obj), err)
			}
			if current == level {
				return false, nil
			}
			if err := unstructured.SetNestedField(scale.Object, level, path...); err != nil {
				return false, fmt.Errorf("setting the scale of %s: %w", describeObject(obj), err)
			}
			if err := c.SubResource(subResourceScale).Update(ctx, obj, client.WithSubResourceBody(scale)); err != nil {
				return false, fmt.Errorf("scaling %s to %d: %w", describeObject(obj), level, err)
			}
			return true, nil
		},
		format:   func(level int64) string { return strconv.FormatInt(level, 10) },
		parse:    func(raw string) (int64, error) { return strconv.ParseInt(raw, 10, 32) },
		describe: func(level int64) string { return fmt.Sprintf("%d replicas", level) },
	}
}

// subResourceScale is the subresource every scalable kind exposes.
const subResourceScale = "scale"

// scaleOf reads a target's scale subresource.
//
// It is fetched as an unstructured autoscaling/v1 Scale rather than the typed
// one because the parent is unstructured, and controller-runtime's unstructured
// client requires a subresource body of the same shape.
func scaleOf(
	ctx context.Context,
	c client.Client,
	obj *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	scale := &unstructured.Unstructured{}
	scale.SetGroupVersionKind(autoscalingv1.SchemeGroupVersion.WithKind("Scale"))
	if err := c.SubResource(subResourceScale).Get(ctx, obj, scale); err != nil {
		return nil, fmt.Errorf("reading the scale of %s: %w", describeObject(obj), err)
	}
	return scale, nil
}

// switchHandler is the level-as-a-switch kind: one boolean spec field whose
// true is the restrictive answer, mapped onto levelSet so that the fold reads
// the same way it does for replicas — most restrictive wins.
//
// set and clear are how the two levels are said out loud, because "0" is not
// what an operator reading an Event at 3am needs to be told.
func switchHandler(
	gvk schema.GroupVersionKind,
	path []string,
	baseline, actionType, set, clear string,
) targetHandler {
	return targetHandler{
		gvk:        gvk,
		baseline:   baseline,
		actionType: actionType,
		read: func(_ context.Context, _ client.Client, obj *unstructured.Unstructured) (int64, error) {
			value, _, err := unstructured.NestedBool(obj.Object, path...)
			if err != nil {
				return 0, fmt.Errorf("reading %v of %s: %w", path, describeObject(obj), err)
			}
			// Absent is false, which is what the API server defaults it to.
			return levelOfFlag(value), nil
		},
		apply: func(ctx context.Context, c client.Client, obj *unstructured.Unstructured, level int64) (bool, error) {
			current, _, err := unstructured.NestedBool(obj.Object, path...)
			if err != nil {
				return false, fmt.Errorf("reading %v of %s: %w", path, describeObject(obj), err)
			}
			want := level == levelSet
			if current == want {
				return false, nil
			}
			patch := client.MergeFrom(obj.DeepCopy())
			if err := unstructured.SetNestedField(obj.Object, want, path...); err != nil {
				return false, fmt.Errorf("setting %v of %s: %w", path, describeObject(obj), err)
			}
			if err := c.Patch(ctx, obj, patch); err != nil {
				return false, fmt.Errorf("setting %s to %v on %s: %w",
					path[len(path)-1], want, describeObject(obj), err)
			}
			return true, nil
		},
		format: func(level int64) string { return strconv.FormatBool(level == levelSet) },
		parse: func(raw string) (int64, error) {
			flag, err := strconv.ParseBool(raw)
			if err != nil {
				return 0, err
			}
			return levelOfFlag(flag), nil
		},
		describe: func(level int64) string {
			if level == levelSet {
				return set
			}
			return clear
		},
	}
}

// levelOfFlag maps a boolean spec field onto the arbiter's ordering: true is
// the restrictive answer and therefore the one a fold picks.
func levelOfFlag(flag bool) int64 {
	if flag {
		return levelSet
	}
	return levelClear
}

// levelOfAction is the level one action asks for, in the units of its type.
//
// A boolean action with its field omitted means the level it is named after —
// kubernetes.cronjob.suspend means suspended — so that spec.actions reads as
// the sentence it is, and spec.onExit says suspended: false to ask for it back.
func levelOfAction(action reactorv1alpha1.Action) (int64, bool) {
	switch action.Type {
	case actionKubernetesScale:
		if action.Replicas == nil {
			return 0, false
		}
		return int64(*action.Replicas), true
	case actionCronJobSuspend:
		return levelOfFlag(action.Suspended == nil || *action.Suspended), true
	case actionKubernetesCordon:
		return levelOfFlag(action.Cordoned == nil || *action.Cordoned), true
	default:
		return 0, false
	}
}

// describeObject names an object for an error message without assuming it has
// a namespace.
func describeObject(obj *unstructured.Unstructured) string {
	if obj.GetNamespace() == "" {
		return obj.GetKind() + "/" + obj.GetName()
	}
	return obj.GetKind() + "/" + obj.GetNamespace() + "/" + obj.GetName()
}
