// Package leafrig holds the per-binding tool mechanism shared by the SWE-Swarm's
// leaf agents (agents/operator, agents/reviewer) and — from Task 3 — the operator
// primary. It is the ONE place the generic per-binding definition builders, the
// fresh-per-bind permission factory, and the cross-agent policy-revision identity
// live, so the three consumers cannot drift (the sharpest risk is PolicyRevision:
// the definition of cross-agent policy identity, which must not exist in two places).
//
// It lives under agents/internal so it stays invisible outside the roster, preserving
// each leaf's "boundary as pure data, no import cycle" intent: leafrig depends only on
// harness + the confine seam, never on swarms/swe. Each agent package keeps only its
// genuinely-distinct boundary data (Name/Description/Role, its roster/approved-set,
// and the operator-only Files write/edit vs the reviewer-only read-only ReadFile wrap).
package leafrig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/tools"
	"github.com/looprig/swe/confine"
)

// Tool names the generic definitions declare (built tools' Info().Name must match —
// loop.Bind enforces this). Local constants because the harness counterparts are
// unexported; a drift is caught loudly at bind time.
const (
	GlobToolName    = "Glob"
	GrepToolName    = "Grep"
	BashToolName    = "Bash"
	TodoToolName    = "Todo"
	AskUserToolName = "AskUser"
	// SkillToolName is the name each agent adds to its hard-approve set when a Skill
	// definition is wired (the Skill definition itself is agent-supplied).
	SkillToolName = "Skill"
)

// Tools is a leaf's per-binding rig contribution: the immutable tool Definitions
// (each builds fresh, workspace-bound concrete tools per loop binding), the
// fresh-per-bind PermissionFactory that mints the gate from immutable policy + the
// bound session ceiling, and a stable PolicyRevision digest. The composition root
// feeds these into loop.Define via WithTools / WithPermissionFactory /
// WithPolicyRevision. Nothing session-specific (root, checker, executor, observation
// set) is captured; every collaborator is constructed inside a factory that reads
// tool.Bindings at bind time.
type Tools struct {
	Definitions    []tool.Definition
	Permission     loop.PermissionFactory
	PolicyRevision string
}

// ToolSetError reports that a leaf's tool set could not be constructed for the named
// Agent. The failure mode is the fail-secure PermissionChecker refusing to build
// because $HOME is unresolvable while a home-relative ("~/…") deny pattern is
// configured. It wraps the underlying cause (e.g. *tools.HomeUnresolvableError) so a
// caller can errors.As it, and it exists so a leaf never fails OPEN (a checker-less
// tool set) on that error. Agent carries the leaf name so the message prefix
// ("operator: …" / "reviewer: …") is preserved across the shared code.
type ToolSetError struct {
	Agent string
	Cause error
}

func (e *ToolSetError) Error() string {
	msg := e.Agent + ": cannot build tool set"
	if e.Cause != nil {
		return msg + ": " + e.Cause.Error()
	}
	return msg
}

func (e *ToolSetError) Unwrap() error { return e.Cause }

// MissingWorkspaceError reports that the fresh permission gate could not be built for
// the named Agent because the loop binding carried no workspace (root). The checker's
// containment root is mandatory, so the leaf refuses to build a gate rather than run
// with an empty root — fail-secure.
type MissingWorkspaceError struct{ Agent string }

func (e *MissingWorkspaceError) Error() string {
	return e.Agent + ": permission gate requires a workspace binding"
}

// GlobDefinition builds a workspace-bound Glob per bind (fresh root + shared static
// read guard).
func GlobDefinition(guard loop.ReadGuard) tool.Definition {
	return tool.NewDefinition(GlobToolName, tool.RequiresWorkspace, func(_ context.Context, b tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{tools.NewGlob(b.Workspace.Root, guard)}, nil
	})
}

// GrepDefinition builds a workspace-bound Grep per bind, routing ripgrep through the
// per-bind sandbox read-only view (confFactory.For), else direct execution.
func GrepDefinition(guard loop.ReadGuard, confFactory confine.Factory) tool.Definition {
	return tool.NewDefinition(GrepToolName, tool.RequiresWorkspace, func(_ context.Context, b tool.Bindings) ([]tool.InvokableTool, error) {
		conf, err := confFactory.For(b)
		if err != nil {
			return nil, err
		}
		return []tool.InvokableTool{tools.NewGrep(b.Workspace.Root, guard, conf.GrepOptions()...)}, nil
	})
}

// BashDefinition builds a workspace-bound Bash per bind: the confined runner + the
// loop's workspace coordinator and shared observation set all come from the binding
// (a Bash whole-workspace mutation invalidates exactly this loop's file observations).
func BashDefinition(confFactory confine.Factory) tool.Definition {
	return tool.NewDefinition(BashToolName, tool.RequiresWorkspace, func(_ context.Context, b tool.Bindings) ([]tool.InvokableTool, error) {
		conf, err := confFactory.For(b)
		if err != nil {
			return nil, err
		}
		opts := append(conf.BashOptions(),
			tools.WithWorkspaceCoordinator(b.Workspace.Coordinator),
			tools.WithObservations(b.Workspace.Observations),
		)
		return []tool.InvokableTool{tools.NewBash(b.Workspace.Root, opts...)}, nil
	})
}

// TodoDefinition builds a self-contained Todo per bind (filesystem-free).
func TodoDefinition() tool.Definition {
	return tool.NewDefinition(TodoToolName, 0, func(_ context.Context, _ tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{tools.NewTodo()}, nil
	})
}

// AskUserDefinition builds a self-contained AskUser per bind (filesystem-free).
func AskUserDefinition() tool.Definition {
	return tool.NewDefinition(AskUserToolName, 0, func(_ context.Context, _ tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{tools.NewAskUser()}, nil
	})
}

// NewReadGuard builds the leaf's static, root-independent ReadGuard: a fail-secure
// *tools.PermissionChecker over DefaultHardDeny only (no posture, no root). Its
// DeniedRead/MaxReadBytes/DeniedReadGlobs read only the immutable hard-deny read
// policy, so ONE guard is safely shared across every bind's read tools (Files/Glob/
// Grep) while the permission GATE stays fresh per bind. It fails ($HOME unresolvable
// while a "~/…" pattern is configured) so a caller can fail closed at BuildTools time.
func NewReadGuard() (loop.ReadGuard, error) {
	return tools.NewPermissionChecker(tools.PermissionPolicy{HardDeny: tools.DefaultHardDeny()})
}

// NewPermissionFactory returns the leaf's fresh-per-bind PermissionFactory: a NEW
// fail-secure *tools.PermissionChecker per bind from the immutable hard-deny/
// hard-approve policy, the bound workspace root (containment), and the per-bind
// ceiling-posture Option from confFactory.For (which carries min(role, bindings.Ceiling)
// + the SAME sandbox executor as Bash/Grep). A missing workspace fails closed with a
// typed *MissingWorkspaceError; a checker-build failure threads up as a typed
// *ToolSetError so the loop never binds an unguarded gate. agent labels both errors.
func NewPermissionFactory(agent string, confFactory confine.Factory, approved []string) loop.PermissionFactory {
	hardApprove := append([]string(nil), approved...)
	return func(_ context.Context, b tool.Bindings) (loop.PermissionGate, error) {
		if b.Workspace == nil {
			return nil, &MissingWorkspaceError{Agent: agent}
		}
		conf, err := confFactory.For(b)
		if err != nil {
			return nil, err
		}
		policy := tools.PermissionPolicy{
			WorkspaceRoot: b.Workspace.Root,
			HardDeny:      tools.DefaultHardDeny(),
			HardApprove:   tools.HardApproveRules{Tools: hardApprove},
		}
		pc, err := tools.NewPermissionChecker(policy, conf.CheckerOptions()...)
		if err != nil {
			return nil, &ToolSetError{Agent: agent, Cause: err}
		}
		return pc, nil
	}
}

// PolicyRevision derives a stable, secret-free, MACHINE-INDEPENDENT digest of a leaf's
// IMMUTABLE policy: the agent name, the sorted hard-approve set, the sorted produced
// tool names across every definition, and the hard-deny read/write/bash pattern set +
// read cap. It changes iff the policy changes (e.g. the Skill tool is added) and is
// byte-identical across binds of the same definition AND across machines — the deny
// patterns are unexpanded "~/…" literals (DefaultHardDeny never resolves $HOME), so no
// absolute home path leaks in. This is the stable identity loop.WithPolicyRevision
// requires for an opaque permission collaborator; it MUST have exactly one definition.
func PolicyRevision(agent string, approved []string, defs []tool.Definition) string {
	produced := make([]string, 0, len(defs))
	for _, d := range defs {
		produced = append(produced, d.ProducedToolNames()...)
	}
	sort.Strings(produced)
	app := append([]string(nil), approved...)
	sort.Strings(app)
	deny := tools.DefaultHardDeny()

	var b strings.Builder
	writeField := func(label string, vals ...string) {
		b.WriteString(label)
		b.WriteByte('=')
		for _, v := range vals {
			b.WriteString(v)
			b.WriteByte('\x1f') // unit separator
		}
		b.WriteByte('\x1e') // record separator
	}
	writeField("agent", agent)
	writeField("approved", app...)
	writeField("tools", produced...)
	writeField("deniedRead", deny.DeniedReadPaths...)
	writeField("deniedWrite", deny.DeniedWritePaths...)
	writeField("deniedBash", deny.DeniedBashPrefixes...)
	writeField("maxRead", strconv.FormatInt(deny.MaxReadBytes, 10))

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
