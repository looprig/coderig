package swe

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/harness/pkg/uuid"
	"github.com/nats-io/nats.go"
)

// fakeSessionEngine is a no-NATS sessionEngine that records whether Close ran.
type fakeSessionEngine struct{ closed bool }

func (f *fakeSessionEngine) JetStream() nats.JetStreamContext { return nil }
func (f *fakeSessionEngine) Close() error                     { f.closed = true; return nil }

// fakeEngineOpener records the id it was asked to open and returns a fakeSessionEngine, so
// the factory's id-resolution + teardown wiring can be tested without starting NATS.
type fakeEngineOpener struct {
	lastID uuid.UUID
	calls  int
	engine *fakeSessionEngine
	err    error
}

func (o *fakeEngineOpener) OpenSessionEngine(id uuid.UUID) (sessionEngine, error) {
	o.calls++
	o.lastID = id
	if o.err != nil {
		return nil, o.err
	}
	o.engine = &fakeSessionEngine{}
	return o.engine, nil
}

// buildRecord captures what the agent builder was constructed with.
type buildRecord struct {
	id    uuid.UUID
	isNew bool
	calls int
}

// newFakeFactory builds a factory whose engine opener and agent builder avoid NATS: the
// builder returns a headless agent (so Close works without a journal) and records the id +
// isNew it was invoked with.
func newFakeFactory(opener *fakeEngineOpener) (*SessionStoreFactory, *buildRecord) {
	rec := &buildRecord{}
	f := &SessionStoreFactory{
		opener:      opener,
		buildClient: func(ModelCatalog) (llm.LLM, ModelFactory, error) { return &fakeLLM{}, newModelFactory(), nil },
		build: func(ctx context.Context, js nats.JetStreamContext, client llm.LLM, factory ModelFactory, id uuid.UUID, isNew bool, sel SessionSelector, cfg Config) (*sessionAgent, error) {
			rec.calls++
			rec.id = id
			rec.isNew = isNew
			return newSessionAgent(ctx, testPrimaryCfg(testModel(), ""))
		},
	}
	return f, rec
}

// TestSessionStoreFactoryNewMintsIDBeforeEngine proves a new open mints a non-zero session
// id and opens the engine for THAT id, before the agent (journal) is constructed.
func TestSessionStoreFactoryNewMintsIDBeforeEngine(t *testing.T) {
	opener := &fakeEngineOpener{}
	f, rec := newFakeFactory(opener)

	a, err := f.Open(context.Background(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if opener.calls != 1 {
		t.Fatalf("engine opened %d times, want 1", opener.calls)
	}
	if opener.lastID.IsZero() {
		t.Error("engine opened with a zero id (id was not minted before engine construction)")
	}
	if !rec.isNew {
		t.Error("builder isNew = false, want true for a new session")
	}
	if rec.id != opener.lastID {
		t.Errorf("builder id %v != engine id %v (would use a different directory)", rec.id, opener.lastID)
	}
}

// TestSessionStoreFactoryResumeOpensRequestedDir proves a resume opens only the requested
// session's directory and constructs it as a resume (not new).
func TestSessionStoreFactoryResumeOpensRequestedDir(t *testing.T) {
	opener := &fakeEngineOpener{}
	f, rec := newFakeFactory(opener)

	want, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}

	a, err := f.Open(context.Background(), SessionSelector{Resume: want}, Config{})
	if err != nil {
		t.Fatalf("Open (resume): %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if opener.lastID != want {
		t.Errorf("engine opened id %v, want the requested resume id %v", opener.lastID, want)
	}
	if rec.isNew {
		t.Error("builder isNew = true on resume, want false")
	}
}

// TestSessionStoreFactoryCloseClosesEngine proves closing the agent closes its own session
// engine (via the installed teardown) — and not before.
func TestSessionStoreFactoryCloseClosesEngine(t *testing.T) {
	opener := &fakeEngineOpener{}
	f, _ := newFakeFactory(opener)

	a, err := f.Open(context.Background(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opener.engine.closed {
		t.Fatal("session engine closed before the agent was closed")
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !opener.engine.closed {
		t.Error("agent Close did not close its session engine")
	}
}

// TestSessionStoreFactoryRepeatedClearMintsDistinct proves a repeated /clear cycle mints a
// fresh session id (and a fresh engine) each time, and that each agent closes its own engine
// — no id reuse, no engine leak across reopens.
func TestSessionStoreFactoryRepeatedClearMintsDistinct(t *testing.T) {
	opener := &fakeEngineOpener{}
	f, _ := newFakeFactory(opener)

	seen := map[uuid.UUID]bool{}
	for i := 0; i < 4; i++ {
		a, err := f.Open(context.Background(), SessionSelector{}, Config{})
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		id := opener.lastID
		if id.IsZero() {
			t.Fatalf("reopen #%d minted a zero id", i)
		}
		if seen[id] {
			t.Errorf("reopen #%d reused session id %v", i, id)
		}
		seen[id] = true

		engine := opener.engine
		if err := a.Close(context.Background()); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}
		if !engine.closed {
			t.Errorf("reopen #%d did not close its own engine", i)
		}
	}
}

// TestSessionStoreFactoryBuildFailureClosesEngine proves a construction failure after the
// engine is opened still closes that engine (no leaked engine/lock on the error path).
func TestSessionStoreFactoryBuildFailureClosesEngine(t *testing.T) {
	opener := &fakeEngineOpener{}
	f := &SessionStoreFactory{
		opener:      opener,
		buildClient: func(ModelCatalog) (llm.LLM, ModelFactory, error) { return &fakeLLM{}, newModelFactory(), nil },
		build: func(ctx context.Context, js nats.JetStreamContext, client llm.LLM, factory ModelFactory, id uuid.UUID, isNew bool, sel SessionSelector, cfg Config) (*sessionAgent, error) {
			return nil, errors.New("build failed")
		},
	}

	if _, err := f.Open(context.Background(), SessionSelector{}, Config{}); err == nil {
		t.Fatal("Open succeeded despite a build failure")
	}
	if opener.engine == nil || !opener.engine.closed {
		t.Error("engine was not closed after a build failure")
	}
}
