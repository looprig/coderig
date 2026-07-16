package app

import (
	"github.com/looprig/coderig/internal/catalog/operator"
	"github.com/looprig/coderig/internal/catalog/reviewer"
	"github.com/looprig/harness/pkg/identity"
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
	}
}

// reviewerBuiltin is the reviewer leaf definition: read-only critique, no embedded skills, no
// runtime-skills eligibility.
func reviewerBuiltin() leafBuiltin {
	return leafBuiltin{
		name:        reviewer.Name,
		description: reviewer.Description,
		role:        reviewer.Role,
	}
}

// leafBuiltins is the fixed roster in deterministic catalog order: operator then reviewer.
// operator appears once (it is the primer's identity AND the spawnable operator leaf).
func leafBuiltins() []leafBuiltin {
	return []leafBuiltin{operatorBuiltin(), reviewerBuiltin()}
}
