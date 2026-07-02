package swe

import (
	"net/http"
	"testing"

	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/swe/agents/operator"
	"github.com/ciram-co/swe/agents/reviewer"
)

// runtime_skills_test.go pins P2b Phase 3c: the --runtime-skills enablement gate
// and the per-agent AllowsRuntimeSkills eligibility. After the operator consolidation
// the runtime-skills-eligible set is EXACTLY {operator}; reviewer is ineligible.
// When the mode is OFF (the default) no leaf gains a workspace-capable Skill tool —
// operator keeps its embedded-only (auto-approve) code-style Skill and reviewer gets
// none. When the mode is ON, ONLY the eligible operator gains a WORKSPACE-enabled Skill
// tool: its embedded code-style name still auto-approves (embedded-wins) while a
// NON-embedded name is human-gated (EffectAsk); the ineligible reviewer still gets no
// Skill tool.

// runtimeDeps is a minimal LeafToolDeps whose Root is a real, distinct path so the
// workspace-enabled Skill tool has a non-empty workspace root.
func runtimeDeps(root string) LeafToolDeps {
	return LeafToolDeps{Root: root, HTTPCl: &http.Client{}}
}

// skillToolFromRegistry resolves agent in reg, builds its toolset, and returns the
// Skill tool (or nil if the agent has none). The deps Root must be the same root the
// registry was built with so the workspace-enabled tool's root matches.
func skillToolFromRegistry(t *testing.T, reg *Registry, deps LeafToolDeps, agent identity.AgentName) loop.ToolSet {
	t.Helper()
	a, ok := reg.Lookup(agent)
	if !ok {
		t.Fatalf("Lookup(%q) not found", agent)
	}
	ts, err := a.BuildTools(deps)
	if err != nil {
		t.Fatalf("BuildTools(%q) error = %v", agent, err)
	}
	return ts
}

// TestAllowsRuntimeSkillsEligibility proves the per-agent eligibility flag is set on
// EXACTLY the operator (eligibility was extended to it when it merged write/exec
// capability) and is false for the reviewer — the §7a boundary, regardless of mode.
func TestAllowsRuntimeSkillsEligibility(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name identity.AgentName
		want bool
	}{
		{name: operator.Name, want: true},
		{name: reviewer.Name, want: false},
	}
	// Eligibility is independent of the RuntimeSkills mode: assert it under BOTH.
	for _, mode := range []bool{false, true} {
		mode := mode
		reg, _, err := leafRegistry(runtimeDeps(t.TempDir()), Config{RuntimeSkills: mode})
		if err != nil {
			t.Fatalf("leafRegistry(RuntimeSkills=%v) error = %v", mode, err)
		}
		for _, tt := range tests {
			tt := tt
			t.Run(string(tt.name), func(t *testing.T) {
				t.Parallel()
				a, ok := reg.Lookup(tt.name)
				if !ok {
					t.Fatalf("Lookup(%q) not found", tt.name)
				}
				if a.AllowsRuntimeSkills != tt.want {
					t.Errorf("AllowsRuntimeSkills = %v, want %v (mode=%v)", a.AllowsRuntimeSkills, tt.want, mode)
				}
			})
		}
	}
}

// TestRuntimeSkillsOffNoWorkspaceTool proves the default (mode OFF): reviewer (skill-less
// AND ineligible) gets NO Skill tool, and operator keeps its embedded Skill tool which
// AUTO-APPROVES (embedded-only — the mode flag is off, so eligibility never workspace-
// enables it). This is the HEAD behaviour, unchanged.
func TestRuntimeSkillsOffNoWorkspaceTool(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	deps := runtimeDeps(root)
	reg, _, err := leafRegistry(deps, Config{RuntimeSkills: false})
	if err != nil {
		t.Fatalf("leafRegistry() error = %v", err)
	}

	// reviewer is skill-less AND runtime-skills ineligible: it gets NO Skill tool.
	revTS := skillToolFromRegistry(t, reg, deps, reviewer.Name)
	if containsName(toolNames(t, revTS), "Skill") {
		t.Errorf("reviewer toolset has a Skill tool with RuntimeSkills OFF, want none")
	}

	// operator keeps its embedded Skill tool, and it AUTO-APPROVES (embedded-only): with
	// the mode off, an eligible agent's Skill is still NOT workspace-enabled.
	opTS := skillToolFromRegistry(t, reg, deps, operator.Name)
	skillTool := mustTool(t, opTS, "Skill")
	eff := opTS.Permission.Check(t.Context(), skillTool, "Skill", `{"name":"code-style"}`)
	if eff != loop.EffectAutoApprove {
		t.Errorf("operator Skill effect = %v, want EffectAutoApprove (embedded-only auto-approves)", eff)
	}
}

// TestRuntimeSkillsOnWorkspaceTool proves the mode ON: the eligible operator gains a
// WORKSPACE-enabled Skill tool whose combined behaviour is (a) its embedded code-style
// name still AUTO-APPROVES (embedded-wins — CheckEffect falls through to HardApprove),
// and (b) a NON-embedded name is human-gated (CheckEffect returns (EffectAsk, true)).
// The ineligible reviewer still gets NO Skill tool even with the mode on — proving the
// mode never workspace-enables an ineligible agent.
func TestRuntimeSkillsOnWorkspaceTool(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	deps := runtimeDeps(root)
	reg, _, err := leafRegistry(deps, Config{RuntimeSkills: true})
	if err != nil {
		t.Fatalf("leafRegistry() error = %v", err)
	}

	// operator gains a workspace-enabled Skill tool. embedded-wins: code-style still
	// auto-approves even though the tool is workspace-enabled.
	opTS := skillToolFromRegistry(t, reg, deps, operator.Name)
	skillTool := mustTool(t, opTS, "Skill")
	if eff := opTS.Permission.Check(t.Context(), skillTool, "Skill", `{"name":"code-style"}`); eff != loop.EffectAutoApprove {
		t.Errorf("operator Skill(code-style) effect = %v, want EffectAutoApprove (embedded-wins)", eff)
	}

	// A NON-embedded (workspace) name returns (EffectAsk, true) from CheckEffect — the
	// human-gated workspace load. This is the §7a gate now extended to the operator.
	ec, ok := skillTool.(interface {
		CheckEffect(string) (loop.Effect, bool)
	})
	if !ok {
		t.Fatal("operator Skill tool does not implement EffectChecker")
	}
	eff, handled := ec.CheckEffect(`{"name":"project-local"}`)
	if !handled || eff != loop.EffectAsk {
		t.Errorf("operator Skill CheckEffect(non-embedded) = (%v, %v), want (EffectAsk, true) — workspace gate", eff, handled)
	}

	// reviewer stays INELIGIBLE even with the mode on: it has no embedded skills and is
	// not workspace-eligible, so it gets no Skill tool at all.
	revTS := skillToolFromRegistry(t, reg, deps, reviewer.Name)
	if containsName(toolNames(t, revTS), "Skill") {
		t.Errorf("reviewer toolset has a Skill tool with RuntimeSkills ON, want none (ineligible)")
	}
}

// TestRuntimeSkillsPrimaryHasEmbeddedSkillTool proves the PRIMARY operator's toolset
// carries its embedded code-style Skill tool under EITHER mode, and that the embedded
// code-style name AUTO-APPROVES under both (embedded-wins). With the mode ON the
// operator is now AllowsRuntimeSkills, so the primary's Skill is workspace-enabled — but
// embedded-wins keeps code-style auto-approving while a non-embedded name is gated
// (asserted on the leaf in TestRuntimeSkillsOnWorkspaceTool).
func TestRuntimeSkillsPrimaryHasEmbeddedSkillTool(t *testing.T) {
	t.Parallel()

	for _, mode := range []bool{false, true} {
		mode := mode
		t.Run(map[bool]string{false: "off", true: "on"}[mode], func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			deps := runtimeDeps(root)
			reg, loader, err := leafRegistry(deps, Config{RuntimeSkills: mode})
			if err != nil {
				t.Fatalf("leafRegistry() error = %v", err)
			}
			sp := newSwarmSpawner(reg, deps, &fakeLLM{}, newModelFactory(), loader, NewRuntimeContextProvider())
			skill := buildLeafSkill(loader, operatorBuiltin(), deps, Config{RuntimeSkills: mode})
			ts, err := operatorPrimaryToolSet(deps.Root, deps.HTTPCl, sp, toolCatalog(reg), skill)
			if err != nil {
				t.Fatalf("operatorPrimaryToolSet() error = %v", err)
			}
			skillTool := mustTool(t, ts, "Skill")
			eff := ts.Permission.Check(t.Context(), skillTool, "Skill", `{"name":"code-style"}`)
			if eff != loop.EffectAutoApprove {
				t.Errorf("primary operator Skill effect = %v (mode=%v), want EffectAutoApprove (embedded-wins)", eff, mode)
			}
		})
	}
}
