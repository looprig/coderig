package coderig

import (
	"context"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/inference"
)

func TestSessionBrowserExcludesCurrentAndResumesSelected(t *testing.T) {
	factory, err := NewSessionStoreFactory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = factory.Close() })
	factory.buildClient = func() (inference.Client, ModelFactory, error) {
		return &fakeLLM{}, newModelFactoryFor(testModel()), nil
	}
	cfg := Config{}
	first, err := factory.Open(context.Background(), SessionSelector{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	firstID := first.SessionID()
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := factory.Open(context.Background(), SessionSelector{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	browser := factory.SessionBrowser(cfg)
	sessions, err := browser.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != firstID {
		t.Fatalf("sessions = %#v, want only previous %s", sessions, firstID)
	}
	if err := second.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	resumed, err := browser.ResumeSession(context.Background(), firstID)
	if err != nil {
		t.Fatal(err)
	}
	identified, ok := resumed.(interface{ SessionID() uuid.UUID })
	if !ok || identified.SessionID() != firstID {
		t.Fatalf("resumed session identity = %#v, want %s", identified, firstID)
	}
	if err := resumed.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
