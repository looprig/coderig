package swe

import (
	"net/http"
	"sort"
	"testing"

	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/swe/agents/operator"
	"github.com/ciram-co/swe/agents/reviewer"
)

// testLeafDeps is a minimal LeafToolDeps for registry-shape tests: a throwaway
// root and a fresh http.Client. The tools are never invoked, only built.
func testLeafDeps() LeafToolDeps {
	return LeafToolDeps{Root: "/tmp/workspace-root", HTTPCl: &http.Client{}}
}

// equalStringSlice reports element-wise equality, treating nil and empty as equal
// (a skill-less agent's Skills is nil; the expectation is also nil).
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

// TestLeafRegistryHasExactlyTheTwoLeaves proves leafRegistry registers EXACTLY
// the two spawnable leaf agents — operator, reviewer — in that order. operator is also
// the swarm's primary loop (sourced from the shared operatorBuiltin), but the leaf it
// registers here has no Subagent tool, so a spawned operator cannot itself spawn.
func TestLeafRegistryHasExactlyTheTwoLeaves(t *testing.T) {
	t.Parallel()

	reg, _, err := leafRegistry(testLeafDeps(), Config{})
	if err != nil {
		t.Fatalf("leafRegistry() error = %v", err)
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

// TestLeafRegistryNoOrphanPrimary proves the retired "orchestrator" agent is NOT in the
// leaf registry: the primary loop is now an operator (which IS a leaf), and no separate
// primary-only agent lingers in the spawnable roster.
func TestLeafRegistryNoOrphanPrimary(t *testing.T) {
	t.Parallel()

	reg, _, err := leafRegistry(testLeafDeps(), Config{})
	if err != nil {
		t.Fatalf("leafRegistry() error = %v", err)
	}
	if _, ok := reg.Lookup(identity.AgentName("orchestrator")); ok {
		t.Error(`Lookup("orchestrator") = found, want absent (the retired orchestrator agent is not a leaf)`)
	}
}

// TestLeafRegistryLookupCarriesLeafData proves each registered leaf carries its
// own package's Name/Description/Role verbatim and a non-nil BuildTools that
// produces a tool set with a non-nil PermissionChecker.
func TestLeafRegistryLookupCarriesLeafData(t *testing.T) {
	t.Parallel()

	deps := testLeafDeps()
	reg, _, err := leafRegistry(deps, Config{})
	if err != nil {
		t.Fatalf("leafRegistry() error = %v", err)
	}

	tests := []struct {
		name              string
		agent             identity.AgentName
		wantDesc          string
		wantRole          string
		wantTools         []string
		wantSkills        []string // the agent's allowed-skill names (nil = none)
		wantRuntimeSkills bool     // §7a eligibility — true for the operator (extended), false for reviewer
	}{
		{
			name:              "operator",
			agent:             operator.Name,
			wantDesc:          operator.Description,
			wantRole:          operator.Role,
			wantTools:         []string{"AskUser", "Bash", "EditFile", "Fetch", "Glob", "Grep", "ReadFile", "Skill", "Todo", "WebSearch", "WriteFile"},
			wantSkills:        []string{"code-style"},
			wantRuntimeSkills: true,
		},
		{
			name:      "reviewer",
			agent:     reviewer.Name,
			wantDesc:  reviewer.Description,
			wantRole:  reviewer.Role,
			wantTools: []string{"AskUser", "Bash", "Glob", "Grep", "ReadFile", "Todo"},
		},
	}
	for _, tt := range tests {
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
				t.Errorf("AllowsRuntimeSkills = %v, want %v (§7a: operator eligible, reviewer not)", a.AllowsRuntimeSkills, tt.wantRuntimeSkills)
			}
			if !equalStringSlice(a.Skills, tt.wantSkills) {
				t.Errorf("Skills = %v, want %v", a.Skills, tt.wantSkills)
			}
			if a.BuildTools == nil {
				t.Fatal("BuildTools = nil, want non-nil")
			}
			ts := a.BuildTools(deps)
			if ts.Permission == nil {
				t.Error("BuildTools().Permission = nil, want non-nil PermissionChecker")
			}
			got := make([]string, 0, len(ts.Registry))
			for _, tl := range ts.Registry {
				info, err := tl.Info(t.Context())
				if err != nil {
					t.Fatalf("Info() error = %v", err)
				}
				got = append(got, info.Name)
			}
			sort.Strings(got)
			if len(got) != len(tt.wantTools) {
				t.Fatalf("tool names = %v, want %v", got, tt.wantTools)
			}
			for i := range tt.wantTools {
				if got[i] != tt.wantTools[i] {
					t.Fatalf("tool names = %v, want %v", got, tt.wantTools)
				}
			}
		})
	}
}
