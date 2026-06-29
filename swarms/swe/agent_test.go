package swe

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/journal"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/session"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/transcript"
	"github.com/ciram-co/looprig/pkg/transcript/journalsource"
	"github.com/ciram-co/looprig/pkg/tui"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// Compile-time proof that *sessionAgent satisfies the TUI's Agent surface (the
// streaming/lifecycle methods plus the Approve/Deny/ProvideAnswer trio). If a
// method's signature drifts this fails to build.
var _ tui.Agent = (*sessionAgent)(nil)

// testPrimaryCfg builds a minimal valid primary loop.Config over a fake client for
// wrapper construction tests.
func testPrimaryCfg(spec llm.ModelSpec) loop.Config {
	return loop.Config{Client: &fakeLLM{}, Model: spec, Tools: loop.ToolSet{}}
}

// TestNewSessionAgentHappy proves construction over a fake client succeeds and
// yields a non-nil wrapper whose session is releasable via Close.
func TestNewSessionAgentHappy(t *testing.T) {
	t.Parallel()

	a, err := newSessionAgent(context.Background(), testPrimaryCfg(testSpec()))
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

	a, err := newSessionAgent(ctx, testPrimaryCfg(testSpec()))
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

	a, err := newSessionAgent(context.Background(), testPrimaryCfg(testSpec()))
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

			spec := llm.ModelSpec{Provider: llm.ProviderLMStudio, Model: "fake-model", AcceptsImages: tt.want}
			a, err := newSessionAgent(context.Background(), testPrimaryCfg(spec))
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

	a, err := newSessionAgent(context.Background(), testPrimaryCfg(testSpec()))
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

func TestSessionAgentExportSource(t *testing.T) {
	t.Parallel()

	sessionID := mustUUID(t)
	primaryLoopID := mustUUID(t)
	otherLoopID := mustUUID(t)
	const primarySystemPrompt = "primary system prompt"

	tests := []struct {
		name          string
		agent         *sessionAgent
		wantErr       bool
		wantOpen      bool
		wantPrompt    string
		wantPromptOK  bool
		wantOtherOK   bool
		wantSessionID uuid.UUID
	}{
		{
			name:    "in-memory agent returns export unavailable",
			agent:   &sessionAgent{},
			wantErr: true,
		},
		{
			name: "journal-backed agent returns source and primary-only prompt resolver",
			agent: &sessionAgent{
				recordReplayer:      &fakeRecordReplayer{records: []journal.JournalRecord{journal.NewEventRecord(event.SessionStarted{})}},
				exportSessionID:     sessionID,
				primaryLoopID:       primaryLoopID,
				primarySystemPrompt: primarySystemPrompt,
			},
			wantOpen:      true,
			wantPrompt:    primarySystemPrompt,
			wantPromptOK:  true,
			wantOtherOK:   false,
			wantSessionID: sessionID,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src, resolver, err := tt.agent.ExportSource(context.Background())

			if tt.wantErr {
				var unavailable *journalsource.ExportUnavailableError
				if !errors.As(err, &unavailable) {
					t.Fatalf("ExportSource() error = %v, want *journalsource.ExportUnavailableError", err)
				}
				if src != nil || resolver != nil {
					t.Fatalf("ExportSource() returned src=%v resolver=%v with unavailable error", src, resolver)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExportSource() error = %v", err)
			}
			if src == nil {
				t.Fatal("ExportSource() source is nil")
			}
			if resolver == nil {
				t.Fatal("ExportSource() resolver is nil")
			}

			gotPrompt, gotOK := resolver.SystemPrompt(primaryLoopID)
			if gotPrompt != tt.wantPrompt || gotOK != tt.wantPromptOK {
				t.Errorf("SystemPrompt(primary) = (%q, %v), want (%q, %v)", gotPrompt, gotOK, tt.wantPrompt, tt.wantPromptOK)
			}
			if gotOther, gotOtherOK := resolver.SystemPrompt(otherLoopID); gotOther != "" || gotOtherOK != tt.wantOtherOK {
				t.Errorf("SystemPrompt(other) = (%q, %v), want (\"\", %v)", gotOther, gotOtherOK, tt.wantOtherOK)
			}

			rec, err := src.Next(context.Background())
			if err != nil {
				t.Fatalf("source Next() error = %v", err)
			}
			if _, ok := rec.(transcript.EventRecord); !ok {
				t.Fatalf("source Next() record = %T, want transcript.EventRecord", rec)
			}
			rr, ok := tt.agent.recordReplayer.(*fakeRecordReplayer)
			if !ok {
				t.Fatalf("recordReplayer = %T, want *fakeRecordReplayer", tt.agent.recordReplayer)
			}
			if rr.opens != 1 {
				t.Errorf("recordReplayer opens = %d, want 1", rr.opens)
			}
			if rr.req.SessionID != tt.wantSessionID {
				t.Errorf("ReplayRequest.SessionID = %v, want %v", rr.req.SessionID, tt.wantSessionID)
			}
			if !rr.req.LoopID.IsZero() {
				t.Errorf("ReplayRequest.LoopID = %v, want zero for all loops", rr.req.LoopID)
			}
			if rr.req.From != journal.Beginning() {
				t.Errorf("ReplayRequest.From = %#v, want Beginning", rr.req.From)
			}
			if rr.req.Follow {
				t.Error("ReplayRequest.Follow = true, want false")
			}
		})
	}
}

// TestSessionAgentGateTrioDelegatesToSession proves the three gate wrappers —
// Approve, Deny, ProvideAnswer — each forward to the underlying session rather
// than short-circuiting locally. The proof is deterministic: Close FIRST (blocks
// until the actor exits and loop.Done is closed), then each call deterministically
// routes onto the loop-exited path inside the session.
func TestSessionAgentGateTrioDelegatesToSession(t *testing.T) {
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

			var se *session.SessionError
			if !errors.As(err, &se) || se.Kind != session.SessionLoopExited {
				t.Fatalf("err = %v, want *session.SessionError{SessionLoopExited}", err)
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
	a, err := newSessionAgent(context.Background(), testPrimaryCfg(testSpec()))
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

type fakeRecordReplayer struct {
	records []journal.JournalRecord
	req     journal.ReplayRequest
	opens   int
}

func (f *fakeRecordReplayer) Open(_ context.Context, req journal.ReplayRequest) (journal.RecordCursor, error) {
	f.opens++
	f.req = req
	return &fakeRecordCursor{records: f.records}, nil
}

type fakeRecordCursor struct {
	records []journal.JournalRecord
	index   int
}

func (c *fakeRecordCursor) Next(context.Context) (journal.JournalRecord, uint64, error) {
	if c.index >= len(c.records) {
		return nil, 0, io.EOF
	}
	rec := c.records[c.index]
	c.index++
	return rec, uint64(c.index), nil
}

func (c *fakeRecordCursor) Close() error { return nil }
