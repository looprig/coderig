package swe

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/storage/memstore"
)

// fakeLLM is a controllable inference.Client for tests. The loop only ever calls Stream,
// so Invoke is a stub. Salvaged from the prior coding agent's fake_test.go.
type fakeLLM struct {
	chunks    []content.Chunk
	streamErr error         // returned from Stream() before any chunk
	hold      chan struct{} // if non-nil, Next blocks on hold or ctx after chunks

	entered     chan struct{} // if non-nil, closed once when Stream is first called
	enteredOnce sync.Once
}

func (f *fakeLLM) Invoke(ctx context.Context, req inference.Request) (*inference.Response, error) {
	return nil, errors.New("fakeLLM.Invoke not used")
}

func (f *fakeLLM) Stream(ctx context.Context, req inference.Request) (*inference.StreamReader[content.Chunk], error) {
	if f.entered != nil {
		f.enteredOnce.Do(func() { close(f.entered) })
	}
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	i := 0
	next := func() (content.Chunk, error) {
		if i < len(f.chunks) {
			c := f.chunks[i]
			i++
			return c, nil
		}
		if f.hold != nil {
			select {
			case <-f.hold:
				return nil, io.EOF
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return nil, io.EOF
	}
	return inference.NewStreamReader(next, nil), nil
}

// testModel is a minimal valid inference.Model for fake-client tests. The fake ignores it; loop
// binding only requires it to pass inference.Model.Validate (valid provider/APIFormat/BaseURL +
// non-empty name), which the loopback LM Studio catalog row satisfies.
func testModel() inference.Model {
	return lmStudioLocal("fake-model")
}

// newTestAgent builds an ISOLATED headless sessionAgent over a FRESH in-memory store and a
// throwaway workspace root, so a test never contends on the process-shared headless store's
// exclusive checkout lease (which would serialize every parallel test on the real cwd) and
// never snapshots the real working tree. Each call is independent and parallel-safe; the agent
// is Closed at test cleanup. client is the fake inference client to drive turns.
func newTestAgent(t *testing.T, client inference.Client, cfg Config) *sessionAgent {
	t.Helper()
	root := t.TempDir()
	definitions, err := swarmDefinitions(client, testModel(), cfg)
	if err != nil {
		t.Fatalf("swarmDefinitions() error = %v", err)
	}
	stores, err := openStores(memstore.New())
	if err != nil {
		t.Fatalf("openStores() error = %v", err)
	}
	assembly, err := buildRig(definitions, stores, root, cfg, false)
	if err != nil {
		t.Fatalf("buildRig() error = %v", err)
	}
	controller, err := assembly.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	agent, err := newSessionAgent(context.Background(), controller, stores.session, false)
	if err != nil {
		t.Fatalf("newSessionAgent() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	return agent
}
