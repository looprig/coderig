package swe

import (
	"context"
	"testing"

	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tools"
	"github.com/looprig/sandbox"
	"github.com/looprig/swe/confine"
)

// confinement_test.go is the Task-22 wiring-invariant suite: it proves the
// effective-mode clamp (min(role, ceiling)), the SAME-executor wiring at a build
// site (Bash runner + Grep read-only view + checker posture), the nil-executor
// graceful fallback (gated, never raw-auto, never crash), the live ceiling clamp,
// the subagent non-escalation clamp, and the shared session ceiling.

// planGranter is the structural PlanGrants probe: the full executor (write mode,
// egress gated) offers a "net" grant candidate; a ReadOnlyView never escalates and
// offers none. It distinguishes Bash's runner (full executor) from Grep's (view).
type planGranter interface {
	PlanGrants(dir, command string) []string
}

// leveled is the structural Level probe (mirrors the harness interlock's probe): a
// LevelNone runner is the null backend / a platform without OS enforcement.
type leveled interface{ Level() uint8 }

// TestEffectiveModeSourceIsMinOfRoleAndCeiling proves the effective mode is
// min(role static mode, session ceiling), read live — a role never exceeds the
// ceiling, and the ceiling never exceeds the role.
func TestEffectiveModeSourceIsMinOfRoleAndCeiling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		role    uint8
		ceiling uint8
		want    uint8
	}{
		{name: "write role capped by readonly ceiling", role: operatorRoleMode, ceiling: uint8(sandbox.ReadOnly), want: uint8(sandbox.ReadOnly)},
		{name: "write role under trusted ceiling stays at role", role: operatorRoleMode, ceiling: uint8(sandbox.Trusted), want: operatorRoleMode},
		{name: "readonly role under trusted ceiling capped by role", role: reviewerRoleMode, ceiling: uint8(sandbox.Trusted), want: reviewerRoleMode},
		{name: "write role under write ceiling equal", role: operatorRoleMode, ceiling: uint8(sandbox.Write), want: uint8(sandbox.Write)},
		{name: "zerotrust ceiling clamps everything to floor", role: operatorRoleMode, ceiling: uint8(sandbox.ZeroTrust), want: uint8(sandbox.ZeroTrust)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := ceiling.New()
			st.Set(tt.ceiling)
			e := effectiveModeSource{role: tt.role, ceiling: st}
			if got := e.Current(); got != tt.want {
				t.Errorf("effectiveModeSource{role:%d}.Current() @ ceiling %d = %d, want %d", tt.role, tt.ceiling, got, tt.want)
			}
		})
	}
}

// TestEffectiveModeSourceNilCeilingFailsClosed proves a nil ceiling fail-closes the
// effective mode to 0 (ZeroTrust), never dereferencing nil.
func TestEffectiveModeSourceNilCeilingFailsClosed(t *testing.T) {
	t.Parallel()

	e := effectiveModeSource{role: operatorRoleMode, ceiling: nil}
	if got := e.Current(); got != uint8(sandbox.ZeroTrust) {
		t.Errorf("effectiveModeSource with nil ceiling = %d, want ZeroTrust (0)", got)
	}
}

// TestConfinementForWiresSameExecutor proves confinementFor mints ONE executor and
// wires it into Bash's runner + the checker (both the full executor) and Grep's
// runner (its read-only view — never escalates). The PlanGrants distinction is
// backend-independent (executor-side), so it holds on every platform.
func TestConfinementForWiresSameExecutor(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	st := ceiling.New()
	st.Set(uint8(sandbox.Write))
	conf := confinementFor(root, effectiveModeSource{role: operatorRoleMode, ceiling: st})

	if conf.BashRunner == nil {
		t.Fatal("confinementFor(): BashRunner = nil, want the confined executor")
	}
	if conf.GrepRunner == nil {
		t.Fatal("confinementFor(): GrepRunner = nil, want the read-only view")
	}
	if conf.CheckerOption == nil {
		t.Fatal("confinementFor(): CheckerOption = nil, want the ceiling-posture option")
	}

	// Bash's runner is the FULL executor: write mode gates egress, so it offers a
	// "net" grant candidate. Grep's runner is the READ-ONLY VIEW: the read path never
	// escalates, so it offers none. This proves Grep got the view, not the full runner.
	bashG, ok := conf.BashRunner.(planGranter)
	if !ok {
		t.Fatal("BashRunner does not expose PlanGrants")
	}
	if got := bashG.PlanGrants(root, "ls"); len(got) == 0 {
		t.Error("BashRunner.PlanGrants() = empty, want a net candidate (write mode gates egress)")
	}
	grepG, ok := conf.GrepRunner.(planGranter)
	if !ok {
		t.Fatal("GrepRunner does not expose PlanGrants")
	}
	if got := grepG.PlanGrants(root, "ls"); len(got) != 0 {
		t.Errorf("GrepRunner.PlanGrants() = %v, want nil (read-only view never escalates)", got)
	}
}

// TestConfinedCheckerAutoApprovesTrivialBashAtWrite proves the checker built from a
// real Write-mode confinement holds the enforcing runner: under an OS backend it
// auto-approves a trivial Bash command (interlock passes). It is SKIPPED on a
// null/degraded-enforcement backend (Linux pre-Landlock), where the interlock
// correctly keeps Bash at Ask — mirroring TestExecutorProbePathEndToEnd's gate.
func TestConfinedCheckerAutoApprovesTrivialBashAtWrite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	st := ceiling.New()
	st.Set(uint8(sandbox.Write))
	conf := confinementFor(root, effectiveModeSource{role: operatorRoleMode, ceiling: st})

	lv, ok := conf.BashRunner.(leveled)
	if !ok || !osEnforcementProven(lv.Level()) {
		t.Skip("no OS backend enforcing (null backend / Linux pre-Landlock): trivial-Bash auto-approve is correctly gated to Ask")
	}

	pc := newConfinedChecker(t, root, conf)
	bash := tools.NewBash(root, conf.BashOptions()...)

	if eff := pc.Check(context.Background(), bash, "Bash", `{"command":"ls -la"}`); eff != loop.EffectAutoApprove {
		t.Errorf("Check(Bash ls) @ Write with enforcing runner = %v, want EffectAutoApprove (interlock passes, trivial)", eff)
	}
	// A non-trivial command still Asks even at Write (trivial-auto, rest-ask).
	if eff := pc.Check(context.Background(), bash, "Bash", `{"command":"rm -rf build"}`); eff == loop.EffectAutoApprove {
		t.Error("Check(Bash rm) @ Write = EffectAutoApprove, want a human gate (non-trivial command)")
	}
}

// TestConfinementForFallbackShapeOnExecutorError proves the graceful fallback: when
// the executor cannot build (HOME unresolvable → the sandbox home guard errors),
// confinementFor returns NIL runners but a NON-nil posture Option — so tools run
// unconfined-but-gated, never crashing and never handing back a raw-auto executor.
func TestConfinementForFallbackShapeOnExecutorError(t *testing.T) {
	// NOT parallel: HOME is process-global.
	t.Setenv("HOME", "")

	st := ceiling.New()
	st.Set(uint8(sandbox.Write))
	conf := confinementFor(t.TempDir(), effectiveModeSource{role: operatorRoleMode, ceiling: st})

	if conf.BashRunner != nil {
		t.Error("fallback: BashRunner != nil, want nil (unconfined direct exec)")
	}
	if conf.GrepRunner != nil {
		t.Error("fallback: GrepRunner != nil, want nil (unconfined direct exec)")
	}
	if conf.CheckerOption == nil {
		t.Error("fallback: CheckerOption = nil, want a posture Option (so the interlock keeps Bash gated)")
	}
}

// TestFallbackConfinementKeepsBashGated proves the fallback posture (nil runner)
// fails the guarantee interlock closed: even at Write mode, Bash never auto-approves
// without an enforcing runner — it stays Ask (never raw-auto). Portable: the nil
// runner fails the interlock on every backend.
func TestFallbackConfinementKeepsBashGated(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	st := ceiling.New()
	st.Set(uint8(sandbox.Write))
	effSrc := effectiveModeSource{role: operatorRoleMode, ceiling: st}
	// The exact fallback shape confinementFor returns on a build error: posture present,
	// runner nil.
	conf := confine.Confinement{CheckerOption: tools.WithCeilingPostures(effSrc, postureTable(), nil)}

	pc := newConfinedChecker(t, root, conf)
	bash := tools.NewBash(root, conf.BashOptions()...)
	if eff := pc.Check(context.Background(), bash, "Bash", `{"command":"ls"}`); eff == loop.EffectAutoApprove {
		t.Errorf("Check(Bash ls) with nil runner = EffectAutoApprove, want a human gate (interlock must fail closed)")
	}
}

// TestCeilingChangeClampsCheckerLive proves the shared ceiling drives the checker
// live: lowering the ceiling flips a would-be auto-approve to Ask on the NEXT Check,
// with no rebuild. SKIPPED on a null backend (where Bash never auto-approves to begin
// with, so there is no flip to observe).
func TestCeilingChangeClampsCheckerLive(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	st := ceiling.New() // no cap: Set(Write) then Set(ReadOnly)
	st.Set(uint8(sandbox.Write))
	conf := confinementFor(root, effectiveModeSource{role: operatorRoleMode, ceiling: st})

	lv, ok := conf.BashRunner.(leveled)
	if !ok || !osEnforcementProven(lv.Level()) {
		t.Skip("no OS backend enforcing: trivial-Bash auto-approve is gated regardless of ceiling")
	}

	pc := newConfinedChecker(t, root, conf)
	bash := tools.NewBash(root, conf.BashOptions()...)

	if eff := pc.Check(context.Background(), bash, "Bash", `{"command":"ls"}`); eff != loop.EffectAutoApprove {
		t.Fatalf("Check(Bash ls) @ Write = %v, want EffectAutoApprove before the clamp", eff)
	}
	// Lower the SHARED ceiling to ReadOnly: the SAME checker's next Check must now Ask
	// (posture[readonly] auto-approves no Bash), proving the live clamp.
	st.Set(uint8(sandbox.ReadOnly))
	if eff := pc.Check(context.Background(), bash, "Bash", `{"command":"ls"}`); eff == loop.EffectAutoApprove {
		t.Error("Check(Bash ls) after lowering ceiling to ReadOnly = EffectAutoApprove, want Ask (live clamp)")
	}
}

// TestSubagentClampBoundsChildToParentEffective proves the non-escalation clamp: a
// child whose static role is MORE permissive than its parent's effective mode is
// clamped to the parent's effective mode — a runtime spawn can never elevate.
func TestSubagentClampBoundsChildToParentEffective(t *testing.T) {
	t.Parallel()

	// Session ceiling ReadOnly makes the primary operator's effective mode ReadOnly
	// (min(Write, ReadOnly)) — the parent-effective source the spawner threads down.
	st := ceiling.New()
	st.Set(uint8(sandbox.ReadOnly))
	parentEff := effectiveModeSource{role: operatorRoleMode, ceiling: st}
	if got := parentEff.Current(); got != uint8(sandbox.ReadOnly) {
		t.Fatalf("parent effective = %d, want ReadOnly (min(Write, ReadOnly))", got)
	}

	// A child operator LEAF (static role Write) spawned under this parent: its ceiling
	// is the parent's effective source, so its effective is min(Write, ReadOnly) =
	// ReadOnly — the child cannot exceed the parent even though its role is Write.
	childEff := effectiveModeSource{role: operatorRoleMode, ceiling: parentEff}
	if got := childEff.Current(); got != uint8(sandbox.ReadOnly) {
		t.Errorf("child (role Write) effective under a ReadOnly-effective parent = %d, want ReadOnly (clamped, no escalation)", got)
	}
}

// TestBuildOperatorWiringSharesCeilingWithSession proves the composition root wires
// the SAME ceiling into the session (via session.WithCeiling): the session's
// CeilingSource reports the configured launch ceiling, and a journaled
// SetSecurityCeiling lowering it is visible on the session's source (the SAME state
// every checker reads).
func TestBuildOperatorWiringSharesCeilingWithSession(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	agent, err := newWithClient(ctx, &fakeLLM{}, newModelFactory(), Config{SecurityCeiling: operatorRoleMode})
	if err != nil {
		t.Fatalf("newWithClient() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(ctx) })

	if got := agent.session.CeilingSource().Current(); got != operatorRoleMode {
		t.Errorf("session ceiling at launch = %d, want the configured %d (Write)", got, operatorRoleMode)
	}

	// Lowering the journaled ceiling is visible on the SAME source (fail-secure tighten).
	if err := agent.session.SetSecurityCeiling(ctx, reviewerRoleMode); err != nil {
		t.Fatalf("SetSecurityCeiling() error = %v", err)
	}
	if got := agent.session.CeilingSource().Current(); got != reviewerRoleMode {
		t.Errorf("session ceiling after lowering = %d, want %d (ReadOnly)", got, reviewerRoleMode)
	}
}

// TestParseSecurityMode proves the CLI mode-name boundary parse: known names map to
// their ceiling ordinal; unknown/unconfined names fail closed.
func TestParseSecurityMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want uint8
		ok   bool
	}{
		{name: "zerotrust", in: "zerotrust", want: uint8(sandbox.ZeroTrust), ok: true},
		{name: "readonly", in: "readonly", want: uint8(sandbox.ReadOnly), ok: true},
		{name: "write", in: "write", want: uint8(sandbox.Write), ok: true},
		{name: "trusted", in: "trusted", want: uint8(sandbox.Trusted), ok: true},
		{name: "unconfined rejected", in: "unconfined", want: 0, ok: false},
		{name: "unknown rejected", in: "bogus", want: 0, ok: false},
		{name: "empty rejected", in: "", want: 0, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseSecurityMode(tt.in)
			if ok != tt.ok || got != tt.want {
				t.Errorf("ParseSecurityMode(%q) = (%d, %v), want (%d, %v)", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// newConfinedChecker builds a PermissionChecker with the operator's real policy plus
// the confinement's ceiling-posture option — the exact checker a leaf assembles.
func newConfinedChecker(t *testing.T, root string, conf confine.Confinement) *tools.PermissionChecker {
	t.Helper()
	policy := tools.PermissionPolicy{
		WorkspaceRoot: root,
		HardDeny:      tools.DefaultHardDeny(),
		HardApprove:   tools.HardApproveRules{Tools: []string{"ReadFile", "Glob", "Grep", "Todo", "AskUser"}},
	}
	pc, err := tools.NewPermissionChecker(policy, conf.CheckerOptions()...)
	if err != nil {
		t.Fatalf("NewPermissionChecker() error = %v", err)
	}
	return pc
}
