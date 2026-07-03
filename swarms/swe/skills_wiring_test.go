package swe

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/swe/agents/operator"
	"github.com/looprig/swe/agents/reviewer"
)

// skills_wiring_test.go proves the Task-3 composition: a SKILLED leaf (operator)
// gets the Skill tool (auto-approved) AND an <available_skills> catalog in its
// system prompt, while a SKILL-LESS leaf (reviewer) gets neither — assembled
// through the SAME spawner seam production uses (Spawn builds the leaf's
// loop.Config), so the assertions exercise the wired behaviour end-to-end.

// spawnCfg drives the spawner to resolve an agent and captures the loop.Config it
// assembled (system prompt + toolset), via the fake runner. It reuses the spawner
// + fakeRunner idiom from spawner_test.go.
func spawnCfg(t *testing.T, agent identity.AgentName) loop.Config {
	t.Helper()
	sp, runner := newTestSwarmSpawner(t)
	if _, err := sp.Spawn(context.Background(), loop.Provenance{}, agent, "do the thing", "toolu_skills"); err != nil {
		t.Fatalf("Spawn(%q) error = %v", agent, err)
	}
	if !runner.called {
		t.Fatalf("Spawn(%q) never ran the runner", agent)
	}
	return runner.gotCfg
}

// spawnCfgRuntimeSkillsOn is spawnCfg with the RuntimeSkills mode ON, so an eligible
// leaf's Skill tool is WORKSPACE-enabled. It mirrors newTestSwarmSpawner's wiring but
// passes Config{RuntimeSkills: true} to leafRegistry, then captures the spawned leaf's
// assembled loop.Config — letting a test assert the mode-ON system prompt + toolset.
func spawnCfgRuntimeSkillsOn(t *testing.T, agent identity.AgentName) loop.Config {
	t.Helper()
	deps := LeafToolDeps{Root: "/tmp/workspace-root", HTTPCl: &http.Client{}}
	reg, loader, err := leafRegistry(deps, Config{RuntimeSkills: true})
	if err != nil {
		t.Fatalf("leafRegistry(RuntimeSkills=true) error = %v", err)
	}
	sp := newSwarmSpawner(reg, deps, &fakeLLM{}, newModelFactory(), loader, NewRuntimeContextProvider())
	runner := &fakeRunner{reply: "subagent done"}
	sp.session = runner // late-bind a fake, exactly where bind sets the live session
	if _, err := sp.Spawn(context.Background(), loop.Provenance{}, agent, "do the thing", "toolu_skills_on"); err != nil {
		t.Fatalf("Spawn(%q) error = %v", agent, err)
	}
	if !runner.called {
		t.Fatalf("Spawn(%q) never ran the runner", agent)
	}
	return runner.gotCfg
}

// TestSkilledAgentGetsToolAndCatalog proves operator's assembled config carries the
// Skill tool (auto-approved through its wired PermissionChecker) and an
// <available_skills> block listing the code-style skill (name + description).
func TestSkilledAgentGetsToolAndCatalog(t *testing.T) {
	t.Parallel()

	cfg := spawnCfg(t, operator.Name)

	// The Skill tool is wired into operator's toolset.
	names := toolNames(t, cfg.Tools)
	if !containsName(names, "Skill") {
		t.Errorf("operator toolset = %v, want it to contain the Skill tool", names)
	}

	// The Skill tool auto-approves (classUnknown + named in HardApprove).
	skillTool := mustTool(t, cfg.Tools, "Skill")
	if eff := cfg.Tools.Permission.Check(context.Background(), skillTool, "Skill", `{"name":"code-style"}`); eff != loop.EffectAutoApprove {
		t.Errorf("Check(Skill) effect = %v, want %v (Skill must auto-approve)", eff, loop.EffectAutoApprove)
	}

	// The system prompt carries the <available_skills> catalog naming code-style and
	// its description (read from the trusted embedded SKILL.md), AFTER Identity+Role.
	sys := cfg.System
	if !strings.HasPrefix(sys, Identity+operator.Role) {
		t.Error("operator system prompt does not begin with Identity+Role")
	}
	if !strings.Contains(sys, "<available_skills>") || !strings.Contains(sys, "</available_skills>") {
		t.Errorf("operator system prompt missing <available_skills> block:\n%s", sys)
	}
	if !strings.Contains(sys, "code-style") {
		t.Error("operator <available_skills> does not list the code-style skill")
	}
	// The description is read from the embedded SKILL.md frontmatter.
	if !strings.Contains(sys, "coding-style checklist") {
		t.Errorf("operator <available_skills> missing the skill description:\n%s", sys)
	}
}

// TestWorkspaceEnabledOperatorKeepsEmbeddedCatalog pins the §7a mode-ON catalog
// behaviour: with RuntimeSkills ON, the operator's Skill tool is WORKSPACE-enabled
// (a NON-embedded name is Ask-gated), yet the operator leaf's system prompt STILL
// carries the trusted <available_skills> catalog for its embedded code-style skill —
// the embedded-wins property that would silently regress if a workspace-enabled agent
// were treated like the old catalog-less read-only leaves. It also proves NO non-embedded
// workspace skill name is injected into the prompt (workspace descriptions are untrusted
// per §7a; the catalog is built only from the embedded operatorSkills set). The only
// existing prompt/catalog assertions run mode OFF, so this closes that gap.
func TestWorkspaceEnabledOperatorKeepsEmbeddedCatalog(t *testing.T) {
	t.Parallel()

	cfg := spawnCfgRuntimeSkillsOn(t, operator.Name)

	// The Skill tool is present AND workspace-enabled: a NON-embedded name returns
	// (EffectAsk, true) from CheckEffect (the §7a workspace gate), proving the mode is on.
	skillTool := mustTool(t, cfg.Tools, "Skill")
	ec, ok := skillTool.(interface {
		CheckEffect(string) (loop.Effect, bool)
	})
	if !ok {
		t.Fatal("operator Skill tool does not implement EffectChecker (expected workspace-enabled)")
	}
	if eff, handled := ec.CheckEffect(`{"name":"project-local"}`); !handled || eff != loop.EffectAsk {
		t.Fatalf("operator Skill CheckEffect(non-embedded) = (%v, %v), want (EffectAsk, true) — not workspace-enabled", eff, handled)
	}

	// Despite being workspace-enabled, the prompt STILL carries the embedded code-style
	// catalog (embedded-wins): this is the property the §7a comment now asserts.
	sys := cfg.System
	if !strings.HasPrefix(sys, Identity+operator.Role) {
		t.Error("operator system prompt does not begin with Identity+Role")
	}
	if !strings.Contains(sys, "<available_skills>") || !strings.Contains(sys, "code-style") {
		t.Errorf("operator system prompt (mode ON) missing the embedded <available_skills> code-style catalog:\n%s", sys)
	}
	// A non-embedded workspace skill name is NEVER injected into the prompt.
	if strings.Contains(sys, "project-local") {
		t.Errorf("operator system prompt (mode ON) leaked a non-embedded workspace skill name:\n%s", sys)
	}
}

// TestSkillLessAgentGetsNeither proves a leaf with no skills (reviewer) gets neither
// the Skill tool nor an <available_skills> catalog — its config is unchanged. reviewer
// is also runtime-skills INELIGIBLE, and spawnCfg builds it with RuntimeSkills off, so
// no workspace Skill tool is wired either.
func TestSkillLessAgentGetsNeither(t *testing.T) {
	t.Parallel()

	cfg := spawnCfg(t, reviewer.Name)

	names := toolNames(t, cfg.Tools)
	if containsName(names, "Skill") {
		t.Errorf("reviewer toolset = %v, must NOT contain a Skill tool (no skills)", names)
	}
	sys := cfg.System
	if strings.Contains(sys, "available_skills") {
		t.Errorf("reviewer system prompt must NOT carry an <available_skills> block:\n%s", sys)
	}
	// A skill-less leaf's prompt is exactly Identity + Role.
	if sys != Identity+reviewer.Role {
		t.Errorf("reviewer system prompt = %q, want exactly Identity+Role", sys)
	}
}

// mustTool returns the InvokableTool whose Info().Name == name from a toolset, or
// fails the test if it is absent.
func mustTool(t *testing.T, ts loop.ToolSet, name string) tool.InvokableTool {
	t.Helper()
	for _, tl := range ts.Registry {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		if info.Name == name {
			return tl
		}
	}
	t.Fatalf("tool %q not in toolset", name)
	return nil
}
