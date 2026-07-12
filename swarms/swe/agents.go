package swe

import (
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/swe/agents/operator"
	"github.com/looprig/swe/agents/reviewer"
)

// operatorSkills is the operator's closed set of allowed embedded skills (the primer and the
// operator leaf share it). The implementer gets the coding-style checklist; the reviewer
// starts with none in this cut. This is the single source of truth the loader's allow-map AND
// the agent's <available_skills> catalog are both derived from.
var operatorSkills = []string{"code-style"}

// leafBuiltin is each agent's package-exported boundary as pure metadata: Name/Description/
// Role plus allowed embedded Skills, runtime-skills eligibility, and the static per-role
// security mode. It no longer carries a tool builder — the composition root (swarm.go) builds
// each loop.Definition directly from the leaf package's BuildTools — so this struct is the ONE
// place the per-agent skill set, runtime-skills eligibility, and role prompt are declared for
// the greeting, the skill loader allow-map, and the <available_skills> catalog.
type leafBuiltin struct {
	name        identity.AgentName
	description string
	role        string
	skills      []string
	// allowsRuntimeSkills marks a leaf eligible for the untrusted, human-gated workspace
	// skill source (§7a). True ONLY for the operator (the approved decision extended
	// eligibility to it once the operator merged write/exec capability); the reviewer stays
	// false. Both this per-agent gate AND the swarm-wide cfg.RuntimeSkills mode must be true
	// to wire the workspace source.
	allowsRuntimeSkills bool
	// securityMode is the leaf's STATIC per-role security mode (SPEC §8): operator → Write
	// (workspace write/edit + gated bash), reviewer → ReadOnly. The per-role confine.Factory
	// clamps the effective mode to min(this, session ceiling).
	securityMode uint8
}

// operatorBuiltin is the single operator definition, shared by the operator-primary primer
// and the operator leaf (via their shared operator.BuildTools call in swarm.go) so their
// skills/eligibility/role cannot drift. A spawned operator leaf has no delegates, so it cannot
// itself spawn.
func operatorBuiltin() leafBuiltin {
	return leafBuiltin{
		name:                operator.Name,
		description:         operator.Description,
		role:                operator.Role,
		skills:              operatorSkills,
		allowsRuntimeSkills: true, // §7a: extended to operator (approved) — bounded, human-gated workspace load.
		securityMode:        operatorRoleMode,
	}
}

// reviewerBuiltin is the reviewer leaf definition: read-only critique, no embedded skills, no
// runtime-skills eligibility.
func reviewerBuiltin() leafBuiltin {
	return leafBuiltin{
		name:         reviewer.Name,
		description:  reviewer.Description,
		role:         reviewer.Role,
		securityMode: reviewerRoleMode,
	}
}

// leafBuiltins is the fixed roster in deterministic catalog order: operator then reviewer.
// operator appears once (it is the primer's identity AND the spawnable operator leaf).
func leafBuiltins() []leafBuiltin {
	return []leafBuiltin{operatorBuiltin(), reviewerBuiltin()}
}

// swarmRegistry builds the swarm's metadata Registry (agent name/description/role/skills) from
// the roster. It is pure data + lookup — the source the catalog/greeting read — and no longer
// creates loop configs or runs children (the rig owns delegation). A duplicate name fails
// secure with a *DuplicateAgentError.
func swarmRegistry() (*Registry, error) {
	builtins := leafBuiltins()
	agents := make([]Agent, 0, len(builtins))
	for _, b := range builtins {
		agents = append(agents, Agent{
			Name:                b.name,
			Description:         b.description,
			Role:                b.role,
			Skills:              b.skills,
			AllowsRuntimeSkills: b.allowsRuntimeSkills,
		})
	}
	return NewRegistry(agents...)
}
