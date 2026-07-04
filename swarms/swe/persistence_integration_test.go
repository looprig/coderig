//go:build integration

package swe

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/fsstore"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/transcript"
	"github.com/looprig/swe/agents/operator"
)

// textChunk wraps s as a streamed text chunk for the fake LLM. (The fake_test fakeLLM is
// shared with the non-tagged unit tests; this helper is only used by the persisted
// integration tests, which need to drive a turn to a terminal.)
func textChunk(s string) content.Chunk { return &content.TextChunk{Text: s} }

// newIntegrationFactory opens the production session-store factory over a temp fsstore backend
// (session ledger/journal/blobs + workspace snapshots all under one root) and registers its
// Close. It is the CLI-shaped composition — one on-disk store shared by every session — and the
// seam the integration tests drive with an injected fake client via openWithClient.
func newIntegrationFactory(t *testing.T) *SessionStoreFactory {
	t.Helper()
	fs, err := fsstore.Open(fsstore.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("fsstore.Open: %v", err)
	}
	f, err := newSessionStoreFactory(fs)
	if err != nil {
		_ = fs.Close()
		t.Fatalf("newSessionStoreFactory: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// drainTurn submits input through the persisted agent and drains a fresh subscription to the
// turn terminal — deterministic (unlike a WaitIdle that can race the fire-and-forget submit). The
// subscription is created BEFORE the submit so the terminal is never missed.
func drainTurn(t *testing.T, a *sessionAgent, text string) {
	t.Helper()
	sub, err := a.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := a.Submit(ctx, []content.Block{&content.TextBlock{Text: text}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before a terminal")
			}
			switch ev.(type) {
			case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
				return
			}
		case <-timeout:
			t.Fatal("no terminal within deadline")
		}
	}
}

// drainUntilCheckpoint submits a turn and drains a fresh subscription until the workspace
// checkpoint at quiescence lands (event.WorkspaceCheckpointed, published by CheckpointWorkspace
// after the Active→Idle edge). The subscription is created before the submit so the checkpoint is
// never missed.
func drainUntilCheckpoint(t *testing.T, a *sessionAgent, text string) {
	t.Helper()
	sub, err := a.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := a.Submit(ctx, []content.Block{&content.TextBlock{Text: text}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	timeout := time.After(30 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before a workspace checkpoint")
			}
			if _, ok := ev.(event.WorkspaceCheckpointed); ok {
				return
			}
		case <-timeout:
			t.Fatal("no workspace checkpoint within deadline")
		}
	}
}

// TestNewSessionStoreFactoryOpensAndCloses proves the exported constructor opens a store over an
// explicit data dir, lists an empty store as zero rows, and closes cleanly.
func TestNewSessionStoreFactoryOpensAndCloses(t *testing.T) {
	f, err := NewSessionStoreFactory(t.TempDir())
	if err != nil {
		t.Fatalf("NewSessionStoreFactory: %v", err)
	}
	metas, err := f.List(context.Background())
	if err != nil {
		t.Fatalf("List (empty): %v", err)
	}
	if len(metas) != 0 {
		t.Errorf("fresh store List = %d entries, want 0", len(metas))
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestSessionStoreNewSessionBasics proves openWithClient with a ZERO selector builds a NEW
// persisted session over the shared store: it has a non-zero SessionID (the factory minted +
// injected it) and, being a NEW (not restored) session, ReplayBacklog returns nil so the TUI
// skips the cold-restore repaint.
func TestSessionStoreNewSessionBasics(t *testing.T) {
	f := newIntegrationFactory(t)

	a, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("first reply")}}, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("openWithClient (new): %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if a.SessionID().IsZero() {
		t.Fatal("new persisted session has a zero SessionID")
	}
	if backlog, err := a.ReplayBacklog(context.Background()); err != nil {
		t.Fatalf("new-session ReplayBacklog: %v", err)
	} else if len(backlog) != 0 {
		t.Errorf("new-session ReplayBacklog returned %d events, want 0", len(backlog))
	}
}

// primaryLoopAgentName returns the AgentName the LoopStarted for loopID carries in evs, or
// the empty string if no such LoopStarted is present.
func primaryLoopAgentName(evs []event.Event, loopID uuid.UUID) identity.AgentName {
	for _, e := range evs {
		ls, ok := e.(event.LoopStarted)
		if !ok {
			continue
		}
		if ls.EventHeader().LoopID == loopID {
			return ls.EventHeader().AgentName
		}
	}
	return ""
}

// TestSessionStoreRoundTrip is the headline CLI-shaped wiring smoke: a NEW persisted session
// runs a turn that persists, the agent is Closed (which releases the session lease), and the
// SAME session is RESUMED over the same store. The restored session's ReplayBacklog reproduces
// the committed Enduring events and a fresh turn continues, proving the durable log survived a
// full close/reopen cycle.
func TestSessionStoreRoundTrip(t *testing.T) {
	f := newIntegrationFactory(t)

	a, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("first reply")}}, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("openWithClient (new): %v", err)
	}
	sessionID := a.SessionID()
	if sessionID.IsZero() {
		t.Fatal("new persisted session has a zero SessionID")
	}

	drainTurn(t, a, "hello")

	// A clean Close releases the lease, so a resume can reopen the same session without contending
	// the lease or waiting out its expiry.
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close (original): %v", err)
	}

	a2, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("after restore")}}, newModelFactory(), SessionSelector{Resume: sessionID}, Config{})
	if err != nil {
		t.Fatalf("openWithClient (resume): %v", err)
	}
	t.Cleanup(func() { _ = a2.Close(context.Background()) })

	if a2.SessionID() != sessionID {
		t.Errorf("resumed SessionID = %v, want %v", a2.SessionID(), sessionID)
	}

	backlog, err := a2.ReplayBacklog(context.Background())
	if err != nil {
		t.Fatalf("resumed ReplayBacklog: %v", err)
	}
	if !hasType(backlog, event.TurnStarted{}) {
		t.Errorf("resumed backlog missing TurnStarted: %v", typeNames(backlog))
	}
	if !hasType(backlog, event.RestoreDone{}) {
		t.Errorf("resumed backlog missing RestoreDone (restore was not bracketed): %v", typeNames(backlog))
	}
	if got := primaryLoopAgentName(backlog, a2.PrimaryLoopID()); got != operator.Name {
		t.Errorf("restored primary-loop LoopStarted AgentName = %q, want %q (operator-as-primary)", got, operator.Name)
	}

	drainTurn(t, a2, "continue")
}

// TestSessionStoreWorkspaceRoundTrip is the acceptance test the whole extraction exists for: a
// session's workspace is checkpointed at quiescence (SessionIdle), and a later restore
// materializes it — so both the conversation state AND the workspace files come back. The
// workspace is a temp dir the session snapshots via os.Getwd(); t.Chdir points the process there
// (and forbids t.Parallel for this test).
func TestSessionStoreWorkspaceRoundTrip(t *testing.T) {
	f := newIntegrationFactory(t)

	workspace := t.TempDir()
	t.Chdir(workspace)

	const name = "notes.txt"
	const body = "workspace survived a restore"
	// Written BEFORE the session opens, so it is present in every checkpoint the session takes.
	if err := os.WriteFile(filepath.Join(workspace, name), []byte(body), 0o600); err != nil {
		t.Fatalf("seed workspace file: %v", err)
	}

	a, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("first reply")}}, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("openWithClient (new): %v", err)
	}
	sessionID := a.SessionID()
	drainUntilCheckpoint(t, a, "make a note")
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close (original): %v", err)
	}

	// Destroy the workspace file; a restore must materialize the checkpointed copy back.
	if err := os.Remove(filepath.Join(workspace, name)); err != nil {
		t.Fatalf("remove workspace file: %v", err)
	}

	a2, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("after restore")}}, newModelFactory(), SessionSelector{Resume: sessionID}, Config{})
	if err != nil {
		t.Fatalf("openWithClient (resume): %v", err)
	}
	t.Cleanup(func() { _ = a2.Close(context.Background()) })

	got, err := os.ReadFile(filepath.Join(workspace, name))
	if err != nil {
		t.Fatalf("workspace file not materialized on restore: %v", err)
	}
	if string(got) != body {
		t.Errorf("materialized workspace file = %q, want %q", string(got), body)
	}
}

func TestSessionStoreExportSource(t *testing.T) {
	f := newIntegrationFactory(t)

	tests := []struct {
		name  string
		setup func(t *testing.T) *sessionAgent
	}{
		{
			name: "new persisted session exports full journal stream",
			setup: func(t *testing.T) *sessionAgent {
				t.Helper()
				a, err := f.openWithClient(context.Background(),
					&fakeLLM{chunks: []content.Chunk{textChunk("new reply")}}, newModelFactory(), SessionSelector{}, Config{})
				if err != nil {
					t.Fatalf("openWithClient (new): %v", err)
				}
				return a
			},
		},
		{
			name: "resumed persisted session exports full journal stream",
			setup: func(t *testing.T) *sessionAgent {
				t.Helper()
				a, err := f.openWithClient(context.Background(),
					&fakeLLM{chunks: []content.Chunk{textChunk("before resume")}}, newModelFactory(), SessionSelector{}, Config{})
				if err != nil {
					t.Fatalf("openWithClient (new): %v", err)
				}
				drainTurn(t, a, "seed")
				sessionID := a.SessionID()
				if err := a.Close(context.Background()); err != nil {
					t.Fatalf("Close (original): %v", err)
				}

				resumed, err := f.openWithClient(context.Background(),
					&fakeLLM{chunks: []content.Chunk{textChunk("after resume")}}, newModelFactory(), SessionSelector{Resume: sessionID}, Config{})
				if err != nil {
					t.Fatalf("openWithClient (resume): %v", err)
				}
				return resumed
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.setup(t)
			t.Cleanup(func() { _ = a.Close(context.Background()) })

			src, resolver, err := a.ExportSource(context.Background())
			if err != nil {
				t.Fatalf("ExportSource() error = %v", err)
			}
			if src == nil {
				t.Fatal("ExportSource() source is nil")
			}
			if resolver == nil {
				t.Fatal("ExportSource() resolver is nil")
			}
			prompt, ok := resolver.SystemPrompt(a.PrimaryLoopID())
			if !ok {
				t.Fatal("SystemPrompt(primary) ok = false, want true")
			}
			if !strings.Contains(prompt, "<identity product=\"SWE\">") {
				t.Errorf("SystemPrompt(primary) = %q, want SWE identity prompt", prompt)
			}
			if otherPrompt, otherOK := resolver.SystemPrompt(mustUUID(t)); otherPrompt != "" || otherOK {
				t.Errorf("SystemPrompt(other) = (%q, %v), want (\"\", false)", otherPrompt, otherOK)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			rec, err := src.Next(ctx)
			if err != nil {
				t.Fatalf("source Next() error = %v", err)
			}
			if _, ok := rec.(transcript.EventRecord); !ok {
				t.Fatalf("source Next() record = %T, want transcript.EventRecord", rec)
			}
		})
	}
}

// TestSessionStoreDistinctSessionsCoexist proves two sessions can be active simultaneously over
// the one shared store, each addressed by name, neither contending the other. This is the core
// isolation property, now delivered by session-by-name addressing rather than per-session engines.
func TestSessionStoreDistinctSessionsCoexist(t *testing.T) {
	f := newIntegrationFactory(t)

	a, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("a reply")}}, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("openWithClient (a): %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	b, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("b reply")}}, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("openWithClient (b): %v", err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })

	if a.SessionID() == b.SessionID() {
		t.Fatal("two new sessions share a SessionID")
	}

	// Both sessions accept and complete a turn while simultaneously active.
	drainTurn(t, a, "to a")
	drainTurn(t, b, "to b")
}

// TestSessionStoreListFindsSession proves the store's listing catalog enumerates a session
// created by opening it (the event tap upserts the catalog on SessionStarted).
func TestSessionStoreListFindsSession(t *testing.T) {
	f := newIntegrationFactory(t)

	// An empty store lists nothing.
	if metas, err := f.List(context.Background()); err != nil {
		t.Fatalf("List (empty): %v", err)
	} else if len(metas) != 0 {
		t.Errorf("List on a fresh store = %d entries, want 0", len(metas))
	}

	a, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("reply")}}, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("openWithClient: %v", err)
	}
	sessionID := a.SessionID()
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	metas, err := f.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, m := range metas {
		if m.SessionID == sessionID {
			found = true
		}
	}
	if !found {
		t.Errorf("List did not include the open session %v: %+v", sessionID, metas)
	}
}

// hasType reports whether evs contains an event of the same concrete type as want.
func hasType(evs []event.Event, want event.Event) bool {
	wt := reflect.TypeOf(want)
	for _, e := range evs {
		if reflect.TypeOf(e) == wt {
			return true
		}
	}
	return false
}

func typeNames(evs []event.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = reflect.TypeOf(e).String()
	}
	return out
}
