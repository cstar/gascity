package main

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

func TestWorkflowScopedWake(t *testing.T) {
	payload := func(scope string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"id": "step", "metadata": map[string]string{"gc.root_store_ref": scope}})
		return b
	}
	for _, tt := range []struct {
		name, scope string
		want        bool
	}{
		{"own", "rig:hatch", true}, {"foreign", "rig:web", false}, {"unknown", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := events.Event{Type: events.BeadUpdated, Subject: "step", Payload: payload(tt.scope)}
			if got := workflowEventForRig(e, "hatch", nil); got != tt.want {
				t.Fatalf("wake=%v want %v", got, tt.want)
			}
		})
	}
}

func TestWorkflowScopedWakeKnownRun(t *testing.T) {
	e := events.Event{Type: events.BeadClosed, RunID: "shared", Payload: json.RawMessage(`{"id":"step","metadata":{"gc.root_store_ref":"rig:web"}}`)}
	if !workflowEventForRig(e, "hatch", map[string]bool{"shared": true}) {
		t.Fatal("known cross-rig run not woken")
	}
}

func TestWorkflowScopedWakeCityDispatcher(t *testing.T) {
	if !workflowEventForRig(events.Event{Type: events.BeadClosed}, "", nil) {
		t.Fatal("city dispatcher must retain global scope")
	}
}

func TestWorkflowScopedWakeLearnsCrossRigDependency(t *testing.T) {
	filter := workflowScopedEventFilter("hatch/dispatcher")
	if !filter(events.Event{Type: events.BeadUpdated, Subject: "own", Payload: json.RawMessage(`{"id":"own","metadata":{"gc.root_store_ref":"rig:hatch"},"dependencies":[{"depends_on_id":"foreign"}]}`)}) {
		t.Fatal("own event rejected")
	}
	if !filter(events.Event{Type: events.BeadClosed, Subject: "foreign"}) {
		t.Fatal("known dependency ignored")
	}
	if filter(events.Event{Type: events.BeadUpdated, Subject: "unrelated"}) {
		t.Fatal("unrelated event reset idle backoff")
	}
}
