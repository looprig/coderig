package reviewer

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

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

type fakeSkillTool struct{}

func (fakeSkillTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "Skill", Desc: "fake", Schema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (fakeSkillTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("fake"), nil
}

func fakeSkillDef() tool.Definition {
	return tool.NewDefinition("Skill", tool.RequiresWorkspace, func(_ context.Context, b tool.Bindings) ([]tool.InvokableTool, error) {
		_ = b.Workspace.Root
		return []tool.InvokableTool{fakeSkillTool{}}, nil
	})
}

type stubRunner struct{ loop uuid.UUID }

func (stubRunner) RunCommand(context.Context, string, string) ([]byte, int, error) {
	return nil, 0, nil
}

type stubArgv struct{ loop uuid.UUID }

func (stubArgv) RunArgv(context.Context, string, []string) ([]byte, int, error) { return nil, 0, nil }

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

// ---- roster / read-only tests ----------------------------------------------

// TestBuildToolsRoster proves reviewer defines EXACTLY its allowlist (ReadFile,
// Glob, Grep, Bash, Todo, AskUser) — critique with the ability to run tests/build
// via Bash — with NO write/edit tool (it never mutates) and NO Subagent (a leaf
// cannot spawn).
func TestBuildToolsRoster(t *testing.T) {
	t.Parallel()

	tls, err := BuildTools(&stubConfFactory{}, nil)
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
	want := []string{"AskUser", "Bash", "Glob", "Grep", "ReadFile", "Todo"}
	if !equalStrings(got, want) {
		t.Errorf("bound tool names = %v, want %v", got, want)
	}
	for _, n := range got {
		switch n {
		case "Subagent":
			t.Fatal("reviewer wired a Subagent tool; a leaf must not be able to spawn")
		case "WriteFile", "EditFile":
			t.Errorf("reviewer wired %q; it critiques, it must not mutate", n)
		}
	}
}

// TestProducedNamesMatchBuilt proves every definition's declared ProducedToolNames
// exactly equals the Info().Name set it builds (catches stale bundle metadata) —
// including the read-only ReadFile definition that wraps tools.Files and returns
// ONLY ReadFile.
func TestProducedNamesMatchBuilt(t *testing.T) {
	t.Parallel()

	tls, err := BuildTools(&stubConfFactory{}, fakeSkillDef())
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

// TestReadOnlyReadFile proves the read-only ReadFile definition (which wraps
// tools.Files because harness has no read-only files definition) builds a WORKING
// ReadFile bound to the workspace root and never exposes Write/Edit.
func TestReadOnlyReadFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tls, err := BuildTools(&stubConfFactory{}, nil)
	if err != nil {
		t.Fatalf("BuildTools() error = %v", err)
	}
	reg := byName(t, bindAll(t, tls.Definitions, bindingsFor(t, root)))
	rf, ok := reg["ReadFile"]
	if !ok {
		t.Fatal("ReadFile not wired")
	}
	res, err := rf.InvokableRun(context.Background(), `{"path":"f.txt"}`)
	if err != nil {
		t.Fatalf("ReadFile run error = %v", err)
	}
	if !strings.Contains(resultText(res), "hello") {
		t.Errorf("ReadFile did not return the file contents: %q", resultText(res))
	}
	if _, ok := reg["WriteFile"]; ok {
		t.Error("reviewer exposed WriteFile")
	}
	if _, ok := reg["EditFile"]; ok {
		t.Error("reviewer exposed EditFile")
	}
}

// TestBindingIsolation binds every workspace definition twice and proves each
// ReadFile uses its own bound root, the confine.Factory is read per bind (fresh
// executor per binding), and the permission factory yields a fresh gate per bind.
func TestBindingIsolation(t *testing.T) {
	t.Parallel()

	confFactory := &stubConfFactory{}
	tls, err := BuildTools(confFactory, nil)
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

	readOK := func(reg map[string]tool.InvokableTool, path, want string) bool {
		res, err := reg["ReadFile"].InvokableRun(context.Background(), `{"path":"`+path+`"}`)
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", path, err)
		}
		return strings.Contains(resultText(res), want)
	}
	if !readOK(regA, "a.txt", "AAA") {
		t.Error("bindA ReadFile did not read its own root")
	}
	if readOK(regA, "b.txt", "BBB") {
		t.Error("bindA ReadFile read bindB's file — roots are not isolated")
	}
	if !readOK(regB, "b.txt", "BBB") {
		t.Error("bindB ReadFile did not read its own root")
	}
	if !confFactory.seenRoot(rootA) || !confFactory.seenRoot(rootB) {
		t.Errorf("confine.Factory was not consulted per bound root (saw %v)", confFactory.roots)
	}
	if n := confFactory.distinctRunners(); n < 2 {
		t.Errorf("confine.Factory returned %d distinct runners across two binds, want >= 2", n)
	}

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
// closed with a typed *tool.MissingBindingError when no workspace binding exists.
func TestMissingWorkspaceFailsClosed(t *testing.T) {
	t.Parallel()

	tls, err := BuildTools(&stubConfFactory{}, nil)
	if err != nil {
		t.Fatalf("BuildTools() error = %v", err)
	}
	noWS := tool.Bindings{SessionID: mustUUID(t), LoopID: mustUUID(t), Ceiling: ceiling.New()}
	var checked bool
	for _, d := range tls.Definitions {
		if d.Requirements()&tool.RequiresWorkspace == 0 {
			continue
		}
		checked = true
		_, err := d.Build(context.Background(), noWS)
		var missing *tool.MissingBindingError
		if !errors.As(err, &missing) {
			t.Errorf("Build(%s) with no workspace = %v, want *tool.MissingBindingError", d.Name(), err)
		}
	}
	if !checked {
		t.Fatal("no workspace-required definition found")
	}
}

// TestGateAutoApprove proves the fresh gate auto-approves the read/todo/ask tools
// and keeps Bash at Ask (reviewer runs a shell only under human approval), with no
// sandbox posture in play.
func TestGateAutoApprove(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tls, err := BuildTools(&stubConfFactory{}, nil)
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
		{tool: "Bash", args: `{"command":"go test ./..."}`, want: loop.EffectAsk},
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

// TestSkillWiring proves an injected Skill definition joins the roster and auto-
// approves; a nil skill wires neither.
func TestSkillWiring(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	withSkill, err := BuildTools(&stubConfFactory{}, fakeSkillDef())
	if err != nil {
		t.Fatalf("BuildTools(skill) error = %v", err)
	}
	b := bindingsFor(t, root)
	reg := byName(t, bindAll(t, withSkill.Definitions, b))
	if _, ok := reg["Skill"]; !ok {
		t.Fatal("Skill definition not wired")
	}
	gate, err := withSkill.Permission(context.Background(), b)
	if err != nil {
		t.Fatalf("Permission() error = %v", err)
	}
	if eff := gate.Check(context.Background(), reg["Skill"], "Skill", `{"name":"code-style"}`); eff != loop.EffectAutoApprove {
		t.Errorf("Check(Skill) = %v, want %v", eff, loop.EffectAutoApprove)
	}

	noSkill, err := BuildTools(&stubConfFactory{}, nil)
	if err != nil {
		t.Fatalf("BuildTools(nil) error = %v", err)
	}
	b2 := bindingsFor(t, root)
	regNo := byName(t, bindAll(t, noSkill.Definitions, b2))
	if _, ok := regNo["Skill"]; ok {
		t.Fatal("nil skill still wired a Skill tool")
	}
}

// TestPolicyRevisionStable proves the revision is deterministic across identical
// builds and changes when the policy changes (adding Skill).
func TestPolicyRevisionStable(t *testing.T) {
	t.Parallel()

	a, err := BuildTools(&stubConfFactory{}, nil)
	if err != nil {
		t.Fatalf("BuildTools() error = %v", err)
	}
	b, err := BuildTools(&stubConfFactory{}, nil)
	if err != nil {
		t.Fatalf("BuildTools() error = %v", err)
	}
	if a.PolicyRevision != b.PolicyRevision {
		t.Errorf("policy revision not stable: %q vs %q", a.PolicyRevision, b.PolicyRevision)
	}
	withSkill, err := BuildTools(&stubConfFactory{}, fakeSkillDef())
	if err != nil {
		t.Fatalf("BuildTools(skill) error = %v", err)
	}
	if withSkill.PolicyRevision == a.PolicyRevision {
		t.Error("policy revision did not change when the Skill tool was added")
	}
}

// TestBuildToolsFailsClosedOnUnresolvableHome proves BuildTools fails CLOSED with a
// typed *ToolSetError unwrapping *tools.HomeUnresolvableError when the read guard's
// checker cannot be built ($HOME unresolvable while "~/…" deny patterns require it).
func TestBuildToolsFailsClosedOnUnresolvableHome(t *testing.T) {
	t.Setenv("HOME", "")

	tls, err := BuildTools(&stubConfFactory{}, nil)

	var tse *ToolSetError
	if !errors.As(err, &tse) {
		t.Fatalf("BuildTools() error = %T (%v), want *ToolSetError", err, err)
	}
	var hue *tools.HomeUnresolvableError
	if !errors.As(err, &hue) {
		t.Fatalf("BuildTools() error does not unwrap to *tools.HomeUnresolvableError: %v", err)
	}
	if tse.Agent != string(Name) {
		t.Errorf("ToolSetError.Agent = %q, want %q", tse.Agent, string(Name))
	}
	if tls.Definitions != nil || tls.Permission != nil || tls.PolicyRevision != "" {
		t.Errorf("BuildTools() returned a non-zero Tools on failure: %+v", tls)
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

// ---- immutable-boundary tests ----------------------------------------------

func TestName(t *testing.T) {
	t.Parallel()
	if Name != identity.AgentName("reviewer") {
		t.Errorf("Name = %q, want %q", Name, "reviewer")
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
		{name: "critiques", want: "critique"},
		{name: "does not fix", want: "not fix"},
		{name: "may run tests", want: "test"},
		{name: "reports findings", want: "findings"},
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
	if probe.RoleNm != "reviewer" {
		t.Errorf("Role name attr = %q, want %q", probe.RoleNm, "reviewer")
	}
}
