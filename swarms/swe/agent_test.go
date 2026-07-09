package swe

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/cli/tui"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/llm"
)

// Compile-time proof that *sessionAgent satisfies the TUI's Agent surface (the
// streaming/lifecycle methods plus the Approve/Deny/ProvideAnswer trio). If a
// method's signature drifts this fails to build.
var _ tui.Agent = (*sessionAgent)(nil)

// testPrimaryCfg builds a minimal valid primary loop.Config over a fake client for
// wrapper construction tests: a secret-free model plus a (possibly empty) system prompt,
// mirroring the post-split loop.Config shape.
func testPrimaryCfg(m inference.Model, system string) loop.Config {
	return loop.Config{Client: &fakeLLM{}, Model: m, System: system, Tools: loop.ToolSet{}}
}

// TestNewSessionAgentHappy proves construction over a fake client succeeds and
// yields a non-nil wrapper whose session is releasable via Close.
func TestNewSessionAgentHappy(t *testing.T) {
	t.Parallel()

	a, err := newSessionAgent(context.Background(), testPrimaryCfg(testModel(), ""))
	if err != nil {
		t.Fatalf("newSessionAgent() error = %v", err)
	}
	if a == nil {
		t.Fatal("newSessionAgent() returned nil agent")
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if a.PrimaryLoopID().IsZero() {
		t.Error("PrimaryLoopID() is zero, want a minted loop id")
	}
}

// TestNewSessionAgentPreCancelledCtx proves construction fails fast on an
// already-cancelled caller ctx with the typed session error — even though the
// session itself runs under an agent-owned (background-derived) root, so the
// fail-fast check must be on the CALLER ctx, not the session root.
func TestNewSessionAgentPreCancelledCtx(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a, err := newSessionAgent(ctx, testPrimaryCfg(testModel(), ""))
	if a != nil {
		t.Errorf("expected nil agent, got %v", a)
		_ = a.Close(context.Background())
	}
	var se *session.SessionError
	if !errors.As(err, &se) || se.Kind != session.SessionContextDone {
		t.Fatalf("err = %v, want *session.SessionError{SessionContextDone}", err)
	}
}

// TestSessionAgentCloseIdempotent proves Close releases the session and is safe to
// call more than once (Shutdown blocks until the actor exits; the second call is a
// no-op).
func TestSessionAgentCloseIdempotent(t *testing.T) {
	t.Parallel()

	a, err := newSessionAgent(context.Background(), testPrimaryCfg(testModel(), ""))
	if err != nil {
		t.Fatalf("newSessionAgent() error = %v", err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

// TestSessionAgentAcceptsImages proves AcceptsImages reflects the primary config's
// model modality flag exactly, with no inversion or defaulting.
func TestSessionAgentAcceptsImages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want bool
	}{
		{name: "text-only model", want: false},
		{name: "model accepts images", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := inference.Model{
				Provider:  inference.ProviderName(llm.ProviderLMStudio),
				APIFormat: inference.APIFormatOpenAI,
				BaseURL:   "http://localhost:1234/v1",
				Name:      "fake-model",
				Caps:      inference.Capabilities{AcceptsImages: tt.want},
			}
			a, err := newSessionAgent(context.Background(), testPrimaryCfg(m, ""))
			if err != nil {
				t.Fatalf("newSessionAgent() error = %v", err)
			}
			t.Cleanup(func() { _ = a.Close(context.Background()) })

			if got := a.AcceptsImages(); got != tt.want {
				t.Errorf("AcceptsImages() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSessionAgentReplayBacklogNilForNewSession proves a fresh (non-restored)
// session has no backlog to repaint: ReplayBacklog returns nil/nil, so the TUI
// skips the cold-restore repaint.
func TestSessionAgentReplayBacklogNilForNewSession(t *testing.T) {
	t.Parallel()

	a, err := newSessionAgent(context.Background(), testPrimaryCfg(testModel(), ""))
	if err != nil {
		t.Fatalf("newSessionAgent() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	events, err := a.ReplayBacklog(context.Background())
	if err != nil {
		t.Fatalf("ReplayBacklog() error = %v", err)
	}
	if events != nil {
		t.Errorf("ReplayBacklog() = %v, want nil (new session has no backlog)", events)
	}
}

// TestSessionAgentGateTrioNoOpenGate proves the three gate wrappers — Approve,
// Deny, ProvideAnswer — fail secure with *GateNotOpenError when no open gate matches
// the tool-execution id, rather than silently succeeding. Each wrapper resolves the
// gate.ID via the session's ListGates before responding; a closed session (Closed
// FIRST, so the actor has exited and no gate is open) has no listable gate, so the
// lookup returns *GateNotOpenError for a random call id. The happy-path forwarding to
// RespondGate is covered end-to-end by the acceptance/runtime integration tests.
func TestSessionAgentGateTrioNoOpenGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		invoke func(ctx context.Context, a *sessionAgent) error
	}{
		{
			name: "Approve",
			invoke: func(ctx context.Context, a *sessionAgent) error {
				return a.Approve(ctx, a.PrimaryLoopID(), mustUUID(t), tool.ScopeSession)
			},
		},
		{
			name: "Deny",
			invoke: func(ctx context.Context, a *sessionAgent) error {
				return a.Deny(ctx, a.PrimaryLoopID(), mustUUID(t))
			},
		},
		{
			name: "ProvideAnswer",
			invoke: func(ctx context.Context, a *sessionAgent) error {
				return a.ProvideAnswer(ctx, a.PrimaryLoopID(), mustUUID(t), "the answer")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := newClosedSessionAgentForGateTest(t)

			err := tt.invoke(context.Background(), a)

			var notOpen *GateNotOpenError
			if !errors.As(err, &notOpen) {
				t.Fatalf("err = %v, want *GateNotOpenError", err)
			}
		})
	}
}

// newClosedSessionAgentForGateTest builds a sessionAgent over a fake client and
// Closes it before returning, so a subsequent gate call deterministically routes
// onto the loop-exited path. Close is idempotent, so the cleanup's second Close is
// a safe no-op.
func newClosedSessionAgentForGateTest(t *testing.T) *sessionAgent {
	t.Helper()
	a, err := newSessionAgent(context.Background(), testPrimaryCfg(testModel(), ""))
	if err != nil {
		t.Fatalf("newSessionAgent: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return a
}

// mustUUID mints a UUID for a gate test, failing the test on the crypto/rand error
// path rather than passing a zero ID.
func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return id
}
