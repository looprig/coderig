package coderig

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
)

// acceptance_test.go drives the assembled CodeRig headless (over an isolated in-memory
// store + a temp checkout, so it never contends on the real current-checkout lease with
// sibling tests). It proves the composed rig starts with the operator-primary as the durable
// root loop, that a submitted turn is observable on the whole-session event stream, and that
// the agent closes cleanly.
//
// The composed managed-delegation action flows live in managed_delegation_test.go; the
// fresh-fsstore restore and runtime-skill matrix lives in the integration-tagged tests.

// openAcceptanceAgent opens a headless CodeRig session over an isolated store + temp root.
func openAcceptanceAgent(t *testing.T) (*sessionAdapter, *swarmStores) {
	t.Helper()
	stores := mustHeadlessTestStores(t)
	agent, err := newSessionOverStores(context.Background(), &fakeLLM{}, newModelFactoryFor(testModel()), Config{}, stores, t.TempDir())
	if err != nil {
		t.Fatalf("newSessionOverStores() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	return agent, stores
}

// durableRootLoop folds the session's durable log and returns the first zero-parent
// LoopStarted's agent name and loop id — the root primer.
func durableRootLoop(t *testing.T, stores *swarmStores, sessionID uuid.UUID) (string, uuid.UUID) {
	t.Helper()
	replayer, err := stores.session.OpenEventReplayer(sessionID, sessionstore.ReplayRequest{})
	if err != nil {
		t.Fatalf("OpenEventReplayer() error = %v", err)
	}
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{From: journal.Beginning()})
	if err != nil {
		t.Fatalf("replayer.Open() error = %v", err)
	}
	defer func() { _ = cursor.Close() }()
	for {
		ev, _, err := cursor.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return "", uuid.UUID{}
		}
		if err != nil {
			t.Fatalf("cursor.Next() error = %v", err)
		}
		if started, ok := ev.(event.LoopStarted); ok && started.Cause.Coordinates.LoopID.IsZero() {
			return string(started.AgentName), started.LoopID
		}
	}
}

// TestAcceptanceRootLoopIsOperatorPrimary proves the composed rig starts with the
// operator-primary as the durable, zero-parent primer and that it is initially active.
func TestAcceptanceRootLoopIsOperatorPrimary(t *testing.T) {
	t.Parallel()
	agent, stores := openAcceptanceAgent(t)

	name, rootID := durableRootLoop(t, stores, agent.SessionID())
	if name != string(operatorPrimaryName) {
		t.Errorf("durable root loop agent = %q, want %q", name, operatorPrimaryName)
	}
	if rootID.IsZero() {
		t.Fatal("no durable zero-parent root LoopStarted found")
	}
	if got := agent.ActiveLoopID(); got != rootID {
		t.Errorf("ActiveLoopID() = %v, want the active primer %v", got, rootID)
	}
}

// TestAcceptanceSubmitIsObservable proves a submitted turn runs on the composed rig and is
// observable on the one whole-session subscription, then the agent closes cleanly.
func TestAcceptanceSubmitIsObservable(t *testing.T) {
	t.Parallel()
	agent, _ := openAcceptanceAgent(t)

	stream, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = stream.Close() }()

	inputID, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "hello"}})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if inputID.IsZero() {
		t.Error("Submit() returned a zero input id")
	}

	// The turn produces at least one enduring event; a bounded wait guards against a hang.
	select {
	case d, ok := <-stream.Events():
		if !ok {
			t.Fatal("event stream closed before any delivery")
		}
		if d.Event == nil {
			t.Error("received a nil event")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event observed within the deadline after Submit")
	}
}

// TestAcceptanceLoopHandleExposesPrimerModel proves the active loop handle exposes the shared
// model identity the primer was defined with.
func TestAcceptanceLoopHandleExposesPrimerModel(t *testing.T) {
	t.Parallel()
	agent, _ := openAcceptanceAgent(t)

	handle, ok := agent.Controller().Loop(agent.ActiveLoopID())
	if !ok {
		t.Fatal("root loop handle not found")
	}
	if handle.Model().Name != testModel().Name {
		t.Errorf("root loop model = %q, want %q", handle.Model().Name, testModel().Name)
	}
	var _ loop.Handle = handle
}
