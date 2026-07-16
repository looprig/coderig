package app

import (
	"context"
	"testing"

	"github.com/looprig/tui"
)

func TestRuntimeCatalogExposesModesModelAndBoundedAccess(t *testing.T) {
	adapter, _ := openAcceptanceAgent(t)
	agent := newRuntimeAgent(adapter, adapter.Controller(), t.TempDir(), 2)

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
	access, err := agent.AccessOptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(access.Choices) != 3 || access.Choices[2].Label != "Writable" {
		t.Fatalf("access choices = %#v, want levels through configured cap", access.Choices)
	}
}

func TestRuntimeControllerRejectsUnknownTypedChoices(t *testing.T) {
	adapter, _ := openAcceptanceAgent(t)
	agent := newRuntimeAgent(adapter, adapter.Controller(), t.TempDir(), 2)
	if err := agent.SetModel(context.Background(), agent.ActiveLoopID(), tui.ModelID("unknown/model")); err == nil {
		t.Fatal("SetModel(unknown) succeeded")
	}
	if err := agent.SetEffort(context.Background(), agent.ActiveLoopID(), tui.EffortID("impossible")); err == nil {
		t.Fatal("SetEffort(unknown) succeeded")
	}
	if err := agent.SetAccess(context.Background(), tui.AccessID("3")); err == nil {
		t.Fatal("SetAccess(over cap) succeeded")
	}
}
