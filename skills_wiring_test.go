package coderig

import (
	"strings"
	"testing"

	"github.com/looprig/coderig/agents/operator"
	"github.com/looprig/coderig/agents/reviewer"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/tools/skill"
)

// skills_wiring_test.go proves the composition root wires the per-agent Skill tool and the
// trusted <available_skills> system-prompt catalog correctly: the operator (which owns the
// embedded code-style skill) gets both; the reviewer (no skills) gets neither.

// testSkillLoader builds the embedded skill loader over the roster's allow-map, exactly as
// swarmDefinitions does.
func testSkillLoader() skill.SkillLoader {
	scopes := []skillScope{
		{name: operator.Name, skills: operatorSkills},
		{name: reviewer.Name},
	}
	return skill.NewEmbeddedSkillLoader(SkillsFS, buildSkillAllow(scopes))
}

// TestDefinitionSystemPromptsCarrySkillCatalog proves the operator loops' system prompt carries
// the <available_skills> catalog naming code-style, and the reviewer's does not.
func TestDefinitionSystemPromptsCarrySkillCatalog(t *testing.T) {
	t.Parallel()

	defs, err := swarmDefinitions(&fakeLLM{}, testModel(), Config{})
	if err != nil {
		t.Fatalf("swarmDefinitions() error = %v", err)
	}
	byName := map[string]loop.Definition{}
	for _, d := range defs {
		byName[string(d.Name())] = d
	}

	operatorSys := byName[string(operator.Name)].FingerprintInitial().EffectiveSystem
	if !strings.Contains(operatorSys, "<available_skills>") || !strings.Contains(operatorSys, "code-style") {
		t.Errorf("operator system prompt missing the skill catalog: %q", operatorSys)
	}
	reviewerSys := byName[string(reviewer.Name)].FingerprintInitial().EffectiveSystem
	if strings.Contains(reviewerSys, "<available_skills>") {
		t.Errorf("reviewer system prompt unexpectedly carries a skill catalog: %q", reviewerSys)
	}
}

// TestSkillDefinitionForGating proves skillDefinitionFor honors the §7a gate: the operator (an
// embedded-skill owner) always gets a Skill definition; the reviewer (no skills, not
// runtime-eligible) never does.
func TestSkillDefinitionForGating(t *testing.T) {
	t.Parallel()
	loader := testSkillLoader()

	tests := []struct {
		name    string
		builtin leafBuiltin
		cfg     Config
		wantNil bool
	}{
		{name: "operator embedded-only", builtin: operatorBuiltin(), cfg: Config{}, wantNil: false},
		{name: "operator runtime-skills on", builtin: operatorBuiltin(), cfg: Config{RuntimeSkills: true}, wantNil: false},
		{name: "reviewer no skills off", builtin: reviewerBuiltin(), cfg: Config{}, wantNil: true},
		{name: "reviewer not runtime-eligible", builtin: reviewerBuiltin(), cfg: Config{RuntimeSkills: true}, wantNil: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			def := skillDefinitionFor(loader, tt.builtin, tt.cfg)
			if (def == nil) != tt.wantNil {
				t.Errorf("skillDefinitionFor() nil = %v, want %v", def == nil, tt.wantNil)
			}
		})
	}
}

// TestBuildSkillAllowAuthorizesOnlyDeclaredSkills proves the loader allow-map authorizes the
// operator's code-style skill and nothing for the reviewer.
func TestBuildSkillAllowAuthorizesOnlyDeclaredSkills(t *testing.T) {
	t.Parallel()

	allow := buildSkillAllow([]skillScope{
		{name: operator.Name, skills: operatorSkills},
		{name: reviewer.Name},
	})
	opSkills, ok := allow[operator.Name]
	if !ok {
		t.Fatalf("operator absent from the allow-map")
	}
	if _, ok := opSkills["code-style"]; !ok {
		t.Errorf("operator allow-map = %v, want it to authorize code-style", opSkills)
	}
	if _, ok := allow[reviewer.Name]; ok {
		t.Errorf("reviewer present in the allow-map, want absent (no skills)")
	}
}
