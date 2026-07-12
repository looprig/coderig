// Package reviewer is the SWE-Swarm's critique leaf agent. It exposes its
// boundary as pure data (Name, Description, Role) and a raw-signature BuildTools
// so the swarm composition root can adapt it into a swe.Agent WITHOUT this
// package importing swarms/swe (which would be an import cycle). It is a leaf: it
// cannot spawn and it never mutates the filesystem — it reads, may run tests/
// build via Bash to verify claims, and reports findings. It does not fix.
package reviewer

import (
	"context"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/tools"
	"github.com/looprig/swe/agents/internal/leafrig"
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

// Tools, ToolSetError, and MissingWorkspaceError are the shared leafrig types (aliased
// so reviewer's public surface and its callers' errors.As targets are unchanged after
// the mechanism was extracted to agents/internal/leafrig). The Agent field carries
// "reviewer" so the error messages keep the leaf prefix.
type (
	Tools                 = leafrig.Tools
	ToolSetError          = leafrig.ToolSetError
	MissingWorkspaceError = leafrig.MissingWorkspaceError
)

// autoApprovedTools is reviewer's hard-approve set: everything EXCEPT Bash. Bash
// runs a shell, so it stays Ask — a human reads and approves each command before
// it runs (the permission gate is the security boundary). The read/todo/ask
// tools are side-effect-free and run without prompting. Names match each tool's
// Info().Name exactly.
var autoApprovedTools = []string{"ReadFile", "Glob", "Grep", "Todo", "AskUser"}

// ReadFileUnavailableError reports the impossible case that the wrapped tools.Files
// bundle did not produce a ReadFile tool. It exists so the read-only ReadFile
// definition fails LOUDLY (never a nil tool) if the harness Files contract ever
// changes out from under the wrapper.
type ReadFileUnavailableError struct{}

func (*ReadFileUnavailableError) Error() string {
	return "reviewer: tools.Files produced no ReadFile to expose read-only"
}

// readFileDefinition builds a read-only ReadFile per bind. The harness exposes no
// read-only file definition — its ReadFile needs an unexported observation set only
// tools.Files can wire — so this wraps tools.Files and returns ONLY the ReadFile
// instance, dropping the built WriteFile/EditFile (never registered → reviewer is
// structurally read-only). Building the two unused mutators is cheap and side-effect-
// free. This is the ONE reviewer-specific definition (the operator uses tools.Files
// directly for its write/edit); the rest of reviewer's roster comes from leafrig. (A
// read-only files definition in harness is a legitimate follow-up; NOT in scope here.)
func readFileDefinition(guard loop.ReadGuard) tool.Definition {
	const readFileToolName = "ReadFile"
	return tool.NewBundleDefinition(readFileToolName, []string{readFileToolName}, tool.RequiresWorkspace, func(ctx context.Context, b tool.Bindings) ([]tool.InvokableTool, error) {
		built, err := tools.Files(guard).Build(ctx, b)
		if err != nil {
			return nil, err
		}
		for _, tl := range built {
			info, err := tl.Info(ctx)
			if err != nil {
				return nil, err
			}
			if info.Name == readFileToolName {
				return []tool.InvokableTool{tl}, nil
			}
		}
		return nil, &ReadFileUnavailableError{}
	})
}

// BuildTools returns reviewer's per-binding tool Definitions (ReadFile, Glob, Grep,
// Bash, Todo, AskUser — critique with the ability to run tests/build via Bash), a
// fresh-per-bind PermissionFactory, and a stable PolicyRevision (all three from the
// shared leafrig mechanism, except the reviewer-only read-only ReadFile wrap). There is
// deliberately NO write/edit tool (reviewer critiques, it never mutates) and NO
// Subagent (a leaf cannot spawn).
//
// EVERY factory reads tool.Bindings.Workspace.Root and constructs fresh collaborators
// at bind time. The read tools share ONE static leafrig ReadGuard (immutable,
// root-independent hard-deny read policy); the permission GATE is a FRESH
// *tools.PermissionChecker per bind. confFactory is the per-bind OS-sandbox seam (SPEC
// §10.2): Grep, Bash, and the permission factory call confFactory.For(bindings) for the
// confined read-only view / runner / ceiling-posture Option from the SAME per-bind
// executor, so reviewer never imports the sandbox module. reviewer's static mode is
// ReadOnly, so posture never auto-approves Bash (it stays human-gated); Bash still runs
// under read-only OS confinement (defense in depth).
//
// skill is the OPTIONAL per-agent Skill DEFINITION (nil otherwise); when non-nil it is
// appended to the roster and "Skill" is added to the hard-approve set.
//
// It returns a typed *ToolSetError (never a partial Tools) when the static read
// guard's fail-secure PermissionChecker cannot be constructed — e.g. $HOME is
// unresolvable while a home-relative deny pattern is configured.
func BuildTools(confFactory confine.Factory, skill tool.Definition) (Tools, error) {
	guard, err := leafrig.NewReadGuard()
	if err != nil {
		return Tools{}, &ToolSetError{Agent: string(Name), Cause: err}
	}

	approved := append([]string(nil), autoApprovedTools...)
	defs := []tool.Definition{
		readFileDefinition(guard),
		leafrig.GlobDefinition(guard),
		leafrig.GrepDefinition(guard, confFactory),
		leafrig.BashDefinition(confFactory),
		leafrig.TodoDefinition(),
		leafrig.AskUserDefinition(),
	}
	if skill != nil {
		defs = append(defs, skill)
		approved = append(approved, leafrig.SkillToolName)
	}

	return Tools{
		Definitions:    defs,
		Permission:     leafrig.NewPermissionFactory(string(Name), confFactory, approved),
		PolicyRevision: leafrig.PolicyRevision(string(Name), approved, defs),
	}, nil
}
