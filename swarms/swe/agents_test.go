package swe

import (
	"testing"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/swe/agents/operator"
	"github.com/looprig/swe/agents/reviewer"
)

// equalStringSlice reports element-wise equality, treating nil and empty as equal (a
// skill-less agent's Skills is nil; the expectation is also nil).
func equalStringSlice(a, b []string) bool {
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

// TestSwarmRegistryHasExactlyTheTwoLeaves proves swarmRegistry registers EXACTLY the two
// roster agents — operator, reviewer — in that order. operator is also the primer's identity
// (sourced from the shared operatorBuiltin), but the operator leaf declares no delegates, so a
// spawned operator cannot itself spawn. The Registry is now pure metadata: it no longer builds
// tools or runs children.
func TestSwarmRegistryHasExactlyTheTwoLeaves(t *testing.T) {
	t.Parallel()

	reg, err := swarmRegistry()
	if err != nil {
		t.Fatalf("swarmRegistry() error = %v", err)
	}

	catalog := reg.Catalog()
	got := make([]identity.AgentName, 0, len(catalog))
	for _, e := range catalog {
		got = append(got, e.Name)
	}
	want := []identity.AgentName{operator.Name, reviewer.Name}
	if len(got) != len(want) {
		t.Fatalf("Catalog() names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Catalog() names = %v, want %v (order matters)", got, want)
		}
	}
}

// TestSwarmRegistryNoOrphanPrimary proves the retired "orchestrator" agent is NOT in the
// roster: the primer is an operator (which IS a leaf), and no separate primary-only agent
// lingers. It also proves the primer key "operator-primary" is NOT itself a registered roster
// agent — the roster is the spawnable delegate set, the primer is a topology concern.
func TestSwarmRegistryNoOrphanPrimary(t *testing.T) {
	t.Parallel()

	reg, err := swarmRegistry()
	if err != nil {
		t.Fatalf("swarmRegistry() error = %v", err)
	}
	if _, ok := reg.Lookup(identity.AgentName("orchestrator")); ok {
		t.Error(`Lookup("orchestrator") = found, want absent`)
	}
	if _, ok := reg.Lookup(operatorPrimaryName); ok {
		t.Error(`Lookup("operator-primary") = found, want absent (primer key is not a roster agent)`)
	}
}

// TestSwarmRegistryCarriesLeafMetadata proves each roster agent carries its own package's
// Name/Description/Role verbatim, its allowed embedded-skill set, and its §7a runtime-skills
// eligibility (operator eligible, reviewer not).
func TestSwarmRegistryCarriesLeafMetadata(t *testing.T) {
	t.Parallel()

	reg, err := swarmRegistry()
	if err != nil {
		t.Fatalf("swarmRegistry() error = %v", err)
	}

	tests := []struct {
		name              string
		agent             identity.AgentName
		wantDesc          string
		wantRole          string
		wantSkills        []string
		wantRuntimeSkills bool
	}{
		{
			name:              "operator",
			agent:             operator.Name,
			wantDesc:          operator.Description,
			wantRole:          operator.Role,
			wantSkills:        []string{"code-style"},
			wantRuntimeSkills: true,
		},
		{
			name:     "reviewer",
			agent:    reviewer.Name,
			wantDesc: reviewer.Description,
			wantRole: reviewer.Role,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, ok := reg.Lookup(tt.agent)
			if !ok {
				t.Fatalf("Lookup(%q) not found", tt.agent)
			}
			if a.Description != tt.wantDesc {
				t.Errorf("Description = %q, want %q", a.Description, tt.wantDesc)
			}
			if a.Role != tt.wantRole {
				t.Errorf("Role = %q, want %q", a.Role, tt.wantRole)
			}
			if a.AllowsRuntimeSkills != tt.wantRuntimeSkills {
				t.Errorf("AllowsRuntimeSkills = %v, want %v (§7a)", a.AllowsRuntimeSkills, tt.wantRuntimeSkills)
			}
			if !equalStringSlice(a.Skills, tt.wantSkills) {
				t.Errorf("Skills = %v, want %v", a.Skills, tt.wantSkills)
			}
		})
	}
}
