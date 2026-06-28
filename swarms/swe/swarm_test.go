package swe

import (
	"context"
	"encoding/xml"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/tools"
	"github.com/ciram-co/looprig/pkg/tui"
	"github.com/ciram-co/swe/agents/operator"
)

// operatorPrimaryArgs builds the inputs operatorPrimaryConfig / operatorPrimaryToolSet
// need from the real leaf registry — the deps, the unbound spawner + Subagent catalog,
// the skill loader (as describer), and the primary's own code-style Skill tool — exactly
// the way buildOperatorWiring assembles them. The spawner is UNBOUND (its session is nil);
// these tests only assemble + inspect the cfg/toolset and never run a turn, so no bind is
// needed.
func operatorPrimaryArgs(t *testing.T, root string) (LeafToolDeps, *swarmSpawner, []tools.SubagentCatalogEntry, skillLoaderDescriber, tool.InvokableTool) {
	t.Helper()
	deps := LeafToolDeps{Root: root, HTTPCl: &http.Client{}}
	reg, loader, err := leafRegistry(deps, Config{})
	if err != nil {
		t.Fatalf("leafRegistry() error = %v", err)
	}
	spawner := newSwarmSpawner(reg, deps, &fakeLLM{}, newModelFactory("test-key"), loader, NewRuntimeContextProvider())
	skill := buildLeafSkill(loader, operatorBuiltin(), deps, Config{})
	return deps, spawner, toolCatalog(reg), loader, skill
}

// TestNewWithClientHappy proves swe.New (via the fake-client seam) builds a usable
// tui.Agent that is releasable via Close.
func TestNewWithClientHappy(t *testing.T) {
	t.Parallel()

	agent, err := newWithClient(context.Background(), &fakeLLM{}, newModelFactory("test-key"), Config{})
	if err != nil {
		t.Fatalf("newWithClient() error = %v", err)
	}
	if agent == nil {
		t.Fatal("newWithClient() returned nil agent")
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })

	// The returned agent must satisfy the TUI surface.
	var _ tui.Agent = agent
}

// TestOperatorPrimaryConfigIsPrimaryWithIdentityRoleAndDelegation proves the primary
// operator config: its AgentName is the operator's name (so it runs AS the primary),
// and its system prompt is the shared Identity + the operator's Role + the primary-only
// delegation fragment + the trusted code-style <available_skills> catalog. It is routed
// through the shared buildOperatorWiring seam (the production assembly).
func TestOperatorPrimaryConfigIsPrimaryWithIdentityRoleAndDelegation(t *testing.T) {
	t.Parallel()

	wiring, err := buildOperatorWiring(&fakeLLM{}, newModelFactory("test-key"), "/tmp/workspace-root", Config{})
	if err != nil {
		t.Fatalf("buildOperatorWiring() error = %v", err)
	}
	cfg := wiring.cfg

	if cfg.AgentName != operator.Name {
		t.Errorf("cfg.AgentName = %q, want %q", cfg.AgentName, operator.Name)
	}
	if cfg.Client == nil {
		t.Error("cfg.Client = nil, want the supplied client")
	}
	// The system prompt begins with Identity + operator.Role + operatorDelegation; the
	// trusted code-style <available_skills> catalog follows.
	wantPrefix := Identity + operator.Role + operatorDelegation
	if !strings.HasPrefix(cfg.Model.System, wantPrefix) {
		t.Errorf("cfg.Model.System does not start with Identity+operator.Role+operatorDelegation:\n%s", cfg.Model.System)
	}
	if !strings.Contains(cfg.Model.System, "<identity product=\"SWE\">") {
		t.Error("system prompt missing the shared identity block")
	}
	if !strings.Contains(cfg.Model.System, "<role name=\"operator\">") {
		t.Error("system prompt missing the operator role block")
	}
	if !strings.Contains(cfg.Model.System, "<delegation>") {
		t.Error("system prompt missing the primary-only delegation block")
	}
	// The primary carries the trusted code-style catalog (proving the skill-catalog
	// wiring on the primary): an <available_skills> block listing code-style.
	if !strings.Contains(cfg.Model.System, "<available_skills>") || !strings.Contains(cfg.Model.System, "code-style") {
		t.Errorf("system prompt missing the code-style <available_skills> catalog:\n%s", cfg.Model.System)
	}
}

// TestOperatorPrimaryConfigCarriesRuntimeContext proves the primary operator's
// loop.Config has a non-nil RuntimeContext when one is wired (so the loop injects the
// volatile date/cwd/git tail every turn), and that a nil provider leaves it OFF.
func TestOperatorPrimaryConfigCarriesRuntimeContext(t *testing.T) {
	t.Parallel()

	t.Run("provider wired -> non-nil RuntimeContext", func(t *testing.T) {
		t.Parallel()
		deps, spawner, catalog, loader, skill := operatorPrimaryArgs(t, "/tmp/workspace-root")
		rc := NewRuntimeContextProvider()
		cfg := operatorPrimaryConfig(&fakeLLM{}, newModelFactory("test-key"), deps, spawner, catalog, rc, loader, skill)
		if cfg.RuntimeContext == nil {
			t.Error("operator cfg.RuntimeContext = nil, want the wired provider")
		}
	})

	t.Run("nil provider -> RuntimeContext stays nil (OFF)", func(t *testing.T) {
		t.Parallel()
		deps, spawner, catalog, loader, skill := operatorPrimaryArgs(t, "/tmp/workspace-root")
		cfg := operatorPrimaryConfig(&fakeLLM{}, newModelFactory("test-key"), deps, spawner, catalog, nil, loader, skill)
		if cfg.RuntimeContext != nil {
			t.Error("operator cfg.RuntimeContext != nil with a nil provider, want OFF")
		}
	})
}

// TestBuildOperatorWiringEnablesRuntimeContext proves the SHARED construction seam (used
// by New, openNew, openResume) wires a non-nil RuntimeContext onto the primary operator's
// cfg, so every construction path inherits runtime-context injection.
func TestBuildOperatorWiringEnablesRuntimeContext(t *testing.T) {
	t.Parallel()
	wiring, err := buildOperatorWiring(&fakeLLM{}, newModelFactory("test-key"), "/tmp/workspace-root", Config{})
	if err != nil {
		t.Fatalf("buildOperatorWiring() error = %v", err)
	}
	if wiring.cfg.RuntimeContext == nil {
		t.Error("wiring.cfg.RuntimeContext = nil, want runtime context enabled for the operator")
	}
}

// TestOperatorPrimaryToolSetIsLeafUnionPlusSubagent proves the PRIMARY operator's toolset
// is EXACTLY the operator leaf's toolset PLUS Subagent — the drift guard the
// operatorPrimaryToolSet doc promises. It builds both over the SAME root + skill and
// asserts primary-minus-Subagent == leaf tools, and that Subagent is present on the
// primary (so the primary can spawn) and absent from the leaf (so a spawned operator
// cannot).
func TestOperatorPrimaryToolSetIsLeafUnionPlusSubagent(t *testing.T) {
	t.Parallel()

	deps, spawner, catalog, _, skill := operatorPrimaryArgs(t, "/tmp/workspace-root")
	primary := operatorPrimaryToolSet(deps.Root, deps.HTTPCl, spawner, catalog, skill)
	if primary.Permission == nil {
		t.Fatal("operatorPrimaryToolSet() Permission = nil, want non-nil PermissionChecker")
	}
	leaf := operator.BuildTools(deps.Root, deps.HTTPCl, skill)

	primaryNames := sortedNames(t, primary)
	leafNames := sortedNames(t, leaf)

	if !containsName(primaryNames, "Subagent") {
		t.Errorf("primary toolset = %v, want it to contain Subagent (the primary must be able to spawn)", primaryNames)
	}
	if containsName(leafNames, "Subagent") {
		t.Errorf("operator leaf toolset = %v, must NOT contain Subagent (a spawned operator cannot spawn)", leafNames)
	}

	// primary MINUS Subagent must equal the leaf set exactly (no drift).
	got := make([]string, 0, len(primaryNames))
	for _, n := range primaryNames {
		if n != "Subagent" {
			got = append(got, n)
		}
	}
	if !equalStringSlice(got, leafNames) {
		t.Errorf("primary tools minus Subagent = %v, want the leaf union %v", got, leafNames)
	}
}

// sortedNames returns the toolset's tool names, sorted.
func sortedNames(t *testing.T, ts loop.ToolSet) []string {
	t.Helper()
	out := toolNames(t, ts)
	sort.Strings(out)
	return out
}

// TestOperatorPrimaryToolSetPermissions proves the primary operator's PermissionChecker
// auto-approves the read/search/plan/ask/spawn/skill tools and human-gates (never
// auto-approves) the mutating + network tools (WriteFile, EditFile, Bash, WebSearch,
// Fetch). Subagent has no path/command boundary and reaches AutoApprove only via the
// hard-approve list; the Skill tool (embedded code-style) auto-approves the same way.
func TestOperatorPrimaryToolSetPermissions(t *testing.T) {
	t.Parallel()

	// A real, existing root so the Stage-1 containment check (which EvalSymlinks the
	// root) clears for the read/search tools.
	root := t.TempDir()
	deps, spawner, catalog, _, skill := operatorPrimaryArgs(t, root)
	ts := operatorPrimaryToolSet(root, deps.HTTPCl, spawner, catalog, skill)

	// Per-tool valid args: the auto-approve tools carry args that clear Stage-1
	// containment; the gated tools carry empty args (a gated tool is never auto-approved
	// regardless of args — fail-secure — so the security-relevant assertion is "not
	// auto-approve").
	autoApprove := map[string]string{
		"ReadFile": `{"path":"file.txt"}`,
		"Glob":     `{"pattern":"*.go","root":"."}`,
		"Grep":     `{"pattern":"foo","path":"."}`,
		"Todo":     `{}`,
		"AskUser":  `{}`,
		"Subagent": `{"agent":"operator","message":"do it"}`,
		"Skill":    `{"name":"code-style"}`,
	}
	gated := map[string]string{
		"WriteFile": `{}`,
		"EditFile":  `{}`,
		"Bash":      `{}`,
		"WebSearch": `{}`,
		"Fetch":     `{}`,
	}

	for _, tl := range ts.Registry {
		info, err := tl.Info(t.Context())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		name := info.Name
		switch {
		case hasKey(autoApprove, name):
			eff := ts.Permission.Check(t.Context(), tl, name, autoApprove[name])
			if eff != loop.EffectAutoApprove {
				t.Errorf("Check(%q) = %v, want EffectAutoApprove", name, eff)
			}
		case hasKey(gated, name):
			eff := ts.Permission.Check(t.Context(), tl, name, gated[name])
			if eff == loop.EffectAutoApprove {
				t.Errorf("Check(%q) = EffectAutoApprove, want a human gate (Ask/Deny — never auto-approve)", name)
			}
		default:
			t.Errorf("unexpected tool %q in primary toolset (no permission expectation)", name)
		}
	}
}

// hasKey reports whether m has key k.
func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}

// TestOperatorPrimaryToolSetPermissionParity hardens the name-only drift guard: for EVERY
// tool the operator LEAF carries, the PRIMARY operator's PermissionChecker must resolve the
// SAME effect (auto-approve vs gated) as the leaf's checker — so the primary really is
// "leaf + Subagent" in GATING semantics, not merely in tool names. It builds both toolsets
// over the SAME root and the SAME code-style Skill (so Skill is compared too) and Checks
// each leaf tool's effect on BOTH checkers with the SAME args; the effect is a deterministic
// function of (policy, tool, args), so any per-side divergence — e.g. a tool silently moving
// Ask->auto-approve on only one side — is a real drift this catches.
func TestOperatorPrimaryToolSetPermissionParity(t *testing.T) {
	t.Parallel()

	// A real, existing root so the Stage-1 containment check clears for the read/search
	// tools on both checkers.
	root := t.TempDir()
	deps, spawner, catalog, _, skill := operatorPrimaryArgs(t, root)
	leaf := operatorBuiltin().build(deps, skill) // == operator.BuildTools(root, http, skill)
	primary := operatorPrimaryToolSet(root, deps.HTTPCl, spawner, catalog, skill)

	// Representative args per leaf tool. The SAME args go to BOTH checkers, so divergence is
	// a real per-side drift (never an args artifact). Auto-approve tools carry in-root/embedded
	// args so they clear containment; a gated tool is never auto-approved regardless of args.
	args := map[string]string{
		"ReadFile":  `{"path":"file.txt"}`,
		"Glob":      `{"pattern":"*.go","root":"."}`,
		"Grep":      `{"pattern":"foo","path":"."}`,
		"WriteFile": `{"path":"out.txt","content":"x"}`,
		"EditFile":  `{"path":"out.txt","old":"a","new":"b"}`,
		"Bash":      `{"command":"ls"}`,
		"WebSearch": `{"query":"go"}`,
		"Fetch":     `{"url":"https://example.com"}`,
		"Todo":      `{}`,
		"AskUser":   `{}`,
		"Skill":     `{"name":"code-style"}`,
	}

	for _, leafTool := range leaf.Registry {
		info, err := leafTool.Info(t.Context())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		name := info.Name
		callArgs, ok := args[name]
		if !ok {
			t.Fatalf("no parity args for leaf tool %q (update the args map)", name)
		}
		leafEff := leaf.Permission.Check(t.Context(), leafTool, name, callArgs)
		primaryTool := mustTool(t, primary, name)
		primaryEff := primary.Permission.Check(t.Context(), primaryTool, name, callArgs)
		if primaryEff != leafEff {
			t.Errorf("tool %q: primary effect = %v, leaf effect = %v — want parity (the primary must gate exactly like the leaf)", name, primaryEff, leafEff)
		}
	}
}

// TestOperatorDelegationIsWellFormedXML proves the primary-only operatorDelegation fragment
// is a single well-formed <delegation> element, mirroring each agent's TestRoleIsWellFormedXML.
// The fragment is baked into the primary's system prompt (after operator.Role), so malformed
// XML would corrupt that assembly; this regression-guards it like the role and identity blocks.
func TestOperatorDelegationIsWellFormedXML(t *testing.T) {
	t.Parallel()
	var probe struct {
		XMLName xml.Name `xml:"delegation"`
	}
	if err := xml.Unmarshal([]byte(operatorDelegation), &probe); err != nil {
		t.Fatalf("operatorDelegation is not well-formed XML: %v", err)
	}
	if probe.XMLName.Local != "delegation" {
		t.Errorf("operatorDelegation root element = %q, want %q", probe.XMLName.Local, "delegation")
	}
}

// TestOperatorSpawnCaps pins the session's spawn safety caps and documents WHY Depth is 2.
// Only the primary operator carries Subagent and every spawnable leaf has none, so the real
// tree is depth-1 (primary → non-spawning leaf). looprig refuses a spawn whose would-be child
// has an ancestor chain ≥ Depth, so Depth=2 permits exactly that one level and refuses anything
// deeper. Depth=1 would refuse even the primary→leaf spawn (TestAcceptanceEndToEndSpawn would
// break); Depth>2 is dead slack since no leaf can spawn. operatorLimits() must carry both consts.
func TestOperatorSpawnCaps(t *testing.T) {
	t.Parallel()
	if operatorSpawnDepth != 2 {
		t.Errorf("operatorSpawnDepth = %d, want 2 (the structural depth-1 tree: primary → non-spawning leaf)", operatorSpawnDepth)
	}
	if operatorSpawnQuota != 64 {
		t.Errorf("operatorSpawnQuota = %d, want 64", operatorSpawnQuota)
	}
	lim := operatorLimits()
	if lim.Depth != operatorSpawnDepth || lim.Quota != operatorSpawnQuota {
		t.Errorf("operatorLimits() = {Depth:%d Quota:%d}, want {Depth:%d Quota:%d}", lim.Depth, lim.Quota, operatorSpawnDepth, operatorSpawnQuota)
	}
}
