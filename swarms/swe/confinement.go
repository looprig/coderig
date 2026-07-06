package swe

import (
	"log/slog"

	"github.com/looprig/harness/pkg/tools"
	"github.com/looprig/sandbox"
	"github.com/looprig/swe/confine"
)

// confinement.go is the Task-22 integration layer: it turns the ordinal↔mode↔posture
// primitives from security.go (postureTable, ceilingModeSource — Task 21) into the
// per-spawn OS-sandbox wiring each tool-building leaf applies. It computes the
// EFFECTIVE mode (min(role static mode, session ceiling)) live, builds ONE dynamic
// sandbox Executor per build site, and hands the leaf a confine.Confinement wiring
// the SAME executor into Bash's runner, Grep's read-only view, and the checker's
// posture Option — so posture and OS enforcement always agree (SPEC §8/§10.2).

// Per-role static security modes (SPEC §8): the fixed mode a role may reach BEFORE
// the session ceiling clamps it. The effective mode of every leaf is min(role static
// mode, session ceiling) — a role can never exceed the ceiling, and never exceeds its
// own static mode. operator (implement) writes; reviewer (critique) is read-only. The
// PRIMARY operator carries the operator role mode.
const (
	operatorRoleMode uint8 = uint8(sandbox.Write)    // 2: workspace write/edit, gated bash/net
	reviewerRoleMode uint8 = uint8(sandbox.ReadOnly) // 1: broad read, no write, everything gated
)

// effectiveModeSource yields min(role static mode, session ceiling) as an ordinal,
// read PER Check/spawn — so tightening the ceiling clamps every leaf immediately
// (SPEC §8) and a role can never exceed the ceiling. It satisfies tools.CeilingSource
// (the harness checker reads it directly via WithCeilingPostures) and, wrapped in
// Task-21's ceilingModeSource, drives the sandbox dynamic executor — ONE source
// feeding BOTH posture selection AND OS enforcement.
//
// For a SPAWNED child the ceiling field is the PARENT's effective-mode source (not
// the raw session ceiling), so the child clamps to min(childRole, parentEffective) —
// a child can never exceed its parent (non-escalation; elevation only via static
// config, never a runtime spawn).
type effectiveModeSource struct {
	role    uint8               // the leaf's static per-role mode (never exceeded)
	ceiling tools.CeilingSource // the shared session ceiling, or the parent's effective source (child clamp)
}

// Current returns min(role, ceiling), fail-closing to 0 (ZeroTrust, the most
// restrictive ordinal) on a nil ceiling — mirroring the harness ceilingPostures
// nil-source clamp to table[0].
func (e effectiveModeSource) Current() uint8 {
	c := ceilingCurrent(e.ceiling)
	if e.role < c {
		return e.role
	}
	return c
}

// ceilingCurrent reads a ceiling source, fail-closing to 0 on a nil source.
func ceilingCurrent(c tools.CeilingSource) uint8 {
	if c == nil {
		return 0
	}
	return c.Current()
}

// confinementFor builds the per-spawn OS-sandbox wiring for one tool-building leaf
// from its effective-mode source. It mints ONE dynamic sandbox Executor whose mode
// is read live from effSrc (min(role, ceiling)) and wires the SAME instance into
// Bash's confined runner, Grep's read-only argv runner (the Executor's ReadOnlyView
// — SPEC §10.1), and the checker's ceiling-posture Option (WithCeilingPostures over
// effSrc + postureTable() + the executor), so posture selection and OS enforcement
// always agree (SPEC §10.2).
//
// GRACEFUL FALLBACK (Ubuntu-safe, SPEC §6/§13.6): if the executor cannot be built
// (e.g. an unsupported platform, or an unresolvable-home secret-deny guard) it returns
// a Confinement with NIL runners but a posture Option carrying a NIL runner — so
// Bash/Grep run UNCONFINED-but-GATED (direct exec) and the guarantee interlock fails
// closed → Bash auto-approve is OFF (stays Ask). It NEVER crashes and NEVER
// auto-approves on the fallback path.
func confinementFor(root string, effSrc tools.CeilingSource) confine.Confinement {
	ex, err := sandbox.NewExecutorDynamic(ceilingModeSource{src: effSrc}, root)
	if err != nil {
		// Fail-open on the RUNTIME (Bash runs unconfined) but fail-CLOSED on the GATE
		// (no enforcing runner → interlock fails → Bash stays Ask). Log the degrade.
		slog.Warn("swe: OS sandbox unavailable; Bash/Grep run unconfined-but-gated (Bash stays Ask)", "err", err)
		return confine.Confinement{
			CheckerOption: tools.WithCeilingPostures(effSrc, postureTable(), nil),
		}
	}
	return confine.Confinement{
		BashRunner:    ex,
		GrepRunner:    ex.ReadOnlyView(),
		CheckerOption: tools.WithCeilingPostures(effSrc, postureTable(), ex),
	}
}

// buildConfinement builds the confinement for a leaf of the given static role mode
// under the given ceiling — the shared session ceiling for the primary, or the
// parent's effective-mode source for a spawned child (the non-escalation clamp).
func buildConfinement(root string, role uint8, ceiling tools.CeilingSource) confine.Confinement {
	return confinementFor(root, effectiveModeSource{role: role, ceiling: ceiling})
}

// DefaultSecurityMode is the CLI's default session ceiling: Write — file edits
// auto-approve (confined by workspace write-containment + the ReadGuard), trivial
// Bash auto-approves under OS enforcement, and network + non-trivial Bash stay gated
// (SPEC §4 write column). It is the sensible default for an interactive coding
// session on a platform with a real OS backend; on a backend that enforces nothing
// the interlock keeps Bash at Ask regardless.
const DefaultSecurityMode = uint8(sandbox.Write)

// securityModeNames maps the CLI-selectable session-ceiling names to their ceiling
// ordinals (== sandbox.Mode). Unconfined is deliberately ABSENT: it steps off the
// sandbox ladder (full user authority) and is not selectable from the CLI (SPEC §4
// scare-surface); no role's effective mode reaches it anyway (roles cap at Write).
var securityModeNames = map[string]uint8{
	"zerotrust": uint8(sandbox.ZeroTrust),
	"readonly":  uint8(sandbox.ReadOnly),
	"write":     uint8(sandbox.Write),
	"trusted":   uint8(sandbox.Trusted),
}

// ParseSecurityMode maps a CLI security-mode NAME to its session-ceiling ordinal,
// reporting ok=false for an unknown name so the caller can fail closed at the flag
// boundary (untrusted CLI input validated before any wiring runs).
func ParseSecurityMode(name string) (uint8, bool) {
	ord, ok := securityModeNames[name]
	return ord, ok
}
