package leafrig

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/tools"
	"github.com/looprig/swe/confine"
)

// ---- test doubles -----------------------------------------------------------

// stubConfFactory returns a zero Confinement (no posture, no runner) for any bind —
// enough to exercise the shared mechanism without the sandbox module. The bare
// checker it yields auto-approves the hard-approve set and asks for everything else.
type stubConfFactory struct{}

func (stubConfFactory) For(tool.Bindings) (confine.Confinement, error) {
	return confine.Confinement{}, nil
}

type fakePermit struct{}

func (fakePermit) Release() {}

type fakeCoordinator struct{}

func (fakeCoordinator) Acquire(context.Context, tool.WorkspaceOperation, string) (tool.WorkspacePermit, error) {
	return fakePermit{}, nil
}
func (fakeCoordinator) Healthy() error { return nil }

func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}

func workspaceBindings(t *testing.T, root string) tool.Bindings {
	t.Helper()
	return tool.Bindings{
		SessionID: mustUUID(t),
		LoopID:    mustUUID(t),
		Ceiling:   ceiling.New(),
		Workspace: &tool.WorkspaceBinding{
			Root:         root,
			Coordinator:  fakeCoordinator{},
			Observations: tools.NewObservations(),
		},
	}
}

// buildTool builds def against b and returns its single tool (the definitions under
// test each build exactly one).
func buildTool(t *testing.T, def tool.Definition, b tool.Bindings) tool.InvokableTool {
	t.Helper()
	built, err := def.Build(context.Background(), b)
	if err != nil {
		t.Fatalf("Build(%s) error = %v", def.Name(), err)
	}
	if len(built) != 1 {
		t.Fatalf("Build(%s) produced %d tools, want 1", def.Name(), len(built))
	}
	return built[0]
}

// ---- PolicyRevision ---------------------------------------------------------

// TestPolicyRevisionStableAndSensitive proves the cross-agent policy identity is
// deterministic for identical inputs and changes for any policy-affecting change
// (agent name, approved set, or produced tool roster) — the property
// loop.WithPolicyRevision depends on.
func TestPolicyRevisionStableAndSensitive(t *testing.T) {
	t.Parallel()

	guard, err := NewReadGuard()
	if err != nil {
		t.Fatalf("NewReadGuard() error = %v", err)
	}
	base := []tool.Definition{GlobDefinition(guard), GrepDefinition(guard, stubConfFactory{}), TodoDefinition()}
	baseApproved := []string{"Glob", "Grep", "Todo"}
	baseRev := PolicyRevision("operator", baseApproved, base)

	tests := []struct {
		name       string
		agent      string
		approved   []string
		defs       []tool.Definition
		wantChange bool // true => revision must differ from baseRev
	}{
		{name: "identical inputs are stable", agent: "operator", approved: baseApproved, defs: base, wantChange: false},
		{name: "reordered approved is stable (sorted)", agent: "operator", approved: []string{"Todo", "Glob", "Grep"}, defs: base, wantChange: false},
		{name: "different agent changes it", agent: "reviewer", approved: baseApproved, defs: base, wantChange: true},
		{name: "added approved name changes it", agent: "operator", approved: append(append([]string(nil), baseApproved...), "Skill"), defs: base, wantChange: true},
		{name: "added tool definition changes it", agent: "operator", approved: baseApproved, defs: append(append([]tool.Definition(nil), base...), AskUserDefinition()), wantChange: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := PolicyRevision(tt.agent, tt.approved, tt.defs)
			if got == "" {
				t.Fatal("PolicyRevision returned empty")
			}
			if changed := got != baseRev; changed != tt.wantChange {
				t.Errorf("PolicyRevision change=%v, want %v (got=%s base=%s)", changed, tt.wantChange, got, baseRev)
			}
		})
	}
}

// TestPolicyRevisionMachineIndependent locks the "stable identity" contract against a
// silent regression to a machine-specific digest: the hard-deny patterns the revision
// hashes MUST stay unexpanded "~/…" literals (DefaultHardDeny never resolves $HOME), so
// two machines with different homes compute the SAME revision. If a future change
// expanded ~ into the absolute home before hashing, this fails.
func TestPolicyRevisionMachineIndependent(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		t.Skip("home dir unresolvable; cannot check for home-expansion leakage")
	}
	deny := tools.DefaultHardDeny()
	all := append(append([]string(nil), deny.DeniedReadPaths...), deny.DeniedWritePaths...)

	var sawHomeLiteral bool
	for _, p := range all {
		if strings.HasPrefix(p, "~/") {
			sawHomeLiteral = true
		}
		if strings.Contains(p, home) {
			t.Errorf("deny pattern %q contains the absolute home %q — PolicyRevision would be machine-specific", p, home)
		}
	}
	if !sawHomeLiteral {
		t.Error("expected at least one unexpanded ~/ deny literal (the machine-independence guard)")
	}
}

// ---- shared permission factory ---------------------------------------------

// TestNewPermissionFactoryGate proves the shared fresh-per-bind gate auto-approves a
// hard-approved tool and asks for one outside the set, and that each call yields a
// DISTINCT gate instance (independent approval state per bind).
func TestNewPermissionFactoryGate(t *testing.T) {
	t.Parallel()

	guard, err := NewReadGuard()
	if err != nil {
		t.Fatalf("NewReadGuard() error = %v", err)
	}
	approved := []string{"Glob", "Grep", "Todo"} // Bash deliberately excluded
	factory := NewPermissionFactory("operator", stubConfFactory{}, approved)

	root := t.TempDir()
	bindA := workspaceBindings(t, root)
	gateA, err := factory(context.Background(), bindA)
	if err != nil {
		t.Fatalf("permission factory error = %v", err)
	}
	globTool := buildTool(t, GlobDefinition(guard), bindA)
	bashTool := buildTool(t, BashDefinition(stubConfFactory{}), bindA)

	cases := []struct {
		name string
		tool tool.InvokableTool
		tn   string
		args string
		want loop.Effect
	}{
		{name: "approved read tool auto-approves", tool: globTool, tn: "Glob", args: `{"pattern":"*"}`, want: loop.EffectAutoApprove},
		{name: "unapproved bash asks", tool: bashTool, tn: "Bash", args: `{"command":"ls"}`, want: loop.EffectAsk},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if eff := gateA.Check(context.Background(), tc.tool, tc.tn, tc.args); eff != tc.want {
				t.Errorf("Check(%q) = %v, want %v", tc.tn, eff, tc.want)
			}
		})
	}

	gateB, err := factory(context.Background(), workspaceBindings(t, root))
	if err != nil {
		t.Fatalf("permission factory error = %v", err)
	}
	if gateA == gateB {
		t.Error("permission factory returned the SAME gate for two binds (must be fresh per bind)")
	}
}

// TestNewPermissionFactoryFailsClosed proves the mandatory missing-required-field
// boundary: a workspace-less binding fails closed with a typed *MissingWorkspaceError
// carrying the agent name (never a nil, unguarded gate). Table-driven over agents so
// the error prefix is exercised for both leaves.
func TestNewPermissionFactoryFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		agent string
	}{
		{name: "operator", agent: "operator"},
		{name: "reviewer", agent: "reviewer"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			factory := NewPermissionFactory(tt.agent, stubConfFactory{}, []string{"Glob"})
			noWS := tool.Bindings{SessionID: mustUUID(t), LoopID: mustUUID(t), Ceiling: ceiling.New()}
			gate, err := factory(context.Background(), noWS)
			if gate != nil {
				t.Errorf("gate = %v, want nil (fail closed)", gate)
			}
			var missing *MissingWorkspaceError
			if !errors.As(err, &missing) {
				t.Fatalf("error = %v, want *MissingWorkspaceError", err)
			}
			if want := tt.agent + ": permission gate requires a workspace binding"; missing.Error() != want {
				t.Errorf("Error() = %q, want %q", missing.Error(), want)
			}
		})
	}
}

// ---- definition builders ----------------------------------------------------

// TestDefinitionBuildersProducedNames proves each shared builder's declared
// ProducedToolNames exactly equals the Info().Name it builds (catches stale metadata).
func TestDefinitionBuildersProducedNames(t *testing.T) {
	t.Parallel()

	guard, err := NewReadGuard()
	if err != nil {
		t.Fatalf("NewReadGuard() error = %v", err)
	}
	b := workspaceBindings(t, t.TempDir())
	tests := []struct {
		name string
		def  tool.Definition
		want string
	}{
		{name: "glob", def: GlobDefinition(guard), want: GlobToolName},
		{name: "grep", def: GrepDefinition(guard, stubConfFactory{}), want: GrepToolName},
		{name: "bash", def: BashDefinition(stubConfFactory{}), want: BashToolName},
		{name: "todo", def: TodoDefinition(), want: TodoToolName},
		{name: "askuser", def: AskUserDefinition(), want: AskUserToolName},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			declared := append([]string(nil), tt.def.ProducedToolNames()...)
			sort.Strings(declared)
			if len(declared) != 1 || declared[0] != tt.want {
				t.Fatalf("ProducedToolNames() = %v, want [%s]", declared, tt.want)
			}
			built := buildTool(t, tt.def, b)
			info, err := built.Info(context.Background())
			if err != nil {
				t.Fatalf("Info() error = %v", err)
			}
			if info.Name != tt.want {
				t.Errorf("built tool name = %q, want %q", info.Name, tt.want)
			}
		})
	}
}
