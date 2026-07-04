package swe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/inference"
)

// fakeBuildRecord captures what the injected agent builder was constructed with.
type fakeBuildRecord struct {
	id    uuid.UUID
	isNew bool
	calls int
}

// newFakeFactory builds a SessionStoreFactory whose agent builder avoids the on-disk store: it
// returns a headless agent (so Close works without a journal) and records the id + isNew it was
// invoked with. The fs/store/catalog/ws fields stay nil — openWithClient never touches them, so
// only the buildClient + build seams are exercised. buildErr, when non-nil, is returned by the
// builder so the failure path can be tested; clientErr, when non-nil, is returned by buildClient.
func newFakeFactory(buildErr, clientErr error) (*SessionStoreFactory, *fakeBuildRecord) {
	rec := &fakeBuildRecord{}
	f := &SessionStoreFactory{
		buildClient: func(ModelCatalog) (inference.Client, ModelFactory, error) {
			if clientErr != nil {
				return nil, nil, clientErr
			}
			return &fakeLLM{}, newModelFactory(), nil
		},
		build: func(ctx context.Context, client inference.Client, factory ModelFactory, id uuid.UUID, isNew bool, sel SessionSelector, cfg Config) (*sessionAgent, error) {
			rec.calls++
			rec.id = id
			rec.isNew = isNew
			if buildErr != nil {
				return nil, buildErr
			}
			return newSessionAgent(ctx, testPrimaryCfg(testModel(), ""))
		},
	}
	return f, rec
}

// TestOpenResolvesSessionID proves Open resolves the session id at the boundary and drives the
// builder accordingly: a zero selector mints a fresh non-zero id and builds a NEW session; a
// non-zero Resume builds THAT id as a resume (not new).
func TestOpenResolvesSessionID(t *testing.T) {
	t.Parallel()

	resumeID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	tests := []struct {
		name    string
		sel     SessionSelector
		wantNew bool
		wantID  uuid.UUID // zero => expect a freshly minted non-zero id
	}{
		{name: "zero selector mints a new session", sel: SessionSelector{}, wantNew: true},
		{name: "resume selector opens the requested id", sel: SessionSelector{Resume: resumeID}, wantNew: false, wantID: resumeID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, rec := newFakeFactory(nil, nil)
			a, err := f.Open(context.Background(), tt.sel, Config{})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { _ = a.Close(context.Background()) })

			if rec.calls != 1 {
				t.Fatalf("builder called %d times, want 1", rec.calls)
			}
			if rec.isNew != tt.wantNew {
				t.Errorf("builder isNew = %v, want %v", rec.isNew, tt.wantNew)
			}
			if tt.wantID.IsZero() {
				if rec.id.IsZero() {
					t.Error("new session built with a zero id (the id was not minted before building)")
				}
			} else if rec.id != tt.wantID {
				t.Errorf("builder id = %v, want the requested resume id %v", rec.id, tt.wantID)
			}
		})
	}
}

// TestOpenMintsDistinctIDs proves a repeated /clear cycle (a fresh zero-selector Open each time)
// mints a distinct session id every time — no id reuse across reopens.
func TestOpenMintsDistinctIDs(t *testing.T) {
	t.Parallel()

	f, rec := newFakeFactory(nil, nil)
	seen := map[uuid.UUID]bool{}
	for i := 0; i < 4; i++ {
		a, err := f.Open(context.Background(), SessionSelector{}, Config{})
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		if rec.id.IsZero() {
			t.Fatalf("reopen #%d minted a zero id", i)
		}
		if seen[rec.id] {
			t.Errorf("reopen #%d reused session id %v", i, rec.id)
		}
		seen[rec.id] = true
		if err := a.Close(context.Background()); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}
	}
}

// TestOpenPropagatesFailures proves Open fails closed: a buildClient error is returned before the
// builder is ever called, and a builder error is returned verbatim. Both cases yield no agent.
func TestOpenPropagatesFailures(t *testing.T) {
	t.Parallel()

	buildErr := errors.New("build failed")
	clientErr := errors.New("client failed")
	tests := []struct {
		name       string
		buildErr   error
		clientErr  error
		want       error
		wantBuilds int
	}{
		{name: "builder failure propagates", buildErr: buildErr, want: buildErr, wantBuilds: 1},
		{name: "client failure short-circuits build", clientErr: clientErr, want: clientErr, wantBuilds: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, rec := newFakeFactory(tt.buildErr, tt.clientErr)
			a, err := f.Open(context.Background(), SessionSelector{}, Config{})
			if a != nil {
				t.Errorf("Open returned a non-nil agent on failure")
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("Open error = %v, want %v", err, tt.want)
			}
			if rec.calls != tt.wantBuilds {
				t.Errorf("builder called %d times, want %d", rec.calls, tt.wantBuilds)
			}
		})
	}
}

// TestOpenCloseIsIdempotent proves the returned agent Closes cleanly and tolerates a repeated
// Close (the composition-root teardown is guarded, so a double Close never errors).
func TestOpenCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	f, _ := newFakeFactory(nil, nil)
	a, err := f.Open(context.Background(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v", err)
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
