package swe

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
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

// testModel is a minimal valid inference.Model for fake-client tests. The fake ignores it; loop.New
// only requires it to pass inference.Model.Validate (valid provider/APIFormat/BaseURL + non-empty
// name), which the loopback LM Studio catalog row satisfies.
func testModel() inference.Model {
	return lmStudioLocal("fake-model")
}
