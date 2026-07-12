package swe

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/looprig/inference"
	"github.com/looprig/storage/memstore"
)

// mustHeadlessTestStores opens an ISOLATED in-memory store (NOT the process-shared headless
// singleton) so a session-opening unit test never contends on the real current-checkout root
// lease with sibling tests. Each caller gets its own leaser, so its exclusive-root lease is
// private to the test.
func mustHeadlessTestStores(t *testing.T) *swarmStores {
	t.Helper()
	stores, err := openStores(memstore.New())
	if err != nil {
		t.Fatalf("openStores(memstore) error = %v", err)
	}
	return stores
}

// TestExclusiveCheckoutContentionAndHandoff proves the Phase-B exclusive-workspace invariant:
// two sessions over the SAME store + SAME checkout root contend on the exclusive root lease —
// the second cannot open while the first holds it — and once the first is Closed the lease is
// released so a third session opens cleanly (release/handoff). This is the mechanism that
// makes two headless sessions contend on the shared current checkout.
func TestExclusiveCheckoutContentionAndHandoff(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	stores := mustHeadlessTestStores(t)
	root := t.TempDir()

	first, err := newSessionOverStores(ctx, &fakeLLM{}, newModelFactory(), Config{}, stores, root)
	if err != nil {
		t.Fatalf("first session open error = %v", err)
	}

	// The second open on the SAME root must not proceed while the first holds the lease.
	// A bounded context ensures the test fails loud rather than hanging if the backend
	// blocks instead of failing fast — either way the second session cannot open.
	blockedCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	second, err := newSessionOverStores(blockedCtx, &fakeLLM{}, newModelFactory(), Config{}, stores, root)
	cancel()
	if err == nil {
		_ = second.Close(ctx)
		_ = first.Close(ctx)
		t.Fatal("second session opened while the first held the exclusive root lease, want a contention error")
	}

	// Release the first session's lease; a third session then opens (handoff).
	if err := first.Close(ctx); err != nil {
		t.Fatalf("first session Close error = %v", err)
	}
	third, err := newSessionOverStores(ctx, &fakeLLM{}, newModelFactory(), Config{}, stores, root)
	if err != nil {
		t.Fatalf("third session open after handoff error = %v", err)
	}
	if err := third.Close(ctx); err != nil {
		t.Fatalf("third session Close error = %v", err)
	}
}

// TestHeadlessNewAndRestoreRoundTrip proves a session opened over an isolated store can be
// Shutdown and RESTORED by id over the SAME store (the rig owns new + restore), and that the
// restored session's active loop id matches the original — parity for the headless rig builder.
func TestHeadlessNewAndRestoreRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	stores := mustHeadlessTestStores(t)
	root := t.TempDir()

	first, err := newSessionOverStores(ctx, &fakeLLM{}, newModelFactory(), Config{}, stores, root)
	if err != nil {
		t.Fatalf("new session error = %v", err)
	}
	id := first.SessionID()
	activeLoop := first.ActiveLoopID()
	if id.IsZero() || activeLoop.IsZero() {
		t.Fatalf("new session id/active loop zero: id=%v active=%v", id, activeLoop)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatalf("Close error = %v", err)
	}

	definitions, err := swarmDefinitions(&fakeLLM{}, newModelFactory()(), Config{})
	if err != nil {
		t.Fatalf("swarmDefinitions error = %v", err)
	}
	assembly, err := buildRig(definitions, stores, root, Config{}, false)
	if err != nil {
		t.Fatalf("buildRig error = %v", err)
	}
	controller, err := assembly.RestoreSession(ctx, id)
	if err != nil {
		t.Fatalf("RestoreSession error = %v", err)
	}
	restored, err := newSessionAgent(ctx, controller, stores.session, true)
	if err != nil {
		t.Fatalf("newSessionAgent(restore) error = %v", err)
	}
	t.Cleanup(func() { _ = restored.Close(ctx) })

	if restored.SessionID() != id {
		t.Errorf("restored SessionID = %v, want %v", restored.SessionID(), id)
	}
	if restored.ActiveLoopID() != activeLoop {
		t.Errorf("restored ActiveLoopID = %v, want %v", restored.ActiveLoopID(), activeLoop)
	}
}

// TestDefaultDataDir proves the default store root is the ~/.looprig/store path the CLI falls
// back to when --data-dir is unset.
func TestDefaultDataDir(t *testing.T) {
	t.Parallel()

	got, err := DefaultDataDir()
	if err != nil {
		t.Fatalf("DefaultDataDir: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory available: %v", err)
	}
	if want := filepath.Join(home, ".looprig", "store"); got != want {
		t.Errorf("DefaultDataDir() = %q, want %q", got, want)
	}
}

// TestNewSessionStoreFactoryLifecycle proves the persisted factory opens over an on-disk store
// and closes cleanly, and that List starts empty (no sessions until one is opened).
func TestNewSessionStoreFactoryLifecycle(t *testing.T) {
	t.Parallel()

	f, err := NewSessionStoreFactory(t.TempDir())
	if err != nil {
		t.Fatalf("NewSessionStoreFactory error = %v", err)
	}
	metas, err := f.List(context.Background())
	if err != nil {
		t.Fatalf("List error = %v", err)
	}
	if len(metas) != 0 {
		t.Errorf("List() = %d sessions, want 0 for a fresh store", len(metas))
	}
	if err := f.Close(); err != nil {
		t.Errorf("Close error = %v", err)
	}
}

// ModelFactory is a plain func type; this compile-time assertion documents its shape: it
// yields the swarm's shared, secret-free inference.Model identity (no system, no secret).
var _ ModelFactory = func() inference.Model {
	return inference.Model{}
}
