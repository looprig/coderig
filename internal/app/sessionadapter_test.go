package app

import (
	"context"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tui/sessionadapter"
)

type sessionAdapter = sessionadapter.Adapter

func newSessionAdapter(ctx context.Context, controller session.SessionController, replay sessionadapter.ReplayOpener, restored bool) (*sessionAdapter, error) {
	if restored {
		return sessionadapter.Restore(ctx, controller, replay)
	}
	return sessionadapter.New(controller), nil
}

func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New(): %v", err)
	}
	return id
}

type testWorkspaceCoordinator struct{}

func (*testWorkspaceCoordinator) Acquire(context.Context, tool.WorkspaceOperation, string) (tool.WorkspacePermit, error) {
	return testWorkspacePermit{}, nil
}

func (*testWorkspaceCoordinator) Healthy() error { return nil }

type testWorkspacePermit struct{}

func (testWorkspacePermit) Release() {}
