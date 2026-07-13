package swe

import (
	"log/slog"
	"sync"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/tools"
	"github.com/looprig/sandbox"
	"github.com/looprig/swe/confine"
)

// confinement.go is the integration layer that turns the ordinal↔mode↔posture
// primitives from security.go (postureTable, ceilingModeSource) into the per-bound-loop
// OS-sandbox wiring each tool-building leaf applies. It computes the EFFECTIVE mode
// (min(role static mode, session ceiling)) live, builds ONE dynamic sandbox Executor per
// bound loop, and hands the leaf a confine.Confinement wiring the SAME executor into Bash's
// runner, Grep's read-only view, and the checker's posture Option — so posture and OS
// enforcement always agree (SPEC §8/§10.2).
//
// Post-rig-migration the confinement is minted PER BOUND LOOP via confineFactory (a
// confine.Factory): the immutable loop.Definition carries the factory, and the leaf's
// per-bind tool/permission factories call For(bindings) once per loop binding. The factory
// memoizes the Confinement per bindings.LoopID so a single loop's Bash/Grep/checker share
// the SAME executor within a process, while distinct loops get fresh executors.

// Per-role static security modes (SPEC §8): the fixed mode a role may reach BEFORE
// the session ceiling clamps it. The effective mode of every leaf is min(role static
// mode, session ceiling) — a role can never exceed the ceiling, and never exceeds its
// own static mode. operator (implement) writes; reviewer (critique) is read-only. The
// operator primer carries the operator role mode.
const (
	operatorRoleMode uint8 = uint8(sandbox.Write)    // 2: workspace write/edit, gated bash/net
	reviewerRoleMode uint8 = uint8(sandbox.ReadOnly) // 1: broad read, no write, everything gated
)

// effectiveModeSource yields min(role static mode, session ceiling) as a ceiling.Level,
// read PER Check/spawn — so tightening the ceiling clamps every leaf immediately
// (SPEC §8) and a role can never exceed the ceiling. It satisfies tools.CeilingSource
// (the harness checker reads it directly via WithCeilingPostures) and ceiling.Source (the
// exact live session-scoped ordinal source on a loop binding), and, wrapped in
// ceilingModeSource, drives the sandbox dynamic executor — ONE source feeding BOTH posture
// selection AND OS enforcement.
type effectiveModeSource struct {
	role    uint8          // the leaf's static per-role mode (never exceeded)
	ceiling ceiling.Source // the shared session ceiling, or the parent's effective source (child clamp)
}

// Current returns min(role, ceiling) as a ceiling.Level, fail-closing to 0 (ZeroTrust, the
// most restrictive ordinal) on a nil ceiling — mirroring the harness ceilingPostures
// nil-source clamp to table[0].
func (e effectiveModeSource) Current() ceiling.Level {
	c := ceilingCurrent(e.ceiling)
	if e.role < c {
		return ceiling.Level(e.role)
	}
	return ceiling.Level(c)
}

// ceilingCurrent reads a ceiling source, fail-closing to 0 on a nil source.
func ceilingCurrent(c ceiling.Source) uint8 {
	if c == nil {
		return 0
	}
	return uint8(c.Current())
}

// confineFactory is the swarms/swe implementation of confine.Factory (SPEC §2 — the ONE
// swe package that couples harness + sandbox). It mints one dynamic sandbox Executor per
// bound loop (keyed by bindings.LoopID), so a single loop's Bash runner, Grep read-only
// view, and permission ceiling-posture Option share the SAME executor while distinct binds
// get fresh executors. role is the leaf's static per-role mode; the live per-bind ceiling
// comes from bindings.Ceiling, so the effective mode is min(role, bindings.Ceiling).
//
// MEMO SAFETY: within one process each LoopID's tools are bound once and the executor is a
// pure function of root + ceiling (stable per LoopID within a process); restore is a fresh
// process → fresh rig → fresh memo. The memo is concurrency-safe (mutex) and bounded by the
// session topology (configured primer + spawn quota) so a runaway bind loop cannot grow it.
type confineFactory struct {
	role uint8
	mu   sync.Mutex
	memo map[uuid.UUID]confine.Confinement
}

// confinementMemoLimit is topology-derived: one configured primer plus every delegate the
// rig's session quota permits. Operator and reviewer factories remain role-separated, so this
// bound does not alter either role's effective-mode semantics.
const confinementMemoLimit = operatorSpawnQuota + 1

// newConfineFactory builds a confine.Factory clamped to role's static mode. The caller wires
// ONE factory per role into that role's leaf/primer loop.Definition.
func newConfineFactory(role uint8) *confineFactory {
	return &confineFactory{role: role, memo: make(map[uuid.UUID]confine.Confinement)}
}

// For returns the OS-sandbox Confinement for one bound loop, memoized per bindings.LoopID.
// The root is read from the binding (never a fixed root); the effective mode is
// min(role, bindings.Ceiling), read live by the dynamic executor. It never errors: an
// unenforceable backend is a graceful degrade (nil runners + a nil-runner posture Option),
// not a failure.
func (f *confineFactory) For(b tool.Bindings) (confine.Confinement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.memo[b.LoopID]; ok {
		return c, nil
	}
	var root string
	if b.Workspace != nil {
		root = b.Workspace.Root
	}
	conf := confinementFor(root, effectiveModeSource{role: f.role, ceiling: b.Ceiling})
	if len(f.memo) < confinementMemoLimit {
		f.memo[b.LoopID] = conf
	}
	return conf, nil
}

// compile-time assertion: confineFactory satisfies the leaf's per-bind seam.
var _ confine.Factory = (*confineFactory)(nil)

// confinementFor builds the per-bound-loop OS-sandbox wiring from an effective-mode source.
// It mints ONE dynamic sandbox Executor whose mode is read live from effSrc (min(role,
// ceiling)) and wires the SAME instance into Bash's confined runner, Grep's read-only argv
// runner (the Executor's ReadOnlyView — SPEC §10.1), and the checker's ceiling-posture
// Option (WithCeilingPostures over effSrc + postureTable() + the executor), so posture
// selection and OS enforcement always agree (SPEC §10.2).
//
// GRACEFUL FALLBACK (Ubuntu-safe, SPEC §6/§13.6): if the executor cannot be built (e.g. an
// unsupported platform, or an unresolvable-home secret-deny guard) it returns a Confinement
// with NIL runners but a posture Option carrying a NIL runner — so Bash/Grep run
// UNCONFINED-but-GATED (direct exec). With a nil runner the guarantee interlock fails closed
// for BOTH the Bash mask AND the edit mask, so BOTH Bash AND file-edit auto-approve are OFF
// on the fallback path. It NEVER crashes and NEVER auto-approves Bash OR edits on fallback.
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

// DefaultSecurityMode is the CLI's default session ceiling: Write — file edits and trivial
// Bash auto-approve ONLY under real OS enforcement, with network + non-trivial Bash gated
// (SPEC §4 write column). Both edit and Bash auto-approve are interlock-gated, so a backend
// that enforces nothing falls back to Ask — Write is FAIL-SECURE on every backend.
const DefaultSecurityMode = uint8(sandbox.Write)

// securityModeNames maps the CLI-selectable session-ceiling names to their ceiling ordinals
// (== sandbox.Mode). Unconfined is deliberately ABSENT: it steps off the sandbox ladder
// (full user authority) and is not selectable from the CLI (SPEC §4 scare-surface).
var securityModeNames = map[string]uint8{
	"zerotrust": uint8(sandbox.ZeroTrust),
	"readonly":  uint8(sandbox.ReadOnly),
	"write":     uint8(sandbox.Write),
	"trusted":   uint8(sandbox.Trusted),
}

// ParseSecurityMode maps a CLI security-mode NAME to its session-ceiling ordinal, reporting
// ok=false for an unknown name so the caller can fail closed at the flag boundary.
func ParseSecurityMode(name string) (uint8, bool) {
	ord, ok := securityModeNames[name]
	return ord, ok
}
