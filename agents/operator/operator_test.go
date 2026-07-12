package operator

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/tools"
	"github.com/looprig/swe/confine"
)

// ---- test doubles -----------------------------------------------------------

// fakeSkillTool is a minimal tool.InvokableTool named "Skill" a fake Skill
// DEFINITION builds per bind. The real tools.Skill is unit-tested in the tools
// package; here we only assert the leaf wires an injected skill DEFINITION.
type fakeSkillTool struct{}

func (fakeSkillTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "Skill", Desc: "fake", Schema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (fakeSkillTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("fake"), nil
}

// fakeSkillDef is the injected per-bind Skill definition (RequiresWorkspace so it
// reads the bound root, mirroring the real tools.NewSkill(WithWorkspaceRoot)).
func fakeSkillDef() tool.Definition {
	return tool.NewDefinition("Skill", tool.RequiresWorkspace, func(_ context.Context, b tool.Bindings) ([]tool.InvokableTool, error) {
		_ = b.Workspace.Root // proves the skill definition reads the bound root
		return []tool.InvokableTool{fakeSkillTool{}}, nil
	})
}

// stubRunner / stubArgv are distinct-per-bind confined runner instances the stub
// confine.Factory hands back, so a test can assert fresh executors per binding.
type stubRunner struct{ loop uuid.UUID }

func (stubRunner) RunCommand(context.Context, string, string) ([]byte, int, error) {
	return nil, 0, nil
}

type stubArgv struct{ loop uuid.UUID }

func (stubArgv) RunArgv(context.Context, string, []string) ([]byte, int, error) { return nil, 0, nil }

// stubConfFactory is a fresh-per-call confine.Factory. It records every bind it is
// asked about and returns a Confinement carrying distinct runner instances, so a
// test can prove the leaf reads the factory per bind (fresh executor per binding).
// CheckerOption is left nil (no posture) so the wired gate is the bare checker —
// the posture path is swarms/swe's concern (confinement_test), not the leaf's.
type stubConfFactory struct {
	mu      sync.Mutex
	roots   []string
	runners []tool.CommandRunner
}

func (f *stubConfFactory) For(b tool.Bindings) (confine.Confinement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	root := ""
	if b.Workspace != nil {
		root = b.Workspace.Root
	}
	r := stubRunner{loop: b.LoopID}
	f.roots = append(f.roots, root)
	f.runners = append(f.runners, r)
	return confine.Confinement{BashRunner: r, GrepRunner: stubArgv{loop: b.LoopID}}, nil
}

func (f *stubConfFactory) seenRoot(root string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.roots {
		if r == root {
			return true
		}
	}
	return false
}

func (f *stubConfFactory) distinctRunners() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := make(map[tool.CommandRunner]struct{}, len(f.runners))
	for _, r := range f.runners {
		seen[r] = struct{}{}
	}
	return len(seen)
}

// fakePermit / fakeCoordinator satisfy the workspace-mutation seam Build validates
// (RequiresWorkspace needs a healthy coordinator). The leaf's Build-time path never
// acquires a permit; behavioral ReadFile never touches the coordinator.
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

// bindingsFor builds a full workspace binding rooted at root (fresh ids + ceiling).
func bindingsFor(t *testing.T, root string) tool.Bindings {
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

func testHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}},
	}
}

// bindAll builds every definition against b and returns the flattened tool slice.
func bindAll(t *testing.T, defs []tool.Definition, b tool.Bindings) []tool.InvokableTool {
	t.Helper()
	var out []tool.InvokableTool
	for _, d := range defs {
		built, err := d.Build(context.Background(), b)
		if err != nil {
			t.Fatalf("Build(%s) error = %v", d.Name(), err)
		}
		out = append(out, built...)
	}
	return out
}

func toolNames(t *testing.T, reg []tool.InvokableTool) []string {
	t.Helper()
	names := make([]string, 0, len(reg))
	for _, tl := range reg {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		names = append(names, info.Name)
	}
	sort.Strings(names)
	return names
}

func byName(t *testing.T, reg []tool.InvokableTool) map[string]tool.InvokableTool {
	t.Helper()
	m := make(map[string]tool.InvokableTool, len(reg))
	for _, tl := range reg {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		m[info.Name] = tl
	}
	return m
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- roster / produced-name tests ------------------------------------------

// TestBuildToolsRoster proves operator defines EXACTLY its allowlist as
// per-binding definitions (ReadFile, Glob, Grep, WriteFile, EditFile, Bash,
// WebSearch, Fetch, Todo, AskUser) — the investigate+implement leaf — with a
// fresh-per-bind permission factory and a stable policy revision, and NO Subagent
// (a leaf cannot spawn).
func TestBuildToolsRoster(t *testing.T) {
	t.Parallel()

	tls, err := BuildTools(&stubConfFactory{}, testHTTPClient(), nil)
	if err != nil {
		t.Fatalf("BuildTools() error = %v", err)
	}
	if tls.Permission == nil {
		t.Fatal("BuildTools() Tools.Permission = nil, want a PermissionFactory")
	}
	if strings.TrimSpace(tls.PolicyRevision) == "" {
		t.Fatal("BuildTools() Tools.PolicyRevision is empty, want a stable revision")
	}

	got := toolNames(t, bindAll(t, tls.Definitions, bindingsFor(t, t.TempDir())))
	want := []string{"AskUser", "Bash", "EditFile", "Fetch", "Glob", "Grep", "ReadFile", "Todo", "WebSearch", "WriteFile"}
	if !equalStrings(got, want) {
		t.Errorf("bound tool names = %v, want %v", got, want)
	}
	for _, n := range got {
		if n == "Subagent" {
			t.Fatal("operator wired a Subagent tool; a leaf must not be able to spawn")
		}
	}

	// operator IS the implementer: it MUST carry write/edit.
	set := make(map[string]struct{}, len(got))
	for _, n := range got {
		set[n] = struct{}{}
	}
	for _, n := range []string{"WriteFile", "EditFile"} {
		if _, ok := set[n]; !ok {
			t.Errorf("operator is missing %q (it implements — it must write/edit)", n)
		}
	}
}

// TestProducedNamesMatchBuilt proves every definition's declared ProducedToolNames
// exactly equals the Info().Name set it builds — catching stale bundle metadata
// (Build enforces this, but we assert it explicitly per the plan).
func TestProducedNamesMatchBuilt(t *testing.T) {
	t.Parallel()

	tls, err := BuildTools(&stubConfFactory{}, testHTTPClient(), fakeSkillDef())
	if err != nil {
		t.Fatalf("BuildTools() error = %v", err)
	}
	b := bindingsFor(t, t.TempDir())
	for _, d := range tls.Definitions {
		built, err := d.Build(context.Background(), b)
		if err != nil {
			t.Fatalf("Build(%s) error = %v", d.Name(), err)
		}
		declared := append([]string(nil), d.ProducedToolNames()...)
		sort.Strings(declared)
		actual := toolNames(t, built)
		if !equalStrings(declared, actual) {
			t.Errorf("definition %q produced names %v, built %v", d.Name(), declared, actual)
		}
	}
}

// TestBindingIsolation binds every workspace definition twice with different roots
// and loop ids and proves the leaf captures nothing session-specific: each file/
// Bash tool uses its OWN bound root, the confine.Factory is read per bind (fresh
// executor + read-only view per binding), and the permission factory yields a
// FRESH gate instance per bind.
func TestBindingIsolation(t *testing.T) {
	t.Parallel()

	confFactory := &stubConfFactory{}
	tls, err := BuildTools(confFactory, testHTTPClient(), nil)
	if err != nil {
		t.Fatalf("BuildTools() error = %v", err)
	}

	rootA, rootB := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(rootA, "a.txt"), []byte("AAA"), 0o600); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "b.txt"), []byte("BBB"), 0o600); err != nil {
		t.Fatalf("seed b: %v", err)
	}
	bindA, bindB := bindingsFor(t, rootA), bindingsFor(t, rootB)

	regA := byName(t, bindAll(t, tls.Definitions, bindA))
	regB := byName(t, bindAll(t, tls.Definitions, bindB))

	// Each ReadFile uses its own root: bindA reads a.txt (in rootA) and cannot read
	// b.txt (absent from rootA); bindB is the mirror.
	readOK := func(reg map[string]tool.InvokableTool, path, want string) bool {
		res, err := reg["ReadFile"].InvokableRun(context.Background(), `{"path":"`+path+`"}`)
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", path, err)
		}
		return strings.Contains(resultText(res), want)
	}
	if !readOK(regA, "a.txt", "AAA") {
		t.Error("bindA ReadFile did not read its own root's a.txt")
	}
	if readOK(regA, "b.txt", "BBB") {
		t.Error("bindA ReadFile read bindB's file — roots are not isolated")
	}
	if !readOK(regB, "b.txt", "BBB") {
		t.Error("bindB ReadFile did not read its own root's b.txt")
	}

	// The confine.Factory was consulted for BOTH roots and returned distinct
	// executor instances per bind (fresh sandbox executor / read-only view).
	if !confFactory.seenRoot(rootA) || !confFactory.seenRoot(rootB) {
		t.Errorf("confine.Factory was not consulted per bound root (saw %v)", confFactory.roots)
	}
	if n := confFactory.distinctRunners(); n < 2 {
		t.Errorf("confine.Factory returned %d distinct runners across two binds, want >= 2 (fresh per bind)", n)
	}

	// The permission factory yields a FRESH gate per bind.
	gateA, err := tls.Permission(context.Background(), bindA)
	if err != nil {
		t.Fatalf("Permission(bindA) error = %v", err)
	}
	gateB, err := tls.Permission(context.Background(), bindB)
	if err != nil {
		t.Fatalf("Permission(bindB) error = %v", err)
	}
	if gateA == nil || gateB == nil {
		t.Fatal("permission factory returned a nil gate")
	}
	if gateA == gateB {
		t.Error("permission factory returned the SAME gate for two binds (must be fresh per bind)")
	}
}

// TestMissingWorkspaceFailsClosed proves a workspace-required definition fails
// closed (a typed *tool.MissingBindingError) when no workspace binding is present —
// never a nil/unguarded tool.
func TestMissingWorkspaceFailsClosed(t *testing.T) {
	t.Parallel()

	tls, err := BuildTools(&stubConfFactory{}, testHTTPClient(), nil)
	if err != nil {
		t.Fatalf("BuildTools() error = %v", err)
	}
	noWS := tool.Bindings{SessionID: mustUUID(t), LoopID: mustUUID(t), Ceiling: ceiling.New()}

	var checkedWorkspaceDef bool
	for _, d := range tls.Definitions {
		if d.Requirements()&tool.RequiresWorkspace == 0 {
			continue
		}
		checkedWorkspaceDef = true
		_, err := d.Build(context.Background(), noWS)
		var missing *tool.MissingBindingError
		if !errors.As(err, &missing) {
			t.Errorf("Build(%s) with no workspace = %v, want *tool.MissingBindingError", d.Name(), err)
		}
	}
	if !checkedWorkspaceDef {
		t.Fatal("no workspace-required definition found to exercise the fail-closed path")
	}
}

// ---- permission-gate behavior ----------------------------------------------

// TestGateAutoApprove proves the wired (fresh) gate auto-approves the five side-
// effect-free read/plan/ask tools and keeps the five mutating/networked tools at
// Ask (the human-gate security boundary), with no sandbox posture in play.
func TestGateAutoApprove(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tls, err := BuildTools(&stubConfFactory{}, testHTTPClient(), nil)
	if err != nil {
		t.Fatalf("BuildTools() error = %v", err)
	}
	b := bindingsFor(t, root)
	reg := byName(t, bindAll(t, tls.Definitions, b))
	gate, err := tls.Permission(context.Background(), b)
	if err != nil {
		t.Fatalf("Permission() error = %v", err)
	}

	cases := []struct {
		tool string
		args string
		want loop.Effect
	}{
		{tool: "ReadFile", args: `{"path":"f.txt"}`, want: loop.EffectAutoApprove},
		{tool: "Glob", args: `{"pattern":"*"}`, want: loop.EffectAutoApprove},
		{tool: "Grep", args: `{"pattern":"x"}`, want: loop.EffectAutoApprove},
		{tool: "Todo", args: `{"todos":[]}`, want: loop.EffectAutoApprove},
		{tool: "AskUser", args: `{"question":"q"}`, want: loop.EffectAutoApprove},
		{tool: "WriteFile", args: `{"path":"g.txt","content":"y"}`, want: loop.EffectAsk},
		{tool: "EditFile", args: `{"path":"f.txt","old":"x","new":"z"}`, want: loop.EffectAsk},
		{tool: "Bash", args: `{"command":"go test ./..."}`, want: loop.EffectAsk},
		{tool: "WebSearch", args: `{"query":"q"}`, want: loop.EffectAsk},
		{tool: "Fetch", args: `{"url":"https://example.com"}`, want: loop.EffectAsk},
	}
	for _, tc := range cases {
		tl, ok := reg[tc.tool]
		if !ok {
			t.Fatalf("tool %q not bound", tc.tool)
		}
		if eff := gate.Check(context.Background(), tl, tc.tool, tc.args); eff != tc.want {
			t.Errorf("Check(%q) = %v, want %v", tc.tool, eff, tc.want)
		}
	}
	assertAutoApproveSet(t, []string{"AskUser", "Glob", "Grep", "ReadFile", "Todo"})
}

// TestSkillWiring proves an injected Skill definition is added to the roster AND
// auto-approves through the fresh gate ("Skill" is in HardApprove only when the
// definition is present); a nil skill wires neither the tool nor the approval.
func TestSkillWiring(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	withSkill, err := BuildTools(&stubConfFactory{}, testHTTPClient(), fakeSkillDef())
	if err != nil {
		t.Fatalf("BuildTools(skill) error = %v", err)
	}
	b := bindingsFor(t, root)
	reg := byName(t, bindAll(t, withSkill.Definitions, b))
	if _, ok := reg["Skill"]; !ok {
		t.Fatal("Skill definition not wired into the roster")
	}
	gate, err := withSkill.Permission(context.Background(), b)
	if err != nil {
		t.Fatalf("Permission() error = %v", err)
	}
	if eff := gate.Check(context.Background(), reg["Skill"], "Skill", `{"name":"code-style"}`); eff != loop.EffectAutoApprove {
		t.Errorf("Check(Skill) = %v, want %v (Skill must auto-approve when wired)", eff, loop.EffectAutoApprove)
	}

	noSkill, err := BuildTools(&stubConfFactory{}, testHTTPClient(), nil)
	if err != nil {
		t.Fatalf("BuildTools(nil) error = %v", err)
	}
	b2 := bindingsFor(t, root)
	regNo := byName(t, bindAll(t, noSkill.Definitions, b2))
	if _, ok := regNo["Skill"]; ok {
		t.Fatal("nil skill still wired a Skill tool")
	}
	gate2, err := noSkill.Permission(context.Background(), b2)
	if err != nil {
		t.Fatalf("Permission() error = %v", err)
	}
	if eff := gate2.Check(context.Background(), fakeSkillTool{}, "Skill", `{"name":"code-style"}`); eff == loop.EffectAutoApprove {
		t.Error("Skill auto-approved with no Skill definition wired (want Ask)")
	}
}

// TestPolicyRevisionStable proves the policy revision is deterministic across binds
// of the same inputs and CHANGES when the policy changes (adding the Skill tool).
func TestPolicyRevisionStable(t *testing.T) {
	t.Parallel()

	a, err := BuildTools(&stubConfFactory{}, testHTTPClient(), nil)
	if err != nil {
		t.Fatalf("BuildTools() error = %v", err)
	}
	b, err := BuildTools(&stubConfFactory{}, testHTTPClient(), nil)
	if err != nil {
		t.Fatalf("BuildTools() error = %v", err)
	}
	if a.PolicyRevision != b.PolicyRevision {
		t.Errorf("policy revision not stable across identical builds: %q vs %q", a.PolicyRevision, b.PolicyRevision)
	}
	withSkill, err := BuildTools(&stubConfFactory{}, testHTTPClient(), fakeSkillDef())
	if err != nil {
		t.Fatalf("BuildTools(skill) error = %v", err)
	}
	if withSkill.PolicyRevision == a.PolicyRevision {
		t.Error("policy revision did not change when the Skill tool was added to the policy")
	}
}

// TestBuildToolsFailsClosedOnUnresolvableHome proves the fail-secure contract: when
// the read guard's PermissionChecker cannot be built ($HOME unresolvable while
// DefaultHardDeny's "~/…" patterns require it) BuildTools fails CLOSED with a typed
// *ToolSetError unwrapping *tools.HomeUnresolvableError, returning the zero Tools.
func TestBuildToolsFailsClosedOnUnresolvableHome(t *testing.T) {
	// NOT t.Parallel(): HOME is process-global and t.Setenv panics under t.Parallel.
	t.Setenv("HOME", "")

	tls, err := BuildTools(&stubConfFactory{}, testHTTPClient(), nil)

	var tse *ToolSetError
	if !errors.As(err, &tse) {
		t.Fatalf("BuildTools() error = %T (%v), want *ToolSetError", err, err)
	}
	var hue *tools.HomeUnresolvableError
	if !errors.As(err, &hue) {
		t.Fatalf("BuildTools() error does not unwrap to *tools.HomeUnresolvableError: %v", err)
	}
	// The shared leafrig error carries operator's name so its message keeps the leaf prefix.
	if tse.Agent != string(Name) {
		t.Errorf("ToolSetError.Agent = %q, want %q", tse.Agent, string(Name))
	}
	if tls.Definitions != nil || tls.Permission != nil || tls.PolicyRevision != "" {
		t.Errorf("BuildTools() returned a non-zero Tools on failure (want fail-closed): %+v", tls)
	}
}

func assertAutoApproveSet(t *testing.T, want []string) {
	t.Helper()
	got := append([]string(nil), autoApprovedTools...)
	sort.Strings(got)
	w := append([]string(nil), want...)
	sort.Strings(w)
	if !equalStrings(got, w) {
		t.Errorf("autoApprovedTools = %v, want %v", got, w)
	}
}

// resultText extracts the concatenated text of a tool result for content assertions.
func resultText(res *tool.ToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range res.Content {
		if tb, ok := blk.(*content.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

// ---- immutable-boundary tests (unchanged behavior) --------------------------

func TestName(t *testing.T) {
	t.Parallel()
	if Name != identity.AgentName("operator") {
		t.Errorf("Name = %q, want %q", Name, "operator")
	}
}

func TestDescriptionNonEmpty(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(Description) == "" {
		t.Fatal("Description is empty")
	}
}

func TestRoleContent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want string
	}{
		{name: "root cause", want: "root cause"},
		{name: "read before editing", want: "read it first"},
		{name: "prefer editing to creating", want: "prefer editing"},
		{name: "states the plan", want: "plan"},
		{name: "approval-gated mutation", want: "approval"},
		{name: "narrowest test first", want: "narrowest test"},
		{name: "does not fix unrelated", want: "unrelated"},
		{name: "does web research", want: "web"},
		{name: "uses WebSearch/Fetch", want: "WebSearch"},
		{name: "cites sources", want: "cite"},
		{name: "fetched content is untrusted data", want: "DATA"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(strings.ToLower(Role), strings.ToLower(tt.want)) {
				t.Errorf("Role is missing %q", tt.want)
			}
		})
	}
}

func TestRoleIsWellFormedXML(t *testing.T) {
	t.Parallel()
	var probe struct {
		XMLName xml.Name `xml:"role"`
		RoleNm  string   `xml:"name,attr"`
	}
	if err := xml.Unmarshal([]byte(Role), &probe); err != nil {
		t.Fatalf("Role is not well-formed XML: %v", err)
	}
	if probe.RoleNm != "operator" {
		t.Errorf("Role name attr = %q, want %q", probe.RoleNm, "operator")
	}
}
