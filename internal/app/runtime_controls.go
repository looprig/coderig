package app

import (
	"context"
	"fmt"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	model "github.com/looprig/inference/model"
	"github.com/looprig/tui"
	"github.com/looprig/tui/sessionadapter"
)

// RuntimeAgent keeps provider and policy knowledge in CodeRig while embedding the stable
// session adapter used by the TUI data plane. It OWNS the session's executor-set closers
// (through access) and supplies the synchronous, session-fixed presentation metadata
// (profile name, workspace root, permission diagnostics) the TUI displays. The access
// profile is fixed at Open; there is no in-session authority mutation surface.
type RuntimeAgent struct {
	*sessionadapter.Adapter
	sess   session.SessionController
	root   string
	access *sessionAccess
}

func newRuntimeAgent(adapter *sessionadapter.Adapter, sess session.SessionController, root string, access *sessionAccess) *RuntimeAgent {
	return &RuntimeAgent{Adapter: adapter, sess: sess, root: root, access: access}
}

// Close shuts the session down and then releases the session's executor sets exactly once.
// The adapter is closed FIRST (stopping any in-flight loop that could still use an executor),
// then the executor sets are closed, removing their owned scratch HOME directories and
// revoking their grant keys and egress proxies.
func (a *RuntimeAgent) Close(ctx context.Context) error {
	err := a.Adapter.Close(ctx)
	if a.access != nil {
		if closeErr := a.access.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	return err
}

// SessionPresentation supplies the TUI's synchronous session security context: the fixed
// access profile name, the workspace root, and the manual out-of-catalog permission
// diagnostics surfaced by the workspace permission-store load. The TUI reads it at screen
// construction and on cross-session resume so it always displays THIS session's context.
func (a *RuntimeAgent) SessionPresentation() tui.SessionPresentation {
	presentation := tui.SessionPresentation{WorkspaceRoot: a.root}
	if a.access != nil {
		presentation.ProfileName = a.access.profileName
		presentation.PermissionDiagnostics = a.access.diagnostics
		if presentation.WorkspaceRoot == "" {
			presentation.WorkspaceRoot = a.access.workspace
		}
	}
	return presentation
}

func (a *RuntimeAgent) LoopRuntimeOptions(_ context.Context, loopID uuid.UUID) (tui.LoopRuntimeOptions, error) {
	handle, ok := a.sess.Loop(loopID)
	if !ok {
		return tui.LoopRuntimeOptions{}, fmt.Errorf("coderig: loop %s is unavailable", loopID)
	}
	options := tui.LoopRuntimeOptions{}
	if catalog, ok := handle.(loop.ModeCatalog); ok {
		for _, mode := range catalog.Modes() {
			label := string(mode)
			if label == "" {
				label = "Default"
			}
			options.Modes = append(options.Modes, tui.ModeOption{ID: tui.ModeID(mode), Label: label})
		}
	}
	selectedModel := handle.Model()
	options.Models = []tui.ModelOption{{ID: tui.ModelID(modelID(selectedModel)), Label: selectedModel.Name, Description: string(selectedModel.Provider)}}
	if selectedModel.Caps.Thinking {
		for _, effort := range []model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortMax} {
			label := string(effort)
			if label == "" {
				label = "Model default"
			}
			options.Efforts = append(options.Efforts, tui.EffortOption{ID: tui.EffortID(effort), Label: label})
		}
	}
	return options, nil
}

func (a *RuntimeAgent) SetMode(ctx context.Context, loopID uuid.UUID, id tui.ModeID) error {
	controller, ok := a.sess.LoopController(loopID)
	if !ok {
		return fmt.Errorf("coderig: loop %s is unavailable", loopID)
	}
	return controller.SetMode(ctx, loop.ModeName(id))
}

func (a *RuntimeAgent) SetModel(ctx context.Context, loopID uuid.UUID, id tui.ModelID) error {
	controller, ok := a.sess.LoopController(loopID)
	if !ok {
		return fmt.Errorf("coderig: loop %s is unavailable", loopID)
	}
	selectedModel := controller.Model()
	if modelID(selectedModel) != string(id) {
		return fmt.Errorf("coderig: model choice %q is stale or unknown", id)
	}
	return controller.Change(ctx, loop.ChangeModel(selectedModel))
}

func (a *RuntimeAgent) SetEffort(ctx context.Context, loopID uuid.UUID, id tui.EffortID) error {
	controller, ok := a.sess.LoopController(loopID)
	if !ok {
		return fmt.Errorf("coderig: loop %s is unavailable", loopID)
	}
	effort := model.Effort(id)
	if !effort.Valid() {
		return fmt.Errorf("coderig: effort choice %q is unknown", id)
	}
	return controller.Change(ctx, loop.ChangeEffort(effort))
}

func modelID(value model.Model) string { return string(value.Provider) + "/" + value.Name }

var (
	_ tui.Agent             = (*RuntimeAgent)(nil)
	_ tui.RuntimeCatalog    = (*RuntimeAgent)(nil)
	_ tui.RuntimeController = (*RuntimeAgent)(nil)
	_ tui.SessionPresenter  = (*RuntimeAgent)(nil)
)
