//go:build integration

package app

import (
	"context"
	"sync"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/workspacestore"
)

// fakeSub is a benign event.Subscription for adapter construction in integration tests.
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

// fakeController is a minimal session.SessionController used to construct a session adapter
// over a real store whose backlog is read directly (the adapter never drives the controller
// in these replay-only cases). Methods the adapter does not exercise return benign zeros.
type fakeController struct {
	sessionID uuid.UUID
	sub       *fakeSub
}

func (f *fakeController) SessionID() uuid.UUID               { return f.sessionID }
func (f *fakeController) ActiveLoop() loop.Handle            { return nil }
func (f *fakeController) Loop(uuid.UUID) (loop.Handle, bool) { return nil, false }
func (f *fakeController) Submit(context.Context, []content.Block) (uuid.UUID, error) {
	return uuid.UUID{}, nil
}
func (f *fakeController) SubmitToLoop(context.Context, uuid.UUID, []content.Block) (uuid.UUID, error) {
	return uuid.UUID{}, nil
}
func (f *fakeController) Compact(context.Context) (uuid.UUID, error) { return uuid.UUID{}, nil }
func (f *fakeController) CompactToLoop(context.Context, uuid.UUID) (uuid.UUID, error) {
	return uuid.UUID{}, nil
}
func (f *fakeController) SubscribeEvents(event.EventFilter) (event.Subscription, error) {
	return f.sub, nil
}
func (f *fakeController) RespondGate(context.Context, gate.GateResponse) error { return nil }
func (f *fakeController) Interrupt(context.Context) (bool, error)              { return false, nil }
func (f *fakeController) SetActiveLoop(context.Context, uuid.UUID) error       { return nil }
func (f *fakeController) LoopController(uuid.UUID) (loop.Controller, bool)     { return nil, false }
func (f *fakeController) CheckpointWorkspace(context.Context) (workspacestore.Ref, error) {
	return "", nil
}
func (f *fakeController) RestoreWorkspace(context.Context, workspacestore.Ref) error { return nil }
func (f *fakeController) Shutdown(context.Context) error                             { return nil }

var _ session.SessionController = (*fakeController)(nil)
