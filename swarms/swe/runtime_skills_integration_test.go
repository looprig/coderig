//go:build integration

package swe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

// runtime_skills_integration_test.go is the P2b Phase 3c END-TO-END acceptance: with
// RuntimeSkills ON and a real on-disk <root>/.skills/<name>/SKILL.md, the operator-primary
// delegates to an operator LEAF, the leaf calls Skill{name:"<workspace-skill>"}, the
// workspace load surfaces a HUMAN-GATED SkillLoadRequest (ScopeOnce) attributed to the
// delegate loop, and after Approve the snapshot body is returned as the tool result.
//
// workspaceSkillBody is the marker phrase planted in the on-disk workspace SKILL.md so a
// future rewrite can prove the APPROVED snapshot body (not an error) is what Skill returned.
const workspaceSkillBody = "WORKSPACE-SKILL-MARKER: project-local checklist"

// writeWorkspaceSkill writes a valid <root>/.skills/<name>/SKILL.md (frontmatter + body) and
// returns name.
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

func TestRuntimeSkillsWorkspaceLoadGatedEndToEnd(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	name := writeWorkspaceSkill(t, root, "project-checklist")
	step := 0
	var skillResult string
	client := &managedScript{}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if strings.Contains(req.System, operatorDelegation) {
			if step == 0 {
				step++
				return toolCall("skill-delegate", `{"agent":"operator","message":"load the project checklist","wait":true}`), nil
			}
			return finalText("runtime skill complete"), nil
		}
		if step == 1 {
			step++
			return []content.Chunk{&content.ToolUseChunk{Index: 0, ID: "skill-load", Name: "Skill", InputJSON: fmt.Sprintf(`{"name":%q}`, name)}}, nil
		}
		skillResult = lastToolText(req)
		return finalText("delegate consumed skill"), nil
	}
	f := newIntegrationFactory(t)
	a, err := f.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{}, Config{RuntimeSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	sub, err := a.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	commandID, err := a.Submit(ctx, []content.Block{&content.TextBlock{Text: "delegate skill load"}})
	if err != nil {
		t.Fatal(err)
	}
	var childID, turnID uuid.UUID
	approved := false
	var observed []event.Event
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("runtime-skill flow timed out after %s: %v", eventTypes(observed), ctx.Err())
		case delivery, ok := <-sub.Events():
			if !ok {
				t.Fatalf("runtime-skill subscription closed after %s", eventTypes(observed))
			}
			observed = append(observed, delivery.Event)
			switch ev := delivery.Event.(type) {
			case event.LoopStarted:
				if !ev.Cause.Coordinates.LoopID.IsZero() {
					childID = ev.LoopID
				}
			case event.GateOpened:
				if ev.EventHeader().LoopID != childID || childID.IsZero() {
					t.Fatalf("workspace-skill gate loop = %v, want delegate %v", ev.EventHeader().LoopID, childID)
				}
				fields := ev.Gate.Prompt.Schema.Fields
				if len(fields) != 1 || len(fields[0].Options) != 1 || fields[0].Options[0].Value != "once" {
					t.Fatalf("workspace-skill scope prompt = %+v, want only once", fields)
				}
				if err := a.Approve(ctx, childID, ev.Gate.Subject.ToolExecutionID, tool.ScopeOnce); err != nil {
					t.Fatal(err)
				}
				approved = true
			case event.TurnStarted:
				if ev.Cause.CommandID == commandID {
					turnID = ev.TurnID
				}
			case event.TurnDone:
				if ev.TurnID == turnID && !turnID.IsZero() {
					if !approved {
						t.Fatal("turn completed without a workspace-skill approval")
					}
					if !strings.Contains(skillResult, workspaceSkillBody) {
						t.Fatalf("skill result = %q", skillResult)
					}
					return
				}
			case event.TurnFailed:
				t.Fatalf("runtime-skill turn failed on loop %v: %v", ev.EventHeader().LoopID, ev.Err)
			}
		}
	}
}
