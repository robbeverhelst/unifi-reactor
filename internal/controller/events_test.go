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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	eventsv1client "k8s.io/client-go/kubernetes/typed/events/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
)

// recordedEvent is one emission captured from a fake recorder.
type recordedEvent struct {
	Type   string
	Reason string
	Action string
	Note   string
}

// fakeRecorder captures emissions instead of writing them to an API server.
type fakeRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (f *fakeRecorder) Eventf(
	_ runtime.Object, _ runtime.Object,
	eventtype, reason, action, note string, args ...any,
) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, recordedEvent{
		Type: eventtype, Reason: reason, Action: action, Note: fmt.Sprintf(note, args...),
	})
}

func (f *fakeRecorder) reasons() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.events))
	for _, e := range f.events {
		out = append(out, e.Reason)
	}
	return out
}

func (f *fakeRecorder) find(reason string) (recordedEvent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if e.Reason == reason {
			return e, true
		}
	}
	return recordedEvent{}, false
}

func automationWithCondition(conditionType, reason string) *reactorv1alpha1.Automation {
	automation := &reactorv1alpha1.Automation{
		ObjectMeta: metav1.ObjectMeta{Name: "pause-on-backup-wan", Namespace: "media"},
		Spec: reactorv1alpha1.AutomationSpec{
			When: &reactorv1alpha1.StateTrigger{Provider: providerUniFi, State: map[string]string{keyWAN: wanBackup}},
		},
	}
	if conditionType != "" {
		automation.Status.Conditions = []metav1.Condition{{
			Type: conditionType, Status: metav1.ConditionFalse, Reason: reason,
			LastTransitionTime: metav1.Now(),
		}}
	}
	return automation
}

// TestARepeatedConditionRaisesOneEvent is the volume guard. Reconcile runs at
// least every reevaluateInterval, so an Event raised unconditionally from a
// steady state — a key that has been missing for an hour — would be an API
// write every fifteen seconds per Automation, forever.
func TestARepeatedConditionRaisesOneEvent(t *testing.T) {
	recorder := &fakeRecorder{}
	r := &AutomationReconciler{Recorder: recorder}

	fresh := automationWithCondition(conditionReady, "Reconciled")
	r.eventOnNewReason(fresh, conditionReady, corev1.EventTypeWarning,
		reasonStateKeyUnavailable, actionEvaluate, "ups went away")

	held := automationWithCondition(conditionReady, reasonStateKeyUnavailable)
	for range 10 {
		r.eventOnNewReason(held, conditionReady, corev1.EventTypeWarning,
			reasonStateKeyUnavailable, actionEvaluate, "ups went away")
	}

	if got := recorder.reasons(); len(got) != 1 {
		t.Fatalf("expected one Event on entering the state, got %d: %v", len(got), got)
	}
}

// TestAnEventIsRaisedAgainAfterRecovery pairs with the test above: suppressing
// repeats must not suppress a second occurrence. A key that vanishes, comes
// back and vanishes again is two incidents, not one.
func TestAnEventIsRaisedAgainAfterRecovery(t *testing.T) {
	recorder := &fakeRecorder{}
	r := &AutomationReconciler{Recorder: recorder}

	automation := automationWithCondition(conditionReady, reasonStateKeyUnavailable)
	r.eventOnNewReason(automation, conditionReady, corev1.EventTypeWarning,
		reasonStateKeyUnavailable, actionEvaluate, "ups went away")
	if got := recorder.reasons(); len(got) != 0 {
		t.Fatalf("expected silence while the condition already reported this, got %v", got)
	}

	r.setCondition(automation, conditionReady, metav1.ConditionTrue, "Reconciled", "recovered")
	r.eventOnNewReason(automation, conditionReady, corev1.EventTypeWarning,
		reasonStateKeyUnavailable, actionEvaluate, "ups went away again")
	if got := recorder.reasons(); len(got) != 1 {
		t.Fatalf("a second occurrence was suppressed: %v", got)
	}
}

// TestTheTransitionEventNamesTheStateChange is why these exist at all: the
// Event stream has to answer "what happened" without anyone reading logs. It is
// also where a state key with an open value set — isp — is reported, since that
// is deliberately not a metric label.
func TestTheTransitionEventNamesTheStateChange(t *testing.T) {
	recorder := &fakeRecorder{}
	r := &AutomationReconciler{Recorder: recorder}

	automation := automationWithCondition("", "")
	automation.Status.LastTransition = &reactorv1alpha1.StateTransition{
		Key: "isp", From: "carrier-a", To: "carrier-b", Time: metav1.Now(),
	}

	r.reportTransition(automation, true, true)
	entered, ok := recorder.find(reasonStateEntered)
	if !ok {
		t.Fatalf("no %s Event: %v", reasonStateEntered, recorder.reasons())
	}
	if entered.Type != corev1.EventTypeNormal {
		t.Errorf("entering a state is reported as %q; it is not a fault", entered.Type)
	}
	for _, want := range []string{"isp", "carrier-a", "carrier-b"} {
		if !strings.Contains(entered.Note, want) {
			t.Errorf("the Event does not name %q: %s", want, entered.Note)
		}
	}

	r.reportTransition(automation, true, false)
	if _, ok := recorder.find(reasonStateExited); !ok {
		t.Errorf("no %s Event: %v", reasonStateExited, recorder.reasons())
	}
}

// TestOnlyChangedTargetsRaiseAnEvent keeps the steady state quiet. Targets are
// reconciled every pass, not only on transitions, so reporting each one would
// bury the two writes that actually happened.
func TestOnlyChangedTargetsRaiseAnEvent(t *testing.T) {
	recorder := &fakeRecorder{}
	r := &AutomationReconciler{Recorder: recorder}
	automation := automationWithCondition("", "")

	zero, one := int32(0), int32(1)
	r.eventsForTargets(automation, []targetOutcome{
		{ref: "Deployment/media/unchanged", effective: &one, changed: false},
		{ref: "Deployment/media/qbittorrent", effective: &zero, changed: true},
		{ref: "Deployment/media/sonarr", effective: nil, changed: true},
	})

	got := recorder.reasons()
	if len(got) != 2 {
		t.Fatalf("expected one Event per write that happened, got %d: %v", len(got), got)
	}
	scaled, ok := recorder.find(reasonTargetScaled)
	if !ok || !strings.Contains(scaled.Note, "qbittorrent") || !strings.Contains(scaled.Note, "0 replicas") {
		t.Errorf("the scale Event does not say what was written: %+v", scaled)
	}
	released, ok := recorder.find(reasonTargetReleased)
	if !ok || !strings.Contains(released.Note, "sonarr") {
		t.Errorf("a released target was not reported: %+v", released)
	}
}

// TestBeingOutvotedIsNotAWarning protects a design decision from being eroded
// by whoever next touches this. Two Automations sharing a workload and one
// losing is the arbitration working; reporting it as a Warning would teach
// people to ignore Warnings on Automations.
func TestBeingOutvotedIsNotAWarning(t *testing.T) {
	recorder := &fakeRecorder{}
	r := &AutomationReconciler{Recorder: recorder}

	automation := automationWithCondition(conditionApplied, "InEffect")
	one := int32(1)
	r.setAppliedCondition(automation, []targetOutcome{{
		ref: "Deployment/media/qbittorrent", desired: &one, effective: new(int32),
		deferredBy: []string{"power/shed-on-battery"},
	}})

	deferred, ok := recorder.find(reasonDeferred)
	if !ok {
		t.Fatalf("no %s Event: %v", reasonDeferred, recorder.reasons())
	}
	if deferred.Type != corev1.EventTypeNormal {
		t.Errorf("being outvoted is reported as %q; it is how sharing a target is meant to work", deferred.Type)
	}
	if !strings.Contains(deferred.Note, "power/shed-on-battery") {
		t.Errorf("the Event does not name who is holding the target: %s", deferred.Note)
	}
}

// TestARecorderlessReconcilerIsSilent covers the path every unit test in this
// package takes, and any deployment where the recorder could not be built.
func TestARecorderlessReconcilerIsSilent(t *testing.T) {
	r := &AutomationReconciler{}
	automation := automationWithCondition("", "")
	// Would panic on a nil recorder if the guard were ever dropped.
	r.event(automation, corev1.EventTypeNormal, reasonStateEntered, actionEvaluate, "nothing to record to")
	r.reportTransition(automation, true, true)
	r.eventsForTargets(automation, []targetOutcome{{ref: "Deployment/media/x", changed: true}})
}

// TestEventsAreWrittenToTheEventsAPIGroup pins down what RBAC has to grant,
// which is not what it looks like.
//
// mgr.GetEventRecorder returns the events.k8s.io/v1 recorder, not the
// deprecated core/v1 one, and those are separate API groups for authorization
// even though they share storage. A rule granting only apiGroups: [""] on
// events therefore fails every emission with a Forbidden that is logged by the
// broadcaster and nowhere else — the Automation simply has no Events, which
// looks exactly like nothing having happened.
//
// This test fails if controller-runtime ever moves back, which would make the
// chart's rule over-broad rather than broken.
func TestEventsAreWrittenToTheEventsAPIGroup(t *testing.T) {
	paths := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case paths <- req.URL.Path:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"kind":"Event","apiVersion":"events.k8s.io/v1","metadata":{"name":"e"}}`))
	}))
	defer server.Close()

	client, err := eventsv1client.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("building the events client: %v", err)
	}

	// Constructed exactly as controller-runtime constructs it in
	// pkg/cluster: an events.k8s.io broadcaster over the EventsV1 client.
	broadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: client})
	if err := broadcaster.StartRecordingToSinkWithContext(t.Context()); err != nil {
		t.Fatalf("starting the broadcaster: %v", err)
	}
	defer broadcaster.Shutdown()

	recorder := broadcaster.NewRecorder(clientgoscheme.Scheme, "automation")
	recorder.Eventf(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "stand-in", Namespace: "media"}},
		nil, corev1.EventTypeWarning, reasonStateKeyUnavailable, actionEvaluate, "note")

	select {
	case path := <-paths:
		if !strings.HasPrefix(path, "/apis/events.k8s.io/v1/") {
			t.Fatalf("Events are written to %q; RBAC must grant that API group, not the one assumed", path)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the broadcaster never wrote the Event")
	}
}
