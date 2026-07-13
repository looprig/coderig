package swe

import (
	"context"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/tools"
	"github.com/looprig/sandbox"
	"github.com/looprig/swe/confine"
)

// mustBindingID mints a fresh loop id for a tool.Bindings fixture.
func mustBindingID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}

// bindingFor builds a tool.Bindings for a bound loop with the given id, an uncapped session
// ceiling, and a workspace root — the shape a leaf's per-bind factory hands confineFactory.For.
func bindingFor(id uuid.UUID, root string) tool.Bindings {
	return tool.Bindings{
		SessionID: id,
		LoopID:    id,
		Ceiling:   ceiling.New(),
		Workspace: &tool.WorkspaceBinding{Root: root},
	}
}

// TestConfineFactoryMemoizesPerLoopID is the companion test deferred from Task 2: the
// composition root's confine.Factory memoizes ONE Confinement per bindings.LoopID, so three
// For(sameLoopID) calls reuse it (the SAME executor within a process) while a different LoopID
// mints a fresh one. The memo is bounded by the spawn quota so a runaway bind loop cannot grow
// it without bound.
func TestConfineFactoryMemoizesPerLoopID(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	f := newConfineFactory(operatorRoleMode)

	id1 := mustBindingID(t)
	b1 := bindingFor(id1, root)
	c1a, err := f.For(b1)
	if err != nil {
		t.Fatalf("For(id1) error = %v", err)
	}
	c1b, err := f.For(b1)
	if err != nil {
		t.Fatalf("For(id1) again error = %v", err)
	}
	c1c, err := f.For(b1)
	if err != nil {
		t.Fatalf("For(id1) third error = %v", err)
	}
	if got := len(f.memo); got != 1 {
		t.Fatalf("memo size after 3 For(sameLoopID) = %d, want 1 (memoized)", got)
	}
	// The reused Confinement carries the SAME executor instance (interface identity). When the
	// backend enforces nothing the runner is nil for every call — the memo is still proven by
	// the size assertion above, so the runner-identity check is gated on a real executor.
	if c1a.BashRunner != c1b.BashRunner || c1b.BashRunner != c1c.BashRunner {
		t.Error("For(sameLoopID) returned different executors (memo not reused)")
	}

	id2 := mustBindingID(t)
	c2, err := f.For(bindingFor(id2, root))
	if err != nil {
		t.Fatalf("For(id2) error = %v", err)
	}
	if got := len(f.memo); got != 2 {
		t.Fatalf("memo size after a new LoopID = %d, want 2 (fresh entry)", got)
	}
	if c1a.BashRunner != nil && c1a.BashRunner == c2.BashRunner {
		t.Error("different LoopIDs shared the SAME executor, want distinct")
	}
}

// TestConfineFactoryMemoBoundedByQuota proves the per-LoopID memo never grows past the spawn
// quota, so a pathological number of distinct binds cannot grow it without bound.
func TestConfineFactoryMemoBoundedByQuota(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	f := newConfineFactory(operatorRoleMode)
	for range operatorSpawnQuota + 8 {
		if _, err := f.For(bindingFor(mustBindingID(t), root)); err != nil {
			t.Fatalf("For() error = %v", err)
		}
	}
	if got := len(f.memo); got > operatorSpawnQuota {
		t.Errorf("memo size = %d, want ≤ quota %d (bounded)", got, operatorSpawnQuota)
	}
}

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

// fakeGuaranteeRunner is a tool.CommandRunner exposing GuaranteeBits — the structural
// probe the posture interlock reads. It lets a test drive the edit/Bash auto-approve
// gate with a CHOSEN guarantee mask WITHOUT a real OS backend, so the interlock
// behavior is proven deterministically on any host (macOS Seatbelt OR the Linux null
// backend). Mirrors the harness posture_test fake.
type fakeGuaranteeRunner struct{ bits uint64 }

func (fakeGuaranteeRunner) RunCommand(context.Context, string, string) ([]byte, int, error) {
	return nil, 0, nil
}
func (f fakeGuaranteeRunner) GuaranteeBits() uint64 { return f.bits }

var _ tool.CommandRunner = fakeGuaranteeRunner{}

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
			st.Set(ceiling.Level(tt.ceiling))
			e := effectiveModeSource{role: tt.role, ceiling: st}
			if got := uint8(e.Current()); got != tt.want {
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
	if got := uint8(e.Current()); got != uint8(sandbox.ZeroTrust) {
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
	st.Set(ceiling.Level(sandbox.Write))
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
	st.Set(ceiling.Level(sandbox.Write))
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
	st.Set(ceiling.Level(sandbox.Write))
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
	st.Set(ceiling.Level(sandbox.Write))
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

// TestWriteModeEditAutoApproveIsOSGated is the fix's proof: under the Write posture a
// file-EDIT/write tool auto-approves ONLY when the held runner actually enforces the
// OS write-boundary (GuaranteeWriteBoundary). A runner WITHOUT that bit — or a nil
// runner (the null-backend / executor-build fallback) — fails the edit interlock and
// the edit falls to Ask. So on the Ubuntu null backend edits Ask; on macOS Seatbelt
// (which enforces WriteBoundary) they auto-approve. Before this fix edits auto-approved
// with NO OS write-boundary. Mirrors the Bash-interlock proofs
// (TestFallbackConfinementKeepsBashGated / TestConfinedCheckerAutoApprovesTrivialBashAtWrite).
func TestWriteModeEditAutoApproveIsOSGated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		runner tool.CommandRunner
		want   loop.Effect
	}{
		{
			name:   "runner enforces WriteBoundary -> auto-approve (macOS Seatbelt path)",
			runner: fakeGuaranteeRunner{bits: sandbox.GuaranteeWriteBoundary},
			want:   loop.EffectAutoApprove,
		},
		{
			name:   "runner enforces the full write bash mask (incl. WriteBoundary) -> auto-approve",
			runner: fakeGuaranteeRunner{bits: writeBashGuarantees},
			want:   loop.EffectAutoApprove,
		},
		{
			name:   "runner WITHOUT WriteBoundary -> ask (OS not enforcing writes)",
			runner: fakeGuaranteeRunner{bits: sandbox.GuaranteeEnvScrub | sandbox.GuaranteeReadDenies},
			want:   loop.EffectAsk,
		},
		{
			name:   "nil runner (null backend / build-failure fallback) -> ask",
			runner: nil,
			want:   loop.EffectAsk,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			st := ceiling.New()
			st.Set(ceiling.Level(sandbox.Write))
			effSrc := effectiveModeSource{role: operatorRoleMode, ceiling: st}
			// The exact confinement shape a build site produces, but with a chosen fake
			// runner so the edit interlock is exercised deterministically on any host.
			conf := confine.Confinement{CheckerOption: tools.WithCeilingPostures(effSrc, postureTable(), tt.runner)}
			pc := newConfinedChecker(t, root, conf)
			edit := boundEditFile(t, root)
			// a.txt is inside the workspace, so containment clears and ONLY the posture
			// edit interlock decides auto-approve vs Ask.
			if eff := pc.Check(context.Background(), edit, "EditFile", `{"path":"a.txt"}`); eff != tt.want {
				t.Errorf("Check(EditFile a.txt) @ Write = %v, want %v (edit auto-approve must be OS-gated)", eff, tt.want)
			}
		})
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
	st.Set(ceiling.Level(sandbox.Write))
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
	st.Set(ceiling.Level(sandbox.ReadOnly))
	if eff := pc.Check(context.Background(), bash, "Bash", `{"command":"ls"}`); eff == loop.EffectAutoApprove {
		t.Error("Check(Bash ls) after lowering ceiling to ReadOnly = EffectAutoApprove, want Ask (live clamp)")
	}
}

// TestSubagentClampBoundsChildToParentEffective proves the non-escalation clamp: a
// child whose static role is MORE permissive than its parent's effective mode is
// clamped to the parent's effective mode — a runtime spawn can never elevate.
func TestSubagentClampBoundsChildToParentEffective(t *testing.T) {
	t.Parallel()

	// Session ceiling ReadOnly makes the parent operator primer's effective mode ReadOnly
	// (min(Write, ReadOnly)) — the parent-effective source the spawner threads down.
	st := ceiling.New()
	st.Set(ceiling.Level(sandbox.ReadOnly))
	parentEff := effectiveModeSource{role: operatorRoleMode, ceiling: st}
	if got := uint8(parentEff.Current()); got != uint8(sandbox.ReadOnly) {
		t.Fatalf("parent effective = %d, want ReadOnly (min(Write, ReadOnly))", got)
	}

	// A child operator LEAF (static role Write) spawned under this parent: its ceiling
	// is the parent's effective source, so its effective is min(Write, ReadOnly) =
	// ReadOnly — the child cannot exceed the parent even though its role is Write.
	childEff := effectiveModeSource{role: operatorRoleMode, ceiling: parentEff}
	if got := uint8(childEff.Current()); got != uint8(sandbox.ReadOnly) {
		t.Errorf("child (role Write) effective under a ReadOnly-effective parent = %d, want ReadOnly (clamped, no escalation)", got)
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

// testWorkspaceCoordinator is a no-op tool.WorkspaceCoordinator for direct-Build unit tests
// (the real one is supplied by the rig at bind time). testWorkspacePermit satisfies the
// permit contract it returns.
type testWorkspaceCoordinator struct{}

func (*testWorkspaceCoordinator) Acquire(context.Context, tool.WorkspaceOperation, string) (tool.WorkspacePermit, error) {
	return testWorkspacePermit{}, nil
}
func (*testWorkspaceCoordinator) Healthy() error { return nil }

type testWorkspacePermit struct{}

func (testWorkspacePermit) Release() {}

// boundEditFile builds a real EditFile the way the harness now requires — bound through the
// tools.Files bundle with a fresh observation set — since NewEditFile's observation argument
// is unexported. Only the tool's "EditFile" identity + workspace root matter to the posture
// edit-interlock the caller exercises.
func boundEditFile(t *testing.T, root string) tool.InvokableTool {
	t.Helper()
	guard, err := tools.NewPermissionChecker(tools.PermissionPolicy{HardDeny: tools.DefaultHardDeny()})
	if err != nil {
		t.Fatalf("NewPermissionChecker(guard) error = %v", err)
	}
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	built, err := tools.Files(guard).Build(context.Background(), tool.Bindings{
		SessionID: id,
		LoopID:    id,
		Workspace: &tool.WorkspaceBinding{Root: root, Coordinator: &testWorkspaceCoordinator{}, Observations: tools.NewObservations()},
	})
	if err != nil {
		t.Fatalf("tools.Files().Build() error = %v", err)
	}
	for _, tl := range built {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		if info.Name == "EditFile" {
			return tl
		}
	}
	t.Fatal("tools.Files produced no EditFile")
	return nil
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
