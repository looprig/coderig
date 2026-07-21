package app

import (
	"context"
	"testing"

	"github.com/looprig/tui"
)

func TestRuntimeCatalogExposesModesAndModel(t *testing.T) {
	agent, _ := openAcceptanceAgent(t)

	options, err := agent.LoopRuntimeOptions(context.Background(), agent.ActiveLoopID())
	if err != nil {
		t.Fatal(err)
	}
	if len(options.Modes) == 0 || options.Modes[0].ID != tui.ModeID("") {
		t.Fatalf("modes = %#v, want declared base mode", options.Modes)
	}
	if len(options.Models) != 1 || options.Models[0].ID == "" {
		t.Fatalf("models = %#v, want one stable current model", options.Models)
	}
}

// TestSessionPresentationReportsFixedProfile proves the runtime agent surfaces the
// session-fixed access profile name and workspace root through the TUI's
// SessionPresenter contract. The default (empty) Config resolves to the readonly
// profile.
func TestSessionPresentationReportsFixedProfile(t *testing.T) {
	agent, _ := openAcceptanceAgent(t)

	var presenter tui.SessionPresenter = agent
	presentation := presenter.SessionPresentation()
	if presentation.ProfileName != string(DefaultAccessProfile) {
		t.Fatalf("ProfileName = %q, want %q", presentation.ProfileName, DefaultAccessProfile)
	}
	if presentation.WorkspaceRoot == "" {
		t.Fatal("WorkspaceRoot is empty, want the session workspace root")
	}
	// A clean headless read-only store carries no out-of-catalog family diagnostics.
	if len(presentation.PermissionDiagnostics) != 0 {
		t.Fatalf("PermissionDiagnostics = %v, want none for a clean store", presentation.PermissionDiagnostics)
	}
}

func TestRuntimeControllerRejectsUnknownTypedChoices(t *testing.T) {
	agent, _ := openAcceptanceAgent(t)
	if err := agent.SetModel(context.Background(), agent.ActiveLoopID(), tui.ModelID("unknown/model")); err == nil {
		t.Fatal("SetModel(unknown) succeeded")
	}
	if err := agent.SetEffort(context.Background(), agent.ActiveLoopID(), tui.EffortID("impossible")); err == nil {
		t.Fatal("SetEffort(unknown) succeeded")
	}
}
