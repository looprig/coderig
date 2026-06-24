//go:build integration

package swe

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/persistence"
	"github.com/ciram-co/looprig/pkg/uuid"
	"github.com/ciram-co/swe/agents/orchestrator"
)

// textChunk wraps s as a streamed text chunk for the fake LLM. (The fake_test fakeLLM is
// shared with the non-tagged unit tests; this helper is only used by the persisted
// integration tests, which need to drive a turn to a terminal.)
func textChunk(s string) content.Chunk { return &content.TextChunk{Text: s} }

// newIntegrationFactory points the data root at a temp XDG home and returns the production
// session-store factory over it: real isolated per-session engines, opened on demand. It is
// the CLI-shaped composition — each session gets its own embedded StoreDir under sessions/.
func newIntegrationFactory(t *testing.T) *SessionStoreFactory {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root, err := persistence.OpenSessionStoreRoot()
	if err != nil {
		t.Fatalf("OpenSessionStoreRoot: %v", err)
	}
	return newSessionStoreFactory(root)
}

// drainTurn submits input through the persisted agent and drains a fresh subscription to
// the turn terminal — deterministic (unlike a WaitIdle that can race the fire-and-forget
// submit). The subscription is created BEFORE the submit so the terminal is never missed.
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

// TestSessionStoreNewSessionBasics proves openWithClient with a ZERO selector builds a NEW
// persisted session over its own isolated engine: it has a non-zero SessionID (the factory
// minted + injected it) and, being a NEW (not restored) session, ReplayBacklog returns nil
// so the TUI skips the cold-restore repaint.
func TestSessionStoreNewSessionBasics(t *testing.T) {
	f := newIntegrationFactory(t)

	a, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("first reply")}}, newModelFactory("test-key"), SessionSelector{}, Config{})
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

// TestSessionStoreRoundTrip is the headline CLI-shaped wiring smoke over isolated engines: a
// NEW persisted session runs a turn that persists, the agent is Closed (which releases the
// session lock and closes its engine), and the SAME session is RESUMED — opening a fresh
// isolated engine on the SAME StoreDir. The restored session's ReplayBacklog reproduces the
// committed Enduring events and a fresh turn continues, proving the durable log survived a
// full close/reopen cycle on its own StoreDir.
func TestSessionStoreRoundTrip(t *testing.T) {
	f := newIntegrationFactory(t)

	a, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("first reply")}}, newModelFactory("test-key"), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("openWithClient (new): %v", err)
	}
	sessionID := a.SessionID()
	if sessionID.IsZero() {
		t.Fatal("new persisted session has a zero SessionID")
	}

	drainTurn(t, a, "hello")

	// A clean Close releases the lock + closes the engine, so a resume can reopen the same
	// session directory without contending the lock or waiting out the lease TTL.
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close (original): %v", err)
	}

	a2, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("after restore")}}, newModelFactory("test-key"), SessionSelector{Resume: sessionID}, Config{})
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
	if got := primaryLoopAgentName(backlog, a2.PrimaryLoopID()); got != orchestrator.Name {
		t.Errorf("restored primary-loop LoopStarted AgentName = %q, want %q (orchestrator-as-primary)", got, orchestrator.Name)
	}

	drainTurn(t, a2, "continue")
}

// TestSessionStoreDistinctSessionsCoexist proves two sessions can be active simultaneously,
// each over its own isolated engine + StoreDir, neither contending the other. This is the
// core isolation property the feature delivers.
func TestSessionStoreDistinctSessionsCoexist(t *testing.T) {
	f := newIntegrationFactory(t)

	a, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("a reply")}}, newModelFactory("test-key"), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("openWithClient (a): %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	b, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("b reply")}}, newModelFactory("test-key"), SessionSelector{}, Config{})
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

// TestSessionStoreListFindsSession proves the engine-free filesystem List enumerates a
// session directory created by opening a session.
func TestSessionStoreListFindsSession(t *testing.T) {
	f := newIntegrationFactory(t)

	// An empty store lists nothing.
	if entries, err := f.List(); err != nil {
		t.Fatalf("List (empty): %v", err)
	} else if len(entries) != 0 {
		t.Errorf("List on a fresh store = %d entries, want 0", len(entries))
	}

	a, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("reply")}}, newModelFactory("test-key"), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("openWithClient: %v", err)
	}
	sessionID := a.SessionID()
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	entries, err := f.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Meta.ID == sessionID {
			found = true
		}
	}
	if !found {
		t.Errorf("List did not include the open session %v: %+v", sessionID, entries)
	}
}

// TestPersistentTitleGenerated proves the end-to-end titling wiring: opening a session with
// an Economy model configured, running one turn writes a generated title to the manifest
// (replacing the synchronous first-user-message fallback). The Economy provider is LM Studio
// (no key) so the wiring needs no credential.
func TestPersistentTitleGenerated(t *testing.T) {
	f := newIntegrationFactory(t)
	cfg := Config{ModelCatalog: ModelCatalog{
		Economy: []llm.ModelSpec{{Provider: llm.ProviderLMStudio, Model: "title-model"}},
	}}

	a, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("Add upload retries")}}, newModelFactory("test-key"),
		SessionSelector{}, cfg)
	if err != nil {
		t.Fatalf("openWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	id := a.SessionID()

	drainTurn(t, a, "make uploads retry on failure")

	// The fallback lands synchronously on TurnStarted; the generated title lands
	// asynchronously after TurnDone. Poll the manifest until the generated title appears.
	store, err := f.root.OpenSessionMeta(id)
	if err != nil {
		t.Fatalf("OpenSessionMeta: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var meta persistence.SessionMeta
	for time.Now().Before(deadline) {
		meta, err = store.Read()
		if err == nil && meta.TitleSource == persistence.TitleSourceGenerated {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if meta.TitleSource != persistence.TitleSourceGenerated {
		t.Fatalf("title source = %q, want generated (meta %+v, err %v)", meta.TitleSource, meta, err)
	}
	if meta.Title == "" {
		t.Error("generated title is empty")
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
