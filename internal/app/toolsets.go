package app

import (
	"context"
	"net/http"

	"github.com/looprig/confinement"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/sandbox"
	"github.com/looprig/tools"
	"github.com/looprig/tools/permission"
	"github.com/looprig/tools/websearch"
)

type loopTools struct {
	definitions    []tool.Definition
	permission     loop.PermissionFactory
	policyRevision string
}

func newReadGuard() (loop.ReadGuard, error) {
	return permission.NewPermissionChecker(permission.PermissionPolicy{HardDeny: permission.DefaultHardDeny()})
}

func newConfinement(maxMode sandbox.Mode) (*confinement.Factory, error) {
	return confinement.New(
		confinement.Config{MaxMode: maxMode, MaxBindings: operatorSpawnQuota + 1},
		confinement.WithGatedFallback(),
	)
}

func permissionFactory(agent string, factory *confinement.Factory, approved []string) loop.PermissionFactory {
	approved = append([]string(nil), approved...)
	return func(_ context.Context, bindings tool.Bindings) (loop.PermissionGate, error) {
		if bindings.Workspace == nil {
			return nil, &WorkspaceRootError{}
		}
		confined, err := factory.For(bindings)
		if err != nil {
			return nil, err
		}
		policy := permission.PermissionPolicy{
			WorkspaceRoot: bindings.Workspace.Root,
			HardDeny:      permission.DefaultHardDeny(),
			HardApprove:   permission.HardApproveRules{Tools: approved},
		}
		checker, err := permission.NewPermissionChecker(policy, confined.PermissionOptions()...)
		if err != nil {
			return nil, &LoopDefinitionError{Agent: agent, Cause: err}
		}
		return checker, nil
	}
}

func policyRevision(approved []string) string {
	return permission.PolicyFingerprint(permission.PermissionPolicy{
		HardDeny:    permission.DefaultHardDeny(),
		HardApprove: permission.HardApproveRules{Tools: approved},
	}, permission.FingerprintMode{})
}

func buildOperatorTools(factory *confinement.Factory, client *http.Client, skill tool.Definition) (loopTools, error) {
	guard, err := newReadGuard()
	if err != nil {
		return loopTools{}, err
	}
	approved := []string{"ReadFile", "Glob", "Grep", "Todo", "AskUser"}
	definitions := []tool.Definition{
		tools.ReadFileDefinition(guard),
		tools.WriteFileDefinition(),
		tools.EditFileDefinition(),
		tools.GlobDefinition(guard),
		factory.GrepDefinition(guard),
		factory.BashDefinition(),
		tools.WebSearchDefinition(websearch.NewDuckDuckGoProvider(client)),
		tools.FetchDefinition(client),
		tools.TodoDefinition(),
		tools.AskUserDefinition(),
	}
	if skill != nil {
		definitions = append(definitions, skill)
		approved = append(approved, skillToolName)
	}
	return loopTools{
		definitions:    definitions,
		permission:     permissionFactory("operator", factory, approved),
		policyRevision: policyRevision(approved),
	}, nil
}

func buildReviewerTools(factory *confinement.Factory, skill tool.Definition) (loopTools, error) {
	guard, err := newReadGuard()
	if err != nil {
		return loopTools{}, err
	}
	approved := []string{"ReadFile", "Glob", "Grep", "Todo", "AskUser"}
	definitions := []tool.Definition{
		tools.ReadFileDefinition(guard),
		tools.GlobDefinition(guard),
		factory.GrepDefinition(guard),
		factory.BashDefinition(),
		tools.TodoDefinition(),
		tools.AskUserDefinition(),
	}
	if skill != nil {
		definitions = append(definitions, skill)
		approved = append(approved, skillToolName)
	}
	return loopTools{
		definitions:    definitions,
		permission:     permissionFactory("reviewer", factory, approved),
		policyRevision: policyRevision(approved),
	}, nil
}
