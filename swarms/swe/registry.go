// Package swe holds the SWE-Swarm's typed agent catalog. It is pure data and
// lookup: the single source of truth for which agents exist, what each one
// exposes (role prompt + its own toolset builder), and the deterministic order
// the catalog is presented in. Tool validation, the prompt catalog, and the
// greeting all read the Registry; nothing here drives a loop or owns identity,
// the model, or a bound loop definition — the swarm assembles those.
package swe

import (
	"strconv"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
)

// Agent is what an agent PACKAGE exposes as METADATA: its name, one-line description, role
// prompt, and allowed embedded-skill set. It does NOT own identity, the model, the toolset,
// or delegation — the composition root (swarm.go) builds each loop.Definition, and the rig
// owns delegation — so this type is pure data + lookup for the catalog and greeting.
type Agent struct {
	Name        identity.AgentName
	Description string // shown in the greeting/catalog
	Role        string // role prompt; the swarm prepends identity

	// Skills is the agent's closed set of allowed embedded-skill names — part of
	// its boundary. An agent with ≥1 skill is wired with the Skill tool and an
	// <available_skills> catalog in its system prompt; an empty set gets neither.
	Skills []string

	AllowsRuntimeSkills bool // §7a workspace-skill eligibility
}

// Config is the swarm's human-set construction config — the knobs a launch flag /
// operator decision sets, never the model (§2, §15). Today it carries only the
// runtime-skills enablement mode; it is a struct (not a bare bool) so future opt-in
// modes extend it without churning every construction signature. The zero value is
// the fail-secure default (every mode off).
type Config struct {
	// RuntimeSkills enables the untrusted, human-gated workspace skill source
	// (<workspaceRoot>/.skills/<name>/SKILL.md) for the agents whose definition sets
	// AllowsRuntimeSkills (the operator per §7a; eligibility was extended to it once it
	// merged write/exec capability). Off by default: embedded-only. The model can never
	// set it — only a launch flag does (cmd/swe's --runtime-skills). When off, no leaf
	// gains a workspace skill source.
	RuntimeSkills bool

	// Greeting enables the OPTIONAL, UI-only startup greeting (§5a): a deterministic,
	// LLM-free opening transcript entry listing the swarm's agents (+ embedded skills),
	// rendered by the TUI before any turn. Off by default (fail-secure): off → no
	// greeting, behavior identical to today. It is purely a rendered opening entry — NOT
	// a turn, NOT a command, never in the model's context. The model can never set it;
	// only a launch flag does (cmd/swe's --greeting). See Greeting() and greeting.go.
	Greeting bool

	// SecurityCeiling is the session security-mode CEILING ordinal (0 ZeroTrust … 4
	// Unconfined, matching sandbox.Mode). It caps how permissive per-role auto-approval
	// may be: each tool-building leaf's EFFECTIVE mode is min(its static role mode, this
	// ceiling), read live so lowering it clamps every leaf at once (SPEC §8). It is BOTH
	// the session's INITIAL ceiling and the runtime cap — a journaled SetSecurityCeiling
	// can lower it, or raise it only up to this value, never past it (fail-secure). The
	// launch flag sets it (cmd/swe's --security-mode); the model can never raise it. The
	// ZERO value is ZeroTrust (fail-secure: nothing auto-approves — identical to the
	// pre-sandbox gate); the CLI defaults it to Write (DefaultSecurityMode).
	SecurityCeiling uint8

	// ModelCatalog is the OPTIONAL model-tier catalog (§ optional model tiers). Its zero
	// value (all tiers empty) preserves the swarm's existing default model and disables
	// title-model generation, so a caller that sets nothing is unaffected. A non-empty
	// Standard selects its first model for normal loops; a non-empty Economy enables
	// best-effort title generation; Premium is stored but never implicitly selected. Every
	// supplied spec is validated at construction. See model_catalog.go.
	ModelCatalog ModelCatalog
}

// ModelFactory yields the swarm's shared, secret-free inference.Model identity (provider/
// endpoint/model/sampling). Post-split it carries NO secret and NO system prompt: the
// connection secret is bound to the Client once at auto.New, and each agent's finished
// system prompt is set on the loop definition (and inference.Request.System), never on the model.
type ModelFactory func() inference.Model

// AgentCatalogEntry is the public, lookup-free view of an agent: just the name
// and one-line description used to render the Subagent catalog and greeting.
type AgentCatalogEntry struct {
	Name        identity.AgentName
	Description string
}

// DuplicateAgentError is returned by NewRegistry when two agents share a Name.
// A duplicate is a programming error at the composition root, so registration
// fails secure (no registry is built) rather than silently picking a winner.
// It is errors.As-recoverable so the caller can report which name collided.
type DuplicateAgentError struct {
	Name identity.AgentName
}

func (e *DuplicateAgentError) Error() string {
	return "swe: duplicate agent name " + strconv.Quote(string(e.Name))
}

// Registry is the single source of truth for agent lookup + the catalog. It is
// immutable after construction: built once at the composition root and only
// read thereafter, so the zero-copy maps/slices are safe to share.
type Registry struct {
	byName map[identity.AgentName]Agent
	order  []identity.AgentName // deterministic catalog order (insertion order)
}

// NewRegistry builds a Registry from agents in the given order, preserving
// insertion order for the catalog. A duplicate Name is rejected with a
// *DuplicateAgentError (fail secure: no partial registry is returned).
func NewRegistry(agents ...Agent) (*Registry, error) {
	r := &Registry{
		byName: make(map[identity.AgentName]Agent, len(agents)),
		order:  make([]identity.AgentName, 0, len(agents)),
	}
	for _, a := range agents {
		if _, exists := r.byName[a.Name]; exists {
			return nil, &DuplicateAgentError{Name: a.Name}
		}
		r.byName[a.Name] = a
		r.order = append(r.order, a.Name)
	}
	return r, nil
}

// Lookup returns the agent registered under n and true, or the zero Agent and
// false if no agent is registered under that name.
func (r *Registry) Lookup(n identity.AgentName) (Agent, bool) {
	a, ok := r.byName[n]
	return a, ok
}

// Catalog returns the name+description of every agent in deterministic
// insertion order. The returned slice is a fresh copy: callers may mutate it
// without affecting the registry.
func (r *Registry) Catalog() []AgentCatalogEntry {
	out := make([]AgentCatalogEntry, 0, len(r.order))
	for _, name := range r.order {
		a := r.byName[name]
		out = append(out, AgentCatalogEntry{Name: a.Name, Description: a.Description})
	}
	return out
}
