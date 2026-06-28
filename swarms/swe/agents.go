package swe

import (
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/tools"
	"github.com/ciram-co/swe/agents/reviewer"
)

// operatorSkills is the operator leaf's closed set of allowed embedded skills. The
// implementer gets the coding-style checklist; the other leaves start with none in
// this cut. This is the single source of truth the loader's allow-map AND the
// agent's <available_skills> catalog are both derived from.
var operatorSkills = []string{"code-style"}

// leafBuiltin describes each spawnable leaf the swarm wires: its package-exported
// boundary (Name/Description/Role + allowed Skills + runtime-skills eligibility) and
// a raw binder that adapts the leaf package's BuildTools(root[, http], skill) into
// the swe.Agent shape. It is the ONE place the per-agent skill set, the runtime-skills
// eligibility, and the leaf wiring are declared, so the loader's allow-map, the
// per-agent Skill tool, and the catalog all stay in sync.
type leafBuiltin struct {
	name        identity.AgentName
	description string
	role        string
	skills      []string
	// allowsRuntimeSkills marks a leaf eligible for the untrusted, human-gated
	// workspace skill source (§7a). True ONLY for the operator (the approved decision
	// extended eligibility to it once the operator merged write/exec capability); the
	// reviewer stays false. operator is write/exec/network-capable, but the added attack
	// surface is bounded, not just asserted-by-authority: a workspace load is
	// load-a-skill-by-a-name-you-already-know, after a human Ask, with NO description
	// injected into the prompt and NO new auto-execution — and only a NON-embedded
	// workspace load is Ask-gated (an embedded name like code-style auto-approves via
	// embedded-wins). So extending eligibility to a write+shell+network agent stays
	// contained — the untrusted source is never auto-trusted. It is the per-agent half of
	// the gate; the swarm-wide RuntimeSkills mode is the other half — BOTH must be true to
	// wire the source.
	allowsRuntimeSkills bool
	// build adapts the leaf's raw BuildTools, threading the OPTIONAL per-agent Skill
	// tool (nil when the agent has neither embedded skills nor a wired workspace
	// source) into the leaf's allowlist.
	build func(d LeafToolDeps, skill tool.InvokableTool) loop.ToolSet
}

// leafBuiltins is the fixed roster of spawnable leaves, in deterministic catalog
// order: operator then reviewer. The operator entry is sourced from the shared
// operatorBuiltin() so the spawnable operator leaf and the swarm's PRIMARY loop (an
// operator carrying the Subagent tool, assembled in swarm.go) cannot drift — they are
// the ONE operator definition. A spawned operator leaf has no Subagent tool, so it
// cannot itself spawn.
func leafBuiltins() []leafBuiltin {
	return []leafBuiltin{
		operatorBuiltin(),
		{
			name:        reviewer.Name,
			description: reviewer.Description,
			role:        reviewer.Role,
			build:       func(d LeafToolDeps, s tool.InvokableTool) loop.ToolSet { return reviewer.BuildTools(d.Root, s) },
		},
	}
}

// leafRegistry builds the SWE-Swarm's registry of spawnable LEAF agents from the
// leaf builtins (operator + reviewer), adapting each leaf's raw-signature BuildTools
// into the swe.Agent shape (func(LeafToolDeps) loop.ToolSet) at the composition root —
// so the leaf packages never import swarms/swe (no import cycle). The primary loop (an
// operator carrying the Subagent tool) is assembled in swarm.go, not here, and shares
// the operator leaf's definition (operatorBuiltin) so the two cannot drift. Each leaf's
// AllowsRuntimeSkills is carried through from its builtin definition (§7a: true for the
// operator, false for the reviewer). A duplicate name fails secure with a
// *DuplicateAgentError.
//
// It also returns the ONE per-agent-scoped skill loader (built over the embedded
// SkillsFS + the allow-map derived from every leaf's Skills), TYPED as the combined
// skillLoaderDescriber: the spawner + catalog read it as a tools.SkillDescriber, while
// the composition root also builds the primary operator's own code-style Skill tool from
// its tools.SkillLoader half (so the primary's Skill matches the operator leaf's). The
// loader is wired multiple ways: a per-agent tools.Skill tool — built here from the
// loader's Load capability — is captured in each skilled leaf's BuildTools closure (so
// the leaf gets the Skill tool, which auto-approves by being named in HardApprove for an
// embedded name), and the spawner uses the SkillDescriber to append the
// <available_skills> catalog to a skilled leaf's system prompt.
//
// RUNTIME (WORKSPACE) SKILLS — §7a. A leaf gets a Skill tool when it has ≥1 embedded
// skill OR it is workspace-eligible AND the cfg.RuntimeSkills mode is on (BOTH gates).
// When the leaf is workspace-eligible and the mode is on, its Skill tool is built
// WORKSPACE-ENABLED (tools.WithWorkspaceRoot(deps.Root), the same root the file tools
// use): an embedded name still auto-approves (embedded-wins), a non-embedded name is a
// human-gated workspace load. The eligible agent (operator) ALSO has the embedded
// code-style skill, so its workspace Skill tool still carries the trusted
// <available_skills> catalog for that embedded name (embedded-wins on load); a
// non-embedded workspace name is never injected into the system prompt — workspace skill
// descriptions are untrusted (§7a) and the model loads such a skill by a name it already
// knows. A leaf that is neither skilled nor (eligible+mode-on) gets a nil Skill tool —
// neither the tool nor a HardApprove entry.
//
// The deps parameter IS now read to source the workspace root for an enabled leaf's
// Skill tool, but it is still NOT captured by the build adapters: each adapter
// re-invokes the leaf's BuildTools with the deps the swarm passes PER SPAWN (the
// registry stores the closure, not its result), so every spawn gets a FRESH
// PermissionChecker — the per-loop approval-isolation guarantee. The Skill tool is
// the one captured value: it is immutable + side-effect-free (loader + agent name +
// the fixed workspace root), so sharing one instance across a leaf's spawns is safe.
func leafRegistry(deps LeafToolDeps, cfg Config) (*Registry, skillLoaderDescriber, error) {
	builtins := leafBuiltins()

	scopes := make([]skillScope, 0, len(builtins))
	for _, b := range builtins {
		scopes = append(scopes, skillScope{name: b.name, skills: b.skills})
	}
	loader := tools.NewEmbeddedSkillLoader(SkillsFS, buildSkillAllow(scopes))

	agents := make([]Agent, 0, len(builtins))
	for _, b := range builtins {
		b := b
		skill := buildLeafSkill(loader, b, deps, cfg)
		agents = append(agents, Agent{
			Name:                b.name,
			Description:         b.description,
			Role:                b.role,
			Skills:              b.skills,
			AllowsRuntimeSkills: b.allowsRuntimeSkills,
			BuildTools:          func(d LeafToolDeps) loop.ToolSet { return b.build(d, skill) },
		})
	}

	reg, err := NewRegistry(agents...)
	if err != nil {
		return nil, nil, err
	}
	return reg, loader, nil
}

// skillLoaderDescriber is the embedded skill loader's full surface: both the Skill
// tool's tools.SkillLoader (Load/Allowed) and the catalog builder's tools.SkillDescriber
// (Describe). leafRegistry returns it (the concrete embeddedSkillLoader satisfies both)
// so the composition root can build BOTH a Skill tool (needs SkillLoader — the leaves AND
// the primary operator) and the <available_skills> catalog (needs SkillDescriber) from
// the one loader, while every consumer narrows to the half it needs.
type skillLoaderDescriber interface {
	tools.SkillLoader
	tools.SkillDescriber
}

// buildLeafSkill constructs the per-agent Skill tool for one leaf, honoring BOTH
// halves of the §7a gate. It returns nil — the leaf gets no Skill tool — unless the
// leaf either has ≥1 embedded skill or is workspace-eligible with the RuntimeSkills
// mode on. When the leaf is workspace-eligible and the mode is on, the tool is
// WORKSPACE-ENABLED at deps.Root (embedded-wins; a non-embedded name is Ask-gated);
// otherwise it is the embedded-only tool (auto-approve). Returning a typed nil
// tool.InvokableTool (not a typed-nil *tools.Skill) keeps the caller's nil check
// correct — the build closure passes nil straight through to the leaf.
func buildLeafSkill(loader tools.SkillLoader, b leafBuiltin, deps LeafToolDeps, cfg Config) tool.InvokableTool {
	workspaceEnabled := cfg.RuntimeSkills && b.allowsRuntimeSkills
	if len(b.skills) == 0 && !workspaceEnabled {
		return nil // neither embedded skills nor a wired workspace source: no Skill tool.
	}
	if workspaceEnabled {
		return tools.NewSkill(loader, b.name, tools.WithWorkspaceRoot(deps.Root))
	}
	return tools.NewSkill(loader, b.name)
}
