package swe

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/cli/tui"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/inference"
)

// Compile-time proof that *sessionAgent satisfies the CLI's tui.Agent surface.
var _ tui.Agent = (*sessionAgent)(nil)

// errNoReplay is the sentinel a new-session fake replay opener returns; newSessionAgent
// ignores it for a NEW session (only a restore requires the replayer).
var errNoReplay = errors.New("no replay for a new session")

// fakeReplayOpener returns no replayer, exercising the new-session path (rootLoopID from the
// active loop; ReplayBacklog nil).
type fakeReplayOpener struct{}

func (fakeReplayOpener) OpenEventReplayer(uuid.UUID, sessionstore.ReplayRequest) (journal.EventReplayer, error) {
	return nil, errNoReplay
}

// fakeHandle is a minimal loop.Handle: an id + a model whose caps drive AcceptsImages.
type fakeHandle struct {
	id    uuid.UUID
	model inference.Model
}

func (h *fakeHandle) ID() uuid.UUID          { return h.id }
func (h *fakeHandle) Mode() loop.ModeName    { return "" }
func (h *fakeHandle) Model() inference.Model { return h.model }

// fakeSub is a controllable event.Subscription: the test feeds deliveries on ch.
type fakeSub struct {
	ch   chan event.Delivery
	once sync.Once
}

func newFakeSub() *fakeSub { return &fakeSub{ch: make(chan event.Delivery, 8)} }

func (s *fakeSub) Events() <-chan event.Delivery { return s.ch }
func (s *fakeSub) Close() error {
	s.once.Do(func() { close(s.ch) })
	return nil
}
func (s *fakeSub) Err() error { return nil }

// fakeController is a fake session.SessionController for the adapter tests: it records gate
// responses + shutdowns and returns configured loops. The methods the adapter never exercises
// return benign zero values.
type fakeController struct {
	sessionID uuid.UUID
	active    *fakeHandle
	loops     map[uuid.UUID]*fakeHandle
	sub       *fakeSub

	mu        sync.Mutex
	gotGate   []gate.GateResponse
	shutdowns int
}

func (f *fakeController) SessionID() uuid.UUID    { return f.sessionID }
func (f *fakeController) ActiveLoop() loop.Handle { return f.active }
func (f *fakeController) Loop(id uuid.UUID) (loop.Handle, bool) {
	h, ok := f.loops[id]
	if !ok {
		return nil, false
	}
	return h, true
}
func (f *fakeController) Submit(context.Context, []content.Block) (uuid.UUID, error) {
	return uuid.UUID{}, nil
}
func (f *fakeController) SubmitToLoop(context.Context, uuid.UUID, []content.Block) (uuid.UUID, error) {
	return uuid.UUID{}, nil
}
func (f *fakeController) SubscribeEvents(event.EventFilter) (event.Subscription, error) {
	return f.sub, nil
}
func (f *fakeController) RespondGate(_ context.Context, r gate.GateResponse) error {
	f.mu.Lock()
	f.gotGate = append(f.gotGate, r)
	f.mu.Unlock()
	return nil
}
func (f *fakeController) Interrupt(context.Context) (bool, error) { return false, nil }
func (f *fakeController) SetActiveLoop(_ context.Context, id uuid.UUID) error {
	if h, ok := f.loops[id]; ok {
		f.active = h
		return nil
	}
	return errNoReplay
}
func (f *fakeController) LoopController(uuid.UUID) (loop.Controller, bool)        { return nil, false }
func (f *fakeController) SetSecurityCeiling(context.Context, ceiling.Level) error { return nil }
func (f *fakeController) CheckpointWorkspace(context.Context) (workspacestore.Ref, error) {
	return "", nil
}
func (f *fakeController) RestoreWorkspace(context.Context, workspacestore.Ref) error { return nil }
func (f *fakeController) Shutdown(context.Context) error {
	f.mu.Lock()
	f.shutdowns++
	f.mu.Unlock()
	return nil
}

func (f *fakeController) responses() []gate.GateResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gate.GateResponse(nil), f.gotGate...)
}

// mustUUID mints a uuid or fails the test.
func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}

// newFakeAgent builds a *sessionAgent over a fakeController with two loops (active + one
// other), exercising the NEW-session construction path.
func newFakeAgent(t *testing.T) (*sessionAgent, *fakeController, uuid.UUID, uuid.UUID) {
	t.Helper()
	rootID := mustUUID(t)
	otherID := mustUUID(t)
	imageModel := testModel()
	imageModel.Caps.AcceptsImages = true
	fc := &fakeController{
		sessionID: mustUUID(t),
		active:    &fakeHandle{id: rootID, model: testModel()},
		sub:       newFakeSub(),
		loops: map[uuid.UUID]*fakeHandle{
			rootID:  {id: rootID, model: testModel()},
			otherID: {id: otherID, model: imageModel},
		},
	}
	a, err := newSessionAgent(context.Background(), fc, fakeReplayOpener{}, false)
	if err != nil {
		t.Fatalf("newSessionAgent() error = %v", err)
	}
	return a, fc, rootID, otherID
}

// gateOpened fabricates a GateOpened for loopID with the given tool-execution id and gate id.
func gateOpened(loopID, toolExecID, gateID uuid.UUID) event.GateOpened {
	return event.GateOpened{
		Header: event.Header{Coordinates: identity.Coordinates{LoopID: loopID}},
		Gate:   gate.Gate{ID: gateID, Subject: gate.Subject{ToolExecutionID: toolExecID}},
	}
}

// TestRootLoopIDFromActiveOnNew proves a NEW session's RootLoopID is the active primer's loop
// id, captured at construction and stable across a later active-loop change.
func TestRootLoopIDFromActiveOnNew(t *testing.T) {
	t.Parallel()
	a, fc, rootID, otherID := newFakeAgent(t)

	if got := a.RootLoopID(); got != rootID {
		t.Fatalf("RootLoopID() = %v, want the active primer %v", got, rootID)
	}
	if err := fc.SetActiveLoop(context.Background(), otherID); err != nil {
		t.Fatalf("SetActiveLoop error = %v", err)
	}
	if got := a.ActiveLoopID(); got != otherID {
		t.Errorf("ActiveLoopID() = %v, want %v after switch", got, otherID)
	}
	if got := a.RootLoopID(); got != rootID {
		t.Errorf("RootLoopID() = %v, want it stable at %v across active change", got, rootID)
	}
}

// TestAcceptsImagesPerLoopFailsClosed proves AcceptsImages queries the CURRENT model of the
// named loop and fails closed for an unknown loop.
func TestAcceptsImagesPerLoopFailsClosed(t *testing.T) {
	t.Parallel()
	a, _, rootID, imageLoop := newFakeAgent(t)

	tests := []struct {
		name string
		loop uuid.UUID
		want bool
	}{
		{name: "text-only loop", loop: rootID, want: false},
		{name: "image-capable loop", loop: imageLoop, want: true},
		{name: "unknown loop fails closed", loop: mustUUID(t), want: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := a.AcceptsImages(tt.loop); got != tt.want {
				t.Errorf("AcceptsImages(%v) = %v, want %v", tt.loop, got, tt.want)
			}
		})
	}
}

// TestGateIndexResolvesPerLoopAndCall proves the (loopID, toolExecutionID)→GateID index: the
// SAME tool-execution id in two loops resolves to two DISTINCT gate ids, an unknown (loop,
// call) fails secure with *GateNotOpenError, and a GateResolved removes the entry.
func TestGateIndexResolvesPerLoopAndCall(t *testing.T) {
	t.Parallel()
	a, fc, loop1, loop2 := newFakeAgent(t)

	call := mustUUID(t) // the SAME tool-execution id in both loops
	gate1 := mustUUID(t)
	gate2 := mustUUID(t)
	a.foldGate(gateOpened(loop1, call, gate1))
	a.foldGate(gateOpened(loop2, call, gate2))

	if err := a.Approve(context.Background(), loop1, call, tool.ScopeOnce); err != nil {
		t.Fatalf("Approve(loop1) error = %v", err)
	}
	if err := a.Approve(context.Background(), loop2, call, tool.ScopeOnce); err != nil {
		t.Fatalf("Approve(loop2) error = %v", err)
	}
	got := fc.responses()
	if len(got) != 2 {
		t.Fatalf("RespondGate called %d times, want 2", len(got))
	}
	if got[0].GateID != gate1 || got[1].GateID != gate2 {
		t.Errorf("gate ids resolved = %v,%v, want %v,%v (same call in two loops keys two gates)", got[0].GateID, got[1].GateID, gate1, gate2)
	}
	if got[0].Action != "approve" {
		t.Errorf("Action = %q, want approve", got[0].Action)
	}

	// An unknown (loop, call) fails secure.
	var notOpen *GateNotOpenError
	if err := a.Deny(context.Background(), mustUUID(t), call); !errors.As(err, &notOpen) {
		t.Errorf("Deny(unknown loop) error = %v, want *GateNotOpenError", err)
	}

	// A GateResolved removes the entry: a later Approve fails secure.
	a.foldGate(event.GateResolved{GateID: gate1})
	if err := a.Approve(context.Background(), loop1, call, tool.ScopeOnce); !errors.As(err, &notOpen) {
		t.Errorf("Approve after GateResolved error = %v, want *GateNotOpenError", err)
	}
}

// TestSubscribeFoldsGatesBeforeForwarding proves Subscribe returns one wrapping subscription
// that folds a GateOpened into the index BEFORE forwarding the event, and that Close tears the
// forwarder down cleanly.
func TestSubscribeFoldsGatesBeforeForwarding(t *testing.T) {
	t.Parallel()
	a, fc, loop1, _ := newFakeAgent(t)

	stream, err := a.Subscribe(event.EventFilter{})
	if err != nil {
		t.Fatalf("Subscribe error = %v", err)
	}

	call := mustUUID(t)
	gateID := mustUUID(t)
	fc.sub.ch <- event.Delivery{Event: gateOpened(loop1, call, gateID)}

	// Receiving the forwarded delivery guarantees the fold ran first (fold-before-forward).
	select {
	case d := <-stream.Events():
		if _, ok := d.Event.(event.GateOpened); !ok {
			t.Fatalf("forwarded event type = %T, want GateOpened", d.Event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("never received the forwarded GateOpened")
	}

	// The index was updated before the event surfaced: Approve resolves the folded gate.
	if err := a.Approve(context.Background(), loop1, call, tool.ScopeOnce); err != nil {
		t.Fatalf("Approve after fold error = %v", err)
	}
	if got := fc.responses(); len(got) != 1 || got[0].GateID != gateID {
		t.Errorf("RespondGate = %+v, want one response with gate %v", got, gateID)
	}

	if err := stream.Close(); err != nil {
		t.Errorf("stream Close error = %v", err)
	}
}

// TestReplayBacklogNilForNewSession proves a NEW session has no restore backlog.
func TestReplayBacklogNilForNewSession(t *testing.T) {
	t.Parallel()
	a, _, _, _ := newFakeAgent(t)
	backlog, err := a.ReplayBacklog(context.Background())
	if err != nil {
		t.Fatalf("ReplayBacklog error = %v", err)
	}
	if backlog != nil {
		t.Errorf("ReplayBacklog() = %v, want nil for a new session", backlog)
	}
}

// TestCloseShutsDownOnce proves Close is idempotent: it Shuts the session down exactly once.
func TestCloseShutsDownOnce(t *testing.T) {
	t.Parallel()
	a, fc, _, _ := newFakeAgent(t)
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("first Close error = %v", err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Errorf("second Close error = %v", err)
	}
	fc.mu.Lock()
	got := fc.shutdowns
	fc.mu.Unlock()
	if got != 1 {
		t.Errorf("Shutdown called %d times, want 1 (idempotent Close)", got)
	}
}
