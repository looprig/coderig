package app

import (
	"context"
	"fmt"
	"strconv"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/security"
	"github.com/looprig/harness/pkg/session"
	model "github.com/looprig/inference/model"
	"github.com/looprig/tui"
	"github.com/looprig/tui/sessionadapter"
)

// RuntimeAgent keeps provider and policy knowledge in CodeRig while embedding the stable
// session adapter used by the TUI data plane.
type RuntimeAgent struct {
	*sessionadapter.Adapter
	sess      session.SessionController
	root      string
	maxAccess security.Level
}

func newRuntimeAgent(adapter *sessionadapter.Adapter, sess session.SessionController, root string, maxAccess uint8) *RuntimeAgent {
	return &RuntimeAgent{Adapter: adapter, sess: sess, root: root, maxAccess: security.Level(maxAccess)}
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

func (a *RuntimeAgent) AccessOptions(context.Context) (tui.AccessOptions, error) {
	labels := []string{"Untrusted", "Read Only", "Writable", "Trusted", "Unconfined"}
	options := tui.AccessOptions{Root: a.root}
	for level := security.Level(0); level <= a.maxAccess && int(level) < len(labels); level++ {
		options.Choices = append(options.Choices, tui.AccessOption{
			ID:          tui.AccessID(strconv.FormatUint(uint64(level), 10)),
			Label:       labels[level],
			Description: accessDescription(level),
		})
	}
	return options, nil
}

func accessDescription(level security.Level) string {
	switch level {
	case 0:
		return "workspace-only reads with network denied"
	case 1:
		return "broad reads with writes gated"
	case 2:
		return "writes confined to workspace and temporary files"
	case 3:
		return "sandboxed writes with trusted network access"
	case 4:
		return "full user-level authority"
	default:
		return ""
	}
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

func (a *RuntimeAgent) SetAccess(ctx context.Context, id tui.AccessID) error {
	ordinal, err := strconv.ParseUint(string(id), 10, 8)
	if err != nil || security.Level(ordinal) > a.maxAccess {
		return fmt.Errorf("coderig: access choice %q is unknown or exceeds the configured cap", id)
	}
	return a.sess.SetSecurityLimit(ctx, security.Level(ordinal))
}

func modelID(value model.Model) string { return string(value.Provider) + "/" + value.Name }

var _ tui.Agent = (*RuntimeAgent)(nil)
var _ tui.RuntimeCatalog = (*RuntimeAgent)(nil)
var _ tui.RuntimeController = (*RuntimeAgent)(nil)
