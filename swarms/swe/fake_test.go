package swe

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/storage/memstore"
)

// fakeInvokeStep scripts exactly one one-shot request. entered/release are
// independent of the stream lane so tests can prove compaction blocks only the
// loop that requested it.
type fakeInvokeStep struct {
	response *inference.Response
	respond  func(inference.Request) (*inference.Response, error)
	err      error
	entered  chan struct{}
	release  <-chan struct{}
}

// fakeStreamStep scripts exactly one ordinary streamed inference request.
type fakeStreamStep struct {
	chunks  []content.Chunk
	result  *inference.StreamResult
	err     error
	entered chan struct{}
	release <-chan struct{}
}

type fakeLLMScriptError struct{ Operation string }

func (e *fakeLLMScriptError) Error() string {
	return "swe test: fake LLM " + e.Operation + " not scripted"
}

// fakeLLM is a controllable inference.Client for tests. Stream and Invoke have
// independent scripts, captures, and barriers so compaction can never consume
// an ordinary-turn fixture. The legacy stream fields remain as a compatibility
// fallback for existing focused tests.
type fakeLLM struct {
	credential string // opaque test-only connection secret; never part of fingerprints
	chunks     []content.Chunk
	streamErr  error         // returned from Stream() before any chunk
	hold       chan struct{} // if non-nil, Next blocks on hold or ctx after chunks

	entered     chan struct{} // if non-nil, closed once when Stream is first called
	enteredOnce sync.Once

	mu             sync.Mutex
	streamSteps    []fakeStreamStep
	invokeSteps    []fakeInvokeStep
	streamRequests []inference.Request
	invokeRequests []inference.Request
}

func (f *fakeLLM) Invoke(ctx context.Context, req inference.Request) (*inference.Response, error) {
	f.mu.Lock()
	f.invokeRequests = append(f.invokeRequests, req)
	if len(f.invokeSteps) == 0 {
		f.mu.Unlock()
		return nil, &fakeLLMScriptError{Operation: "Invoke"}
	}
	step := f.invokeSteps[0]
	f.invokeSteps = f.invokeSteps[1:]
	f.mu.Unlock()
	if step.entered != nil {
		close(step.entered)
	}
	if step.release != nil {
		select {
		case <-step.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if step.err != nil {
		return nil, step.err
	}
	if step.respond != nil {
		return step.respond(req)
	}
	return step.response, nil
}

func (f *fakeLLM) Stream(ctx context.Context, req inference.Request) (*inference.StreamReader[content.Chunk], error) {
	f.mu.Lock()
	f.streamRequests = append(f.streamRequests, req)
	var scripted *fakeStreamStep
	if len(f.streamSteps) != 0 {
		step := f.streamSteps[0]
		f.streamSteps = f.streamSteps[1:]
		scripted = &step
	}
	f.mu.Unlock()
	if scripted != nil {
		if scripted.entered != nil {
			close(scripted.entered)
		}
		if scripted.release != nil {
			select {
			case <-scripted.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if scripted.err != nil {
			return nil, scripted.err
		}
		return fakeStreamReader(scripted.chunks, scripted.result), nil
	}
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

func fakeStreamReader(chunks []content.Chunk, result *inference.StreamResult) *inference.StreamReader[content.Chunk] {
	index := 0
	next := func() (content.Chunk, error) {
		if index == len(chunks) {
			return nil, io.EOF
		}
		chunk := chunks[index]
		index++
		return chunk, nil
	}
	if result == nil {
		return inference.NewStreamReader(next, nil)
	}
	terminal := *result
	return inference.NewStreamReaderWithResult(next, nil, func() (inference.StreamResult, bool, error) {
		return terminal, true, nil
	})
}

func (f *fakeLLM) capturedRequests() ([]inference.Request, []inference.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]inference.Request(nil), f.streamRequests...), append([]inference.Request(nil), f.invokeRequests...)
}

// testModel is a minimal valid inference.Model for fake-client tests. The fake
// ignores it, but the configured window mirrors a consumer-supplied LM Studio
// limit so context-enabled definitions have a resolvable input denominator.
func testModel() inference.Model {
	model := lmStudioLocal("fake-model")
	model.Limits = inference.ContextLimits{WindowTokens: 128_000}
	return model
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
