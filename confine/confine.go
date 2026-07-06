// Package confine carries the per-spawn OS-sandbox wiring the SWE-Swarm's
// composition root (swarms/swe) injects into each tool-building leaf. It is the
// SEAM that lets the agent leaf packages (agents/operator, agents/reviewer) wire a
// confined Bash runner, a read-only Grep runner, and a ceiling-posture permission
// gate WITHOUT importing the sandbox module: swarms/swe — the ONE swe package that
// couples harness + sandbox (SPEC §2) — builds a Confinement (carrying only
// harness-typed runners + the checker Option) and hands it down. The leaves depend
// only on this harness-typed seam, so agents/* never import sandbox and there is no
// import cycle back onto swarms/swe.
package confine

import (
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/tools"
)

// Confinement is the OS-sandbox wiring for ONE tool-building leaf, built per spawn
// by the composition root and applied inside the leaf's BuildTools. Its three fields
// are derived from the SAME sandbox Executor instance so posture selection and OS
// enforcement can never disagree (SPEC §10.2).
//
// The ZERO value is the FAIL-SECURE fallback: every field nil. Applied that way,
// Bash/Grep run UNCONFINED (direct exec) and no posture is registered, reproducing
// the pre-sandbox gate behavior (every mutating call Asks). The composition root's
// nil-executor fallback instead sets CheckerOption (with a NIL runner) while leaving
// the runners nil, so the guarantee interlock fails closed and Bash stays Ask — never
// a raw auto-approve on an unenforcing platform.
type Confinement struct {
	// BashRunner is the confined command runner Bash routes through. nil = direct
	// `sh -c` execution (the fallback / unconfined path).
	BashRunner tool.CommandRunner

	// GrepRunner is the read-only argv runner Grep's ripgrep backend routes through —
	// the Executor's ReadOnlyView (read+exec, no write/net/grant, SPEC §10.1). nil =
	// direct execution.
	GrepRunner tool.ArgvRunner

	// CheckerOption is the ceiling-posture Option wired into the leaf's FRESH
	// PermissionChecker (tools.WithCeilingPostures over the effective-mode source and
	// the SAME executor as BashRunner). It is present even on the nil-executor fallback
	// (carrying a nil runner) so the interlock governs Bash auto-approve and fails
	// closed. nil only for the zero Confinement (no posture configured).
	CheckerOption tools.Option
}

// BashOptions returns the Bash construction options: WithRunner when a confined
// runner is present, else none (direct execution).
func (c Confinement) BashOptions() []tools.BashOption {
	if c.BashRunner == nil {
		return nil
	}
	return []tools.BashOption{tools.WithRunner(c.BashRunner)}
}

// GrepOptions returns the Grep construction options: WithArgvRunner (the read-only
// view) when present, else none.
func (c Confinement) GrepOptions() []tools.GrepOption {
	if c.GrepRunner == nil {
		return nil
	}
	return []tools.GrepOption{tools.WithArgvRunner(c.GrepRunner)}
}

// CheckerOptions returns the PermissionChecker options: the ceiling-posture Option
// when configured, else none (no posture — the pre-sandbox gate behavior).
func (c Confinement) CheckerOptions() []tools.Option {
	if c.CheckerOption == nil {
		return nil
	}
	return []tools.Option{c.CheckerOption}
}
