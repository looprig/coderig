package swe

import (
	"testing"

	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/tools"
	"github.com/looprig/sandbox"
)

// security_test.go is the I-2 DRIFT GUARD. swe is the ONLY module that imports
// BOTH harness and sandbox, so this file is the sole place the structural coupling
// between them can be checked. The compile-time assertions below assert that a
// *sandbox.Executor satisfies EVERY harness interface the runner seam and the
// posture interlock probe it for. harness holds the runner as a stdlib-typed
// tool.CommandRunner and probes GuaranteeBits()/Level()/PlanGrants()/DescribeGrant()
// STRUCTURALLY (it never imports sandbox), so a signature drift on either side
// would NOT break the harness build — the probe would silently type-assert to
// false and fail closed (GuaranteeBits → 0, auto-approve degrades to Ask). That is
// the silent-probe-returns-0 failure mode. THIS FILE catches it: if any signature
// ever drifts, the assertions below fail to COMPILE.

// Compile-time conformance: a *sandbox.Executor must satisfy every harness runner
// interface plus every optional capability the posture interlock probes for.
var (
	_ tool.CommandRunner = (*sandbox.Executor)(nil)
	_ tool.ArgvRunner    = (*sandbox.Executor)(nil)
	_ tool.GrantedRunner = (*sandbox.Executor)(nil)

	_ interface{ GuaranteeBits() uint64 } = (*sandbox.Executor)(nil)
	_ interface{ Level() uint8 }          = (*sandbox.Executor)(nil)
	_ interface {
		PlanGrants(dir, command string) []string
	} = (*sandbox.Executor)(nil)
	_ interface {
		DescribeGrant(token string) (string, bool)
	} = (*sandbox.Executor)(nil)

	// The ordinal↔mode adapter must satisfy sandbox.ModeSource so the SAME ceiling
	// source can drive the sandbox dynamic executor as well as the harness checker.
	_ sandbox.ModeSource = ceilingModeSource{}
)

// TestExecutorProbePathEndToEnd builds a REAL *sandbox.Executor and confirms the
// GuaranteeBits()/Level() probe path works end-to-end — i.e. it returns real
// enforcement data, not the silent 0 the fail-closed interlock treats as "no
// guarantees". The compile-time block above proves the SIGNATURES match; this
// proves the wired methods actually report.
func TestExecutorProbePathEndToEnd(t *testing.T) {
	t.Parallel()

	ex, err := sandbox.NewExecutor(sandbox.PolicyFor(sandbox.Write, t.TempDir()))
	if err != nil {
		t.Fatalf("NewExecutor(Write): unexpected error: %v", err)
	}
	if ex == nil {
		t.Fatal("NewExecutor(Write): nil executor")
	}

	bits := ex.GuaranteeBits()
	if bits == 0 {
		t.Fatal("GuaranteeBits() = 0: probe path returns no guarantees (silent-0 failure mode)")
	}
	// EnvScrub is enforced for Write on every backend (even the null backend), so a
	// real Write executor must report it — the portable floor of the probe path.
	if bits&sandbox.GuaranteeEnvScrub == 0 {
		t.Errorf("GuaranteeBits() = %#b, missing GuaranteeEnvScrub", bits)
	}
	// The host platform (darwin/linux) must actually enforce the write-mode bash
	// interlock mask, otherwise write-mode Bash auto-approve could never fire.
	if bits&writeBashGuarantees != writeBashGuarantees {
		t.Errorf("GuaranteeBits() = %#b, does not satisfy writeBashGuarantees %#b", bits, writeBashGuarantees)
	}

	if lvl := ex.Level(); lvl == sandbox.LevelNone {
		t.Errorf("Level() = LevelNone: a real Write executor should report a non-none isolation level")
	}
}

// TestPostureTableMatchesSpec asserts each ordinal's Posture encodes the SPEC §4
// row for the corresponding mode, with the exact §10.3 guarantee masks.
func TestPostureTableMatchesSpec(t *testing.T) {
	t.Parallel()

	table := postureTable()
	if got, want := len(table), 5; got != want {
		t.Fatalf("postureTable() length = %d, want %d (one row per sandbox.Mode)", got, want)
	}

	tests := []struct {
		name               string
		ordinal            sandbox.Mode
		autoApproveEdits   bool
		autoApproveBash    bool
		requiredGuarantees uint64
		trivialBashSet     bool
	}{
		{
			name:               "zerotrust: everything ask",
			ordinal:            sandbox.ZeroTrust,
			autoApproveEdits:   false,
			autoApproveBash:    false,
			requiredGuarantees: 0,
			trivialBashSet:     false,
		},
		{
			name:               "readonly: everything ask",
			ordinal:            sandbox.ReadOnly,
			autoApproveEdits:   false,
			autoApproveBash:    false,
			requiredGuarantees: 0,
			trivialBashSet:     false,
		},
		{
			name:               "write: edits auto, trivial bash auto, rest ask",
			ordinal:            sandbox.Write,
			autoApproveEdits:   true,
			autoApproveBash:    false,
			requiredGuarantees: sandbox.GuaranteeWriteBoundary | sandbox.GuaranteeEnvScrub | sandbox.GuaranteeReadDenies,
			trivialBashSet:     true,
		},
		{
			name:               "trusted: edits auto, all bash auto",
			ordinal:            sandbox.Trusted,
			autoApproveEdits:   true,
			autoApproveBash:    true,
			requiredGuarantees: sandbox.GuaranteeWriteBoundary | sandbox.GuaranteeEnvScrub | sandbox.GuaranteeReadDenies | sandbox.GuaranteeNetworkBoundary,
			trivialBashSet:     false,
		},
		{
			name:               "unconfined: all auto, no interlock",
			ordinal:            sandbox.Unconfined,
			autoApproveEdits:   true,
			autoApproveBash:    true,
			requiredGuarantees: 0,
			trivialBashSet:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := table[tt.ordinal]
			if p.AutoApproveEdits != tt.autoApproveEdits {
				t.Errorf("AutoApproveEdits = %v, want %v", p.AutoApproveEdits, tt.autoApproveEdits)
			}
			if p.AutoApproveBash != tt.autoApproveBash {
				t.Errorf("AutoApproveBash = %v, want %v", p.AutoApproveBash, tt.autoApproveBash)
			}
			if p.RequiredGuarantees != tt.requiredGuarantees {
				t.Errorf("RequiredGuarantees = %#b, want %#b", p.RequiredGuarantees, tt.requiredGuarantees)
			}
			if (p.TrivialBash != nil) != tt.trivialBashSet {
				t.Errorf("TrivialBash set = %v, want %v", p.TrivialBash != nil, tt.trivialBashSet)
			}
			// Escalations are NEVER auto-approved by posture: every row asks on a
			// grant-carrying call, regardless of mode (SPEC §9.3/§10.7).
			if !p.GrantCarryingAlwaysAsk {
				t.Errorf("GrantCarryingAlwaysAsk = false, want true (escalations must always ask)")
			}
			// No secondary Level() floor is set by this table; the guarantee mask is
			// the sole interlock (SPEC §10.3).
			if p.RequiredLevel != 0 {
				t.Errorf("RequiredLevel = %d, want 0", p.RequiredLevel)
			}
		})
	}
}

// TestCeilingModeSourceMapsOrdinalToMode asserts the adapter maps the ceiling
// ordinal 1:1 onto sandbox.Mode along the same ladder (0 = most restrictive).
func TestCeilingModeSourceMapsOrdinalToMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ordinal uint8
		want    sandbox.Mode
	}{
		{name: "0 -> ZeroTrust", ordinal: 0, want: sandbox.ZeroTrust},
		{name: "1 -> ReadOnly", ordinal: 1, want: sandbox.ReadOnly},
		{name: "2 -> Write", ordinal: 2, want: sandbox.Write},
		{name: "3 -> Trusted", ordinal: 3, want: sandbox.Trusted},
		{name: "4 -> Unconfined", ordinal: 4, want: sandbox.Unconfined},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Drive the adapter through the SAME source shape the harness checker
			// reads (tools.CeilingSource), proving one source feeds both seams.
			st := ceiling.NewClamped(tt.ordinal)
			st.Set(tt.ordinal)
			var src tools.CeilingSource = st
			m := ceilingModeSource{src: src}
			if got := m.Current(); got != tt.want {
				t.Errorf("Current() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestTrivialBashClassifier documents the write-mode "trivial auto, rest ask"
// slot: read-only prefixes auto-approve; anything that could chain, substitute,
// or redirect a non-trivial command falls to Ask (fail-safe).
func TestTrivialBashClassifier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{name: "ls", command: "ls -la", want: true},
		{name: "cat file", command: "cat go.mod", want: true},
		{name: "pwd", command: "pwd", want: true},
		{name: "echo", command: "echo hello", want: true},
		{name: "git status", command: "git status --short", want: true},
		{name: "git diff", command: "git diff HEAD", want: true},
		{name: "collapsed whitespace", command: "git   status", want: true},

		{name: "empty", command: "", want: false},
		{name: "unknown command", command: "rm -rf /", want: false},
		{name: "word-boundary: catalog is not cat", command: "catalog build", want: false},
		{name: "chained with &&", command: "cat x && rm -rf /", want: false},
		{name: "piped", command: "cat x | sh", want: false},
		{name: "semicolon", command: "ls; rm -rf /", want: false},
		{name: "redirect", command: "echo hi > /etc/passwd", want: false},
		{name: "command substitution", command: "echo $(rm -rf /)", want: false},
		{name: "backtick substitution", command: "echo `rm -rf /`", want: false},
		{name: "newline separator", command: "cat x\nrm -rf /", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := trivialBash(tt.command); got != tt.want {
				t.Errorf("trivialBash(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}
