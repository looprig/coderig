// Package reviewer is the SWE-Swarm's critique leaf agent. It exposes its
// boundary as pure data (Name, Description, Role) and a raw-signature BuildTools
// so the swarm composition root can adapt it into a swe.Agent WITHOUT this
// package importing swarms/swe (which would be an import cycle). It is a leaf: it
// cannot spawn and it never mutates the filesystem — it reads, may run tests/
// build via Bash to verify claims, and reports findings. It does not fix.
package reviewer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/tools"
	"github.com/looprig/swe/confine"
)

// Name is the reviewer's immutable attribution name.
const Name = identity.AgentName("reviewer")

// Description is the one-line summary the Subagent catalog and greeting render.
const Description = "Critiques code and verifies it with tests/build; reports findings, never fixes."

// Role is the reviewer's role prompt: a single well-formed
// <role name="reviewer"> element, identity-free (the swarm prepends the shared
// identity). It pins critique-don't-fix, the ability to run tests/build via
// Bash, and report-don't-mutate.
const Role = `<role name="reviewer">
  <mission>You critique code: correctness, security, design, and adherence to the project's standards. You assess and report — you do NOT fix. Fixing is the implementer's job; your job is to find what is wrong and say why.</mission>
  <method>
    <item>Read the change and its context, then verify your claims: you may run the test suite or build via Bash to confirm a failure rather than assert it from inspection alone.</item>
    <item>Report findings as a prioritized list — each with the file, line range, the problem, and why it matters. Distinguish blocking defects from nits.</item>
  </method>
  <boundary>Never edit, write, or otherwise mutate the workspace; you have no write tools. If a fix is needed, describe it precisely for the implementer instead of applying it.</boundary>
</role>`

// ToolSetError reports that reviewer's tool set could not be constructed. Currently the
// only failure mode is the fail-secure PermissionChecker refusing to build because $HOME is
// unresolvable while a home-relative ("~/…") deny pattern is configured. It wraps the
// underlying cause (e.g. *tools.HomeUnresolvableError) so a caller can errors.As it, and it
// exists so BuildTools never fails OPEN (returning a checker-less tool set) on that error.
type ToolSetError struct{ Cause error }

func (e *ToolSetError) Error() string {
	if e.Cause == nil {
		return "reviewer: cannot build tool set"
	}
	return "reviewer: cannot build tool set: " + e.Cause.Error()
}

func (e *ToolSetError) Unwrap() error { return e.Cause }

// autoApprovedTools is reviewer's hard-approve set: everything EXCEPT Bash. Bash
// runs a shell, so it stays Ask — a human reads and approves each command before
// it runs (the permission gate is the security boundary). The read/todo/ask
// tools are side-effect-free and run without prompting. Names match each tool's
// Info().Name exactly.
var autoApprovedTools = []string{"ReadFile", "Glob", "Grep", "Todo", "AskUser"}

// MissingWorkspaceError reports that the fresh permission gate could not be built
// because the loop binding carried no workspace (root) — fail-secure: the checker's
// containment root is mandatory, so the leaf refuses to build a gate.
type MissingWorkspaceError struct{}

func (*MissingWorkspaceError) Error() string {
	return "reviewer: permission gate requires a workspace binding"
}

// ReadFileUnavailableError reports the impossible case that the wrapped tools.Files
// bundle did not produce a ReadFile tool. It exists so the read-only ReadFile
// definition fails LOUDLY (never a nil tool) if the harness Files contract ever
// changes out from under the wrapper.
type ReadFileUnavailableError struct{}

func (*ReadFileUnavailableError) Error() string {
	return "reviewer: tools.Files produced no ReadFile to expose read-only"
}

// Tools is reviewer's per-binding rig contribution: the immutable tool Definitions,
// the fresh-per-bind PermissionFactory, and a stable PolicyRevision. The composition
// root feeds these into loop.Define via WithTools / WithPermissionFactory /
// WithPolicyRevision. Nothing session-specific is captured; every collaborator is
// constructed inside a factory that reads tool.Bindings at bind time.
type Tools struct {
	Definitions    []tool.Definition
	Permission     loop.PermissionFactory
	PolicyRevision string
}

// Tool names the leaf's definitions declare (built tools' Info().Name must match —
// loop.Bind enforces this). Local constants because the harness counterparts are
// unexported; a drift is caught loudly at bind time.
const (
	toolReadFile = "ReadFile"
	toolGlob     = "Glob"
	toolGrep     = "Grep"
	toolBash     = "Bash"
	toolTodo     = "Todo"
	toolSkill    = "Skill"
)

// BuildTools returns reviewer's per-binding tool Definitions (ReadFile, Glob, Grep,
// Bash, Todo, AskUser — critique with the ability to run tests/build via Bash), a
// fresh-per-bind PermissionFactory, and a stable PolicyRevision. There is deliberately
// NO write/edit tool (reviewer critiques, it never mutates) and NO Subagent (a leaf
// cannot spawn).
//
// EVERY factory reads tool.Bindings.Workspace.Root and constructs fresh collaborators
// at bind time. The read tools share ONE static ReadGuard (immutable, root-independent
// hard-deny read policy); the permission GATE is a FRESH *tools.PermissionChecker per
// bind. confFactory is the per-bind OS-sandbox seam (SPEC §10.2): Grep, Bash, and the
// permission factory call confFactory.For(bindings) for the confined read-only view /
// runner / ceiling-posture Option from the SAME per-bind executor, so reviewer never
// imports the sandbox module. reviewer's static mode is ReadOnly, so posture never
// auto-approves Bash (it stays human-gated); Bash still runs under read-only OS
// confinement (defense in depth).
//
// skill is the OPTIONAL per-agent Skill DEFINITION (nil otherwise); when non-nil it is
// appended to the roster and "Skill" is added to the hard-approve set.
//
// It returns a typed *ToolSetError (never a partial Tools) when the static read
// guard's fail-secure PermissionChecker cannot be constructed — e.g. $HOME is
// unresolvable while a home-relative deny pattern is configured.
func BuildTools(confFactory confine.Factory, skill tool.Definition) (Tools, error) {
	guard, err := tools.NewPermissionChecker(tools.PermissionPolicy{HardDeny: tools.DefaultHardDeny()})
	if err != nil {
		return Tools{}, &ToolSetError{Cause: err}
	}

	approved := append([]string(nil), autoApprovedTools...)
	defs := []tool.Definition{
		readFileDefinition(guard),
		globDefinition(guard),
		grepDefinition(guard, confFactory),
		bashDefinition(confFactory),
		todoDefinition(),
		askUserDefinition(),
	}
	if skill != nil {
		defs = append(defs, skill)
		approved = append(approved, toolSkill)
	}

	return Tools{
		Definitions:    defs,
		Permission:     newPermissionFactory(confFactory, approved),
		PolicyRevision: policyRevision(string(Name), approved, defs),
	}, nil
}

// readFileDefinition builds a read-only ReadFile per bind. The harness exposes no
// read-only file definition — its ReadFile needs an unexported observation set only
// tools.Files can wire — so this wraps tools.Files and returns ONLY the ReadFile
// instance, dropping the built WriteFile/EditFile (never registered → reviewer is
// structurally read-only). Building the two unused mutators is cheap and side-effect-
// free. (A read-only files definition in harness is a legitimate follow-up; it is NOT
// in scope here.)
func readFileDefinition(guard loop.ReadGuard) tool.Definition {
	return tool.NewBundleDefinition(toolReadFile, []string{toolReadFile}, tool.RequiresWorkspace, func(ctx context.Context, b tool.Bindings) ([]tool.InvokableTool, error) {
		built, err := tools.Files(guard).Build(ctx, b)
		if err != nil {
			return nil, err
		}
		for _, tl := range built {
			info, err := tl.Info(ctx)
			if err != nil {
				return nil, err
			}
			if info.Name == toolReadFile {
				return []tool.InvokableTool{tl}, nil
			}
		}
		return nil, &ReadFileUnavailableError{}
	})
}

// globDefinition builds a workspace-bound Glob per bind.
func globDefinition(guard loop.ReadGuard) tool.Definition {
	return tool.NewDefinition(toolGlob, tool.RequiresWorkspace, func(_ context.Context, b tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{tools.NewGlob(b.Workspace.Root, guard)}, nil
	})
}

// grepDefinition builds a workspace-bound Grep per bind, routing ripgrep through the
// per-bind sandbox read-only view.
func grepDefinition(guard loop.ReadGuard, confFactory confine.Factory) tool.Definition {
	return tool.NewDefinition(toolGrep, tool.RequiresWorkspace, func(_ context.Context, b tool.Bindings) ([]tool.InvokableTool, error) {
		conf, err := confFactory.For(b)
		if err != nil {
			return nil, err
		}
		return []tool.InvokableTool{tools.NewGrep(b.Workspace.Root, guard, conf.GrepOptions()...)}, nil
	})
}

// bashDefinition builds a workspace-bound Bash per bind: the confined read-only runner,
// the loop's coordinator, and the shared observation set all come from the binding.
func bashDefinition(confFactory confine.Factory) tool.Definition {
	return tool.NewDefinition(toolBash, tool.RequiresWorkspace, func(_ context.Context, b tool.Bindings) ([]tool.InvokableTool, error) {
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

// todoDefinition builds a self-contained Todo per bind (filesystem-free).
func todoDefinition() tool.Definition {
	return tool.NewDefinition(toolTodo, 0, func(_ context.Context, _ tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{tools.NewTodo()}, nil
	})
}

// askUserDefinition builds a self-contained AskUser per bind (filesystem-free).
func askUserDefinition() tool.Definition {
	return tool.NewDefinition("AskUser", 0, func(_ context.Context, _ tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{tools.NewAskUser()}, nil
	})
}

// newPermissionFactory returns the leaf's fresh-per-bind PermissionFactory: a NEW
// fail-secure *tools.PermissionChecker per bind from the immutable hard-deny/
// hard-approve policy, the bound root, and the per-bind ceiling-posture Option from
// confFactory.For. A missing workspace fails closed; a checker-build failure threads
// up as a typed *ToolSetError.
func newPermissionFactory(confFactory confine.Factory, approved []string) loop.PermissionFactory {
	hardApprove := append([]string(nil), approved...)
	return func(_ context.Context, b tool.Bindings) (loop.PermissionGate, error) {
		if b.Workspace == nil {
			return nil, &MissingWorkspaceError{}
		}
		conf, err := confFactory.For(b)
		if err != nil {
			return nil, err
		}
		policy := tools.PermissionPolicy{
			WorkspaceRoot: b.Workspace.Root,
			HardDeny:      tools.DefaultHardDeny(),
			HardApprove:   tools.HardApproveRules{Tools: append([]string(nil), hardApprove...)},
		}
		pc, err := tools.NewPermissionChecker(policy, conf.CheckerOptions()...)
		if err != nil {
			return nil, &ToolSetError{Cause: err}
		}
		return pc, nil
	}
}

// policyRevision derives a stable, secret-free digest of reviewer's IMMUTABLE policy
// (agent name + sorted hard-approve set + sorted produced tool names + hard-deny
// pattern set + read cap). It changes iff the policy changes and is identical across
// binds of the same definition.
func policyRevision(agent string, approved []string, defs []tool.Definition) string {
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
			b.WriteByte('\x1f')
		}
		b.WriteByte('\x1e')
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
