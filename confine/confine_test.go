package confine

import (
	"context"
	"testing"

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
		tt := tt
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
