package swe

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/persistence"
	"github.com/ciram-co/looprig/pkg/uuid"
)

var titleClock = time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)

// recordingLLM records the last Request and the number of Stream calls, returning scripted
// chunks (or an error). It can optionally block in Stream to test non-blocking behavior.
type recordingLLM struct {
	mu      sync.Mutex
	lastReq llm.Request
	calls   int
	chunks  []content.Chunk
	err     error
	block   chan struct{} // if non-nil, Stream blocks on it (or ctx) before returning
}

func (r *recordingLLM) Invoke(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("recordingLLM.Invoke not used")
}

func (r *recordingLLM) Stream(ctx context.Context, req llm.Request) (*llm.StreamReader[content.Chunk], error) {
	r.mu.Lock()
	r.lastReq = req
	r.calls++
	r.mu.Unlock()

	if r.block != nil {
		select {
		case <-r.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	i := 0
	chunks := r.chunks
	next := func() (content.Chunk, error) {
		if i < len(chunks) {
			c := chunks[i]
			i++
			return c, nil
		}
		return nil, io.EOF
	}
	return llm.NewStreamReader(next, nil), nil
}

func (r *recordingLLM) streamCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *recordingLLM) request() llm.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastReq
}

// newTitleTestCoordinator builds a coordinator over a real (temp filesystem) manifest, with
// the given client and an optional Economy title spec (lmstudio, no key).
func newTitleTestCoordinator(t *testing.T, client llm.LLM, withEconomy bool) (*titleCoordinator, *persistence.SessionMetaStore) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root, err := persistence.OpenSessionStoreRoot()
	if err != nil {
		t.Fatalf("OpenSessionStoreRoot: %v", err)
	}
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	store, err := root.OpenSessionMeta(id)
	if err != nil {
		t.Fatalf("OpenSessionMeta: %v", err)
	}
	if _, err := store.Init(titleClock); err != nil {
		t.Fatalf("Init: %v", err)
	}

	var titleSpec func(string) llm.ModelSpec
	if withEconomy {
		titleSpec = func(system string) llm.ModelSpec {
			s := testSpec() // lmstudio, no key
			s.System = system
			return s
		}
	}
	coord := newTitleCoordinator(store, client, titleSpec, func() time.Time { return titleClock })
	return coord, store
}

func textBlocks(s string) []content.Block { return []content.Block{&content.TextBlock{Text: s}} }

// TestSessionTitleFallbackOnUserInput proves the first non-empty user text is stored
// immediately as the first-user-message fallback.
func TestSessionTitleFallbackOnUserInput(t *testing.T) {
	coord, store := newTitleTestCoordinator(t, &recordingLLM{}, false)

	coord.observeUserInput(textBlocks("Fix the login redirect bug"))

	meta, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if meta.Title != "Fix the login redirect bug" {
		t.Errorf("Title = %q, want the user message", meta.Title)
	}
	if meta.TitleSource != persistence.TitleSourceFirstUserMessage {
		t.Errorf("TitleSource = %q, want %q", meta.TitleSource, persistence.TitleSourceFirstUserMessage)
	}
}

// TestSessionTitleImageOnlyKeepsNone proves an image-only first message leaves the manifest
// at TitleSourceNone.
func TestSessionTitleImageOnlyKeepsNone(t *testing.T) {
	coord, store := newTitleTestCoordinator(t, &recordingLLM{}, false)

	coord.observeUserInput([]content.Block{&content.ImageBlock{MediaType: "image/png"}})

	meta, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if meta.TitleSource != persistence.TitleSourceNone {
		t.Errorf("TitleSource = %q, want %q for image-only input", meta.TitleSource, persistence.TitleSourceNone)
	}
}

// TestSessionTitleGeneratedReplacesFallback proves a successful Economy generation replaces
// the fallback with a generated title after the first terminal turn, with NO tools in the
// request.
func TestSessionTitleGeneratedReplacesFallback(t *testing.T) {
	rec := &recordingLLM{chunks: []content.Chunk{&content.TextChunk{Text: "Login Redirect Fix"}}}
	coord, store := newTitleTestCoordinator(t, rec, true)

	coord.observeUserInput(textBlocks("fix login"))
	coord.observeTerminalResponse("Patched the redirect handler.")
	coord.wait()

	meta, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if meta.Title != "Login Redirect Fix" {
		t.Errorf("Title = %q, want the generated title", meta.Title)
	}
	if meta.TitleSource != persistence.TitleSourceGenerated {
		t.Errorf("TitleSource = %q, want %q", meta.TitleSource, persistence.TitleSourceGenerated)
	}
	if tools := rec.request().Tools; len(tools) != 0 {
		t.Errorf("title request carried %d tools, want 0", len(tools))
	}
}

// TestSessionTitleGenerationFailureKeepsFallback proves an error/empty/invalid generation
// preserves the first-user-message fallback.
func TestSessionTitleGenerationFailureKeepsFallback(t *testing.T) {
	tests := []struct {
		name string
		llm  *recordingLLM
	}{
		{name: "stream error", llm: &recordingLLM{err: errors.New("provider down")}},
		{name: "empty output", llm: &recordingLLM{chunks: []content.Chunk{&content.TextChunk{Text: "   "}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coord, store := newTitleTestCoordinator(t, tt.llm, true)

			coord.observeUserInput(textBlocks("fix login"))
			coord.observeTerminalResponse("done")
			coord.wait()

			meta, err := store.Read()
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if meta.Title != "fix login" || meta.TitleSource != persistence.TitleSourceFirstUserMessage {
				t.Errorf("manifest = %q/%q, want the preserved fallback", meta.Title, meta.TitleSource)
			}
		})
	}
}

// TestSessionTitleGeneratesAtMostOnce proves only one generation runs even across multiple
// terminal responses.
func TestSessionTitleGeneratesAtMostOnce(t *testing.T) {
	rec := &recordingLLM{chunks: []content.Chunk{&content.TextChunk{Text: "First Title"}}}
	coord, _ := newTitleTestCoordinator(t, rec, true)

	coord.observeUserInput(textBlocks("fix login"))
	coord.observeTerminalResponse("reply one")
	coord.wait()
	coord.observeTerminalResponse("reply two")
	coord.wait()

	if got := rec.streamCalls(); got != 1 {
		t.Errorf("title model called %d times, want 1 (one generation maximum)", got)
	}
}

// TestSessionTitleNoEconomyKeepsFallback proves an absent Economy model means no generation:
// the fallback stands and the client is never called.
func TestSessionTitleNoEconomyKeepsFallback(t *testing.T) {
	rec := &recordingLLM{chunks: []content.Chunk{&content.TextChunk{Text: "should not be used"}}}
	coord, store := newTitleTestCoordinator(t, rec, false)

	coord.observeUserInput(textBlocks("fix login"))
	coord.observeTerminalResponse("done")
	coord.wait()

	if got := rec.streamCalls(); got != 0 {
		t.Errorf("title model called %d times with no Economy configured, want 0", got)
	}
	meta, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if meta.TitleSource != persistence.TitleSourceFirstUserMessage {
		t.Errorf("TitleSource = %q, want the fallback %q", meta.TitleSource, persistence.TitleSourceFirstUserMessage)
	}
}

// TestSessionTitleObserveDoesNotBlock proves observeTerminalResponse returns immediately even
// when the title model is slow (the worker runs in the background; Close never waits on it).
func TestSessionTitleObserveDoesNotBlock(t *testing.T) {
	rec := &recordingLLM{block: make(chan struct{}), chunks: []content.Chunk{&content.TextChunk{Text: "x"}}}
	coord, _ := newTitleTestCoordinator(t, rec, true)
	coord.observeUserInput(textBlocks("fix login"))

	done := make(chan struct{})
	go func() {
		coord.observeTerminalResponse("done")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("observeTerminalResponse blocked on a slow title model")
	}
	close(rec.block) // let the background worker finish
	coord.wait()
}
