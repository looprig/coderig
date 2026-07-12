package confine

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/tools"
)

// confine_test.go covers Confinement's option-projection methods — the tiny mapping a
// tool-building leaf relies on: a PRESENT runner/option projects to a one-element
// option slice, an ABSENT one (the zero Confinement — the fail-secure fallback shape)
// projects to an empty slice with no panic (per the swe CLAUDE.md table-driven
// mandate). Applying an empty option slice is a no-op, reproducing the pre-sandbox
// unconfined-but-gated behavior.

// stubCommandRunner is a minimal tool.CommandRunner stand-in (Bash's runner seam).
type stubCommandRunner struct{}

func (stubCommandRunner) RunCommand(context.Context, string, string) ([]byte, int, error) {
	return nil, 0, nil
}

// stubArgvRunner is a minimal tool.ArgvRunner stand-in (Grep's read-only view seam).
type stubArgvRunner struct{}

func (stubArgvRunner) RunArgv(context.Context, string, []string) ([]byte, int, error) {
	return nil, 0, nil
}

// errStub is a leaf test fixture error (a For failure a leaf must propagate).
var errStub = errors.New("stub factory error")

// stubFactory is a per-bind Factory stand-in: it returns a fixed Confinement (or a
// typed error) for any binding, proving the Factory seam a tool-building leaf calls
// once per bind. The real memoizing-per-LoopID Factory lives in the swarms/swe
// composition root (which owns the sandbox import).
type stubFactory struct {
	conf Confinement
	err  error
}

func (f stubFactory) For(tool.Bindings) (Confinement, error) { return f.conf, f.err }

// Factory is the per-bind seam; a stub must satisfy it structurally.
var _ Factory = stubFactory{}

// TestFactoryForContract asserts a Factory implementation yields its Confinement
// (or a typed error) for a binding — the contract a leaf's per-bind tool/permission
// factories rely on.
func TestFactoryForContract(t *testing.T) {
	t.Parallel()

	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	b := tool.Bindings{SessionID: id, LoopID: id, Workspace: &tool.WorkspaceBinding{Root: "/ws"}}

	tests := []struct {
		name     string
		factory  Factory
		wantBash bool
		wantErr  bool
	}{
		{name: "yields confinement", factory: stubFactory{conf: Confinement{BashRunner: stubCommandRunner{}}}, wantBash: true},
		{name: "yields zero confinement", factory: stubFactory{}, wantBash: false},
		{name: "propagates error", factory: stubFactory{err: errStub}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			conf, err := tt.factory.For(b)
			if (err != nil) != tt.wantErr {
				t.Fatalf("For() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got := len(conf.BashOptions()); (got == 1) != tt.wantBash {
				t.Errorf("BashOptions() len = %d, wantBash %v", got, tt.wantBash)
			}
		})
	}
}

// TestConfinementOptionProjection asserts each projection method emits exactly one
// option when its field is set and none when it is nil.
func TestConfinementOptionProjection(t *testing.T) {
	t.Parallel()

	// Any tools.Option is a valid CheckerOption; WithCeilingPostures returns a non-nil
	// option func even with nil args, so it is a convenient present-option fixture.
	checkerOpt := tools.WithCeilingPostures(nil, nil, nil)

	populated := Confinement{
		BashRunner:    stubCommandRunner{},
		GrepRunner:    stubArgvRunner{},
		CheckerOption: checkerOpt,
	}
	zero := Confinement{} // the fail-secure fallback: every field nil.

	tests := []struct {
		name        string
		conf        Confinement
		wantBash    int
		wantGrep    int
		wantChecker int
	}{
		{name: "fully populated -> one option each", conf: populated, wantBash: 1, wantGrep: 1, wantChecker: 1},
		{name: "zero confinement -> empty option slices (no panic)", conf: zero, wantBash: 0, wantGrep: 0, wantChecker: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := len(tt.conf.BashOptions()); got != tt.wantBash {
				t.Errorf("BashOptions() len = %d, want %d", got, tt.wantBash)
			}
			if got := len(tt.conf.GrepOptions()); got != tt.wantGrep {
				t.Errorf("GrepOptions() len = %d, want %d", got, tt.wantGrep)
			}
			if got := len(tt.conf.CheckerOptions()); got != tt.wantChecker {
				t.Errorf("CheckerOptions() len = %d, want %d", got, tt.wantChecker)
			}
		})
	}
}
