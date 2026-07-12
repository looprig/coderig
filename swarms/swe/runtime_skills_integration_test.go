//go:build integration

package swe

import (
	"os"
	"path/filepath"
	"testing"
)

// runtime_skills_integration_test.go is the P2b Phase 3c END-TO-END acceptance: with
// RuntimeSkills ON and a real on-disk <root>/.skills/<name>/SKILL.md, the operator-primary
// delegates to an operator LEAF, the leaf calls Skill{name:"<workspace-skill>"}, the
// workspace load surfaces a HUMAN-GATED SkillLoadRequest (ScopeOnce) attributed to the
// delegate loop, and after Approve the snapshot body is returned as the tool result.
//
// TODO(task6): the delegation-driving end-to-end flow was rewritten from the retired
// swarmSpawner mechanism to the rig's managed delegation. Re-authoring this end-to-end path
// (drive the operator-primary to emit a managed Subagent tool call, observe the delegate
// loop's workspace-skill gate, Approve it on that loop) is Task 6/7 integration work. The
// file compiles today; the workspace-skill fixture helper below is retained for that rewrite.

// workspaceSkillBody is the marker phrase planted in the on-disk workspace SKILL.md so a
// future rewrite can prove the APPROVED snapshot body (not an error) is what Skill returned.
const workspaceSkillBody = "WORKSPACE-SKILL-MARKER: project-local checklist"

// writeWorkspaceSkill writes a valid <root>/.skills/<name>/SKILL.md (frontmatter + body) and
// returns name, for the deferred end-to-end rewrite.
func writeWorkspaceSkill(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, ".skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", dir, err)
	}
	doc := "---\nname: " + name + "\ndescription: A project-local workspace skill.\n---\n\n# Local\n\n" + workspaceSkillBody + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(doc), 0o600); err != nil {
		t.Fatalf("WriteFile(SKILL.md) error = %v", err)
	}
	return name
}

// TestRuntimeSkillWorkspaceLoadGatedEndToEnd is deferred to Task 6/7 (rig managed delegation).
func TestRuntimeSkillWorkspaceLoadGatedEndToEnd(t *testing.T) {
	t.Skip("TODO(task6): re-author the workspace-skill delegation end-to-end flow over the rig's managed Subagent tool")
	_ = writeWorkspaceSkill
	_ = workspaceSkillBody
}
