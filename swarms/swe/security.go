package swe

import (
	"strings"

	"github.com/looprig/harness/pkg/tools"
	"github.com/looprig/sandbox"
)

// security.go is the ordinal↔mode↔posture layer — the ONE place swe (the only
// module that imports BOTH harness and sandbox, SPEC §2) translates between the
// two vocabularies:
//
//   - harness knows an ORDINAL scale only: the session ceiling is a uint8 (0 = most
//     restrictive), read per-Check as a tools.CeilingSource, and each ordinal maps to
//     a tools.Posture the consumer registers (SPEC §8/§10.2). harness never sees a
//     mode name.
//   - sandbox knows a MODE ladder: sandbox.Mode (ZeroTrust=0 … Unconfined=4), same
//     order, and enforces the OS policy plus reports the guarantees it achieves.
//
// The two ladders are the SAME ladder. This file pins that: postureTable() maps each
// ordinal to the posture for the mode at that ordinal, and ceilingModeSource adapts
// the ONE ceiling source so it drives both the harness checker (as tools.CeilingSource,
// uint8) and the sandbox dynamic executor (as sandbox.ModeSource, sandbox.Mode). One
// source → posture and enforcement can never disagree (§10.2).
//
// NOTE: this task builds the types/table/adapter only. Wiring the executor and this
// table into the tool-build sites (BuildTools / WithCeilingPostures / NewExecutorDynamic)
// is Task 22.

// ceilingModeSource adapts the harness ceiling ORDINAL source (tools.CeilingSource,
// uint8) into the sandbox.ModeSource (sandbox.Mode) the dynamic executor reads. The
// ordinal maps 1:1 onto sandbox.Mode along the same ladder (0 = most restrictive), so
// the adapter is a pure widen-and-retype.
//
// The whole point is that ONE ceiling State is shared: *ceiling.State satisfies
// tools.CeilingSource structurally, the checker reads it directly, and the sandbox
// executor reads the very same State through this adapter — so a journaled ceiling
// change moves the checker's posture AND the executor's enforced mode together, never
// letting the gate and the OS boundary drift apart (SPEC §10.2).
type ceilingModeSource struct {
	// src is the shared read side of the session ceiling — the SAME source the
	// harness PermissionChecker reads per Check. It must not be nil in production
	// wiring; a nil src is a programming error (Current would panic).
	src tools.CeilingSource
}

// Current returns the live ceiling as a sandbox.Mode. It is read per command by the
// sandbox dynamic executor, mirroring the checker's per-Check read, so a ceiling
// downgrade takes effect on the very next command.
//
// FAIL-CLOSED: a nil src resolves to sandbox.Mode(0) (ZeroTrust, the most
// restrictive mode) rather than panicking on the nil deref — mirroring the harness
// ceilingPostures side, which clamps a nil/out-of-range source to table[0] (the most
// restrictive posture). Consistent with the module's fail-secure ethos.
//
// CONCURRENCY: src.Current() is a lock-free atomic load (ceiling.State) — cheap and
// safe to call from the executor's compile path.
func (m ceilingModeSource) Current() sandbox.Mode {
	if m.src == nil {
		return sandbox.Mode(0) // fail closed to ZeroTrust; never nil-deref.
	}
	return sandbox.Mode(m.src.Current())
}

// Guarantee interlock masks (SPEC §10.3). A Bash auto-approve fires only when the
// held runner's GuaranteeBits() satisfy the posture's RequiredGuarantees
// (runnerBits & required == required). These masks are the LOAD-BEARING safety gate:
// they name the SPECIFIC OS guarantees a mode's Bash auto-approve depends on, so an
// unenforcing runner (nil, a null backend, or a degraded platform) fails the interlock
// and Bash falls back to Ask.
const (
	// writeBashGuarantees is what write-mode Bash auto-approve requires (SPEC §10.3):
	// writes are confined to the workspace (WriteBoundary), the environment is
	// scrubbed to the baseline allowlist so secrets are not exported to subprocesses
	// (EnvScrub), and secret/host deny-reads are enforced (ReadDenies). Network stays
	// gated in write mode, so NetworkBoundary is NOT required here.
	writeBashGuarantees = sandbox.GuaranteeWriteBoundary | sandbox.GuaranteeEnvScrub | sandbox.GuaranteeReadDenies

	// trustedBashGuarantees is what trusted-mode "all bash auto" requires: everything
	// write requires PLUS a real network boundary (SPEC §10.3 "trusted adds
	// NetworkBoundary"). trusted permits full Bash auto-approve, so egress must be
	// bounded (default-deny with the trusted allowlist) before that is safe.
	// AddressNetwork (metadata/local-net scoping) is deliberately NOT required: it is
	// unavailable on macOS Seatbelt and Linux rung 2 (SPEC §7.1/§13.6), so requiring it
	// would make trusted behave as write-with-asks on every supported platform today.
	trustedBashGuarantees = writeBashGuarantees | sandbox.GuaranteeNetworkBoundary

	// editsGuarantees is what an auto-approved file EDIT requires (SPEC §10.3): the OS
	// write-boundary must ACTUALLY confine writes to the workspace. Edits, like Bash,
	// are now interlock-gated — in-process write-containment alone is not an OS
	// boundary, so on a non-enforcing backend (nil / null-backend runner) the edit
	// interlock fails and edits fall back to Ask. This is a NARROWER mask than
	// writeBashGuarantees: an in-process editor never spawns a subprocess, so it needs
	// neither EnvScrub (no child env to scrub) nor NetworkBoundary — only that a write
	// escaping the workspace is actually blocked by the OS.
	editsGuarantees = sandbox.GuaranteeWriteBoundary
)

// postureTable returns the ordinal→Posture table (SPEC §4/§10.3), indexed by the
// ceiling ordinal, which equals the sandbox.Mode value at that rung. It is registered
// with the checker via tools.WithCeilingPostures so a Check selects table[ordinal]
// (harness clamps an out-of-range ordinal to table[0], the most restrictive — §10.2).
//
// Each row encodes one column of the SPEC §4 mode matrix:
//
//	ordinal 0 zerotrust  : file-edit ask,   bash ask
//	ordinal 1 readonly   : file-edit ask,   bash ask
//	ordinal 2 write      : file-edit auto,  bash trivial-auto / rest-ask
//	ordinal 3 trusted    : file-edit auto,  bash all-auto
//	ordinal 4 unconfined : file-edit auto,  bash all-auto (no interlock)
//
// EDITS ARE INTERLOCK-GATED TOO (SPEC §10.3): the "file-edit auto" rows set
// RequiredGuaranteesEdits so an edit auto-approves ONLY when the held runner actually
// enforces the OS write-boundary — exactly as Bash auto-approve requires OS
// enforcement. Write and trusted require GuaranteeWriteBoundary (editsGuarantees);
// unconfined steps off the ladder with a ZERO mask (no interlock). On a null /
// non-enforcing backend the edit interlock fails and edits fall back to Ask, so the
// "file-edit auto" rows degrade fail-secure just like the Bash column.
//
// GrantCarryingAlwaysAsk is set on EVERY row (including the ask-only rows, where it is
// harmless): an escalation grant-carrying call is never auto-approved by posture in
// any mode — it must be human-reviewed (SPEC §9.3/§10.7).
func postureTable() []tools.Posture {
	return []tools.Posture{
		// [0] zerotrust — the fail-closed floor: writes denied, network hard-denied,
		// reads restricted. Nothing auto-approves; every file-edit and Bash call asks
		// (SPEC §4 zerotrust column). RequiredGuarantees is moot (nothing auto-fires).
		sandbox.ZeroTrust: {
			AutoApproveEdits:       false,
			AutoApproveBash:        false,
			GrantCarryingAlwaysAsk: true,
		},
		// [1] readonly — broad reads but writes gated; still no auto-approve (same gate
		// posture as zerotrust: file-edit ask, bash ask — SPEC §4 readonly column). The
		// difference between zerotrust and readonly is OS read scope, not gate posture.
		sandbox.ReadOnly: {
			AutoApproveEdits:       false,
			AutoApproveBash:        false,
			GrantCarryingAlwaysAsk: true,
		},
		// [2] write — file edits auto-approve, but ONLY when the runner enforces the OS
		// write-boundary (editsGuarantees): in-process write-containment + the ReadGuard
		// confine edits, yet that is not an OS boundary, so the edit interlock requires
		// GuaranteeWriteBoundary and edits Ask on a null/non-enforcing backend. Bash is
		// "trivial auto, rest ask" via trivialBash, and its interlock requires the write
		// bash guarantees (SPEC §4 write column, §10.3 write masks).
		sandbox.Write: {
			AutoApproveEdits:        true,
			AutoApproveBash:         false,
			TrivialBash:             trivialBash,
			RequiredGuarantees:      writeBashGuarantees,
			RequiredGuaranteesEdits: editsGuarantees,
			GrantCarryingAlwaysAsk:  true,
		},
		// [3] trusted — file edits auto (OS write-boundary gated, same editsGuarantees as
		// write), ALL Bash auto but only when the runner enforces the trusted bash mask
		// (write guarantees + NetworkBoundary). Without an enforcing runner both interlocks
		// fail: Bash degrades to write-with-asks AND edits degrade to Ask (SPEC §4 trusted
		// column, §10.3).
		sandbox.Trusted: {
			AutoApproveEdits:        true,
			AutoApproveBash:         true,
			RequiredGuarantees:      trustedBashGuarantees,
			RequiredGuaranteesEdits: editsGuarantees,
			GrantCarryingAlwaysAsk:  true,
		},
		// [4] unconfined — steps OFF the ladder (SPEC §4 note): no OS wrapper, so there
		// is nothing to interlock against. Everything auto-approves with EMPTY
		// RequiredGuarantees AND RequiredGuaranteesEdits masks — the consumer's explicit
		// "no interlock" choice (§10.3). The scare-surface for choosing unconfined lives
		// at config/CLI, not here.
		sandbox.Unconfined: {
			AutoApproveEdits:        true,
			AutoApproveBash:         true,
			RequiredGuarantees:      0,
			RequiredGuaranteesEdits: 0,
			GrantCarryingAlwaysAsk:  true,
		},
	}
}

// trivialBashPrefixes is the conservative interim allowlist backing the write-mode
// "trivial auto, rest ask" slot (SPEC §4 write row; Phase-0 decision §13.2 —
// "extend the existing HardApproveRules prefix rules"). Every entry is a trivial
// command whose side effects are CONTAINED by the write-mode boundary — NOT
// unconditionally side-effect-free. `git diff` is the sharp case: `--output=FILE`
// writes and `diff.external`/`GIT_EXTERNAL_DIFF` can exec, but a `--output` outside
// the workspace is blocked by write-mode `WriteBoundary`, `.git/config` is a
// deny-read + carveout, and `EnvScrub` strips `GIT_EXTERNAL_DIFF` — so every path
// stays contained. The list is deliberately SMALL: a command that is not provably
// trivial falls to Ask, which is fail-safe.
//
// TODO(Task 22): converge on the shared HardApproveRules prefix classifier so these
// trivial-command rules live in one place.
var trivialBashPrefixes = []string{
	"ls",
	"cat",
	"pwd",
	"echo",
	"git status",
	"git diff",
}

// trivialBash is the write-mode Posture.TrivialBash classifier. It auto-approves ONLY
// commands it can prove are trivial read-only invocations and fails safe (returns
// false → Ask) for everything else.
//
// It is STRICTER than the harness advisory denied-prefix matcher: that matcher
// over-DENIES (fail-secure), but an AUTO-APPROVE classifier must never over-APPROVE
// (that would be fail-OPEN). So it (a) rejects any command carrying a shell
// metacharacter that could chain, substitute, or redirect a non-trivial command — so
// "cat x && rm -rf /" never auto-approves — and (b) matches the allowlist on WORD
// boundaries, so "cat" matches "cat file" but never "catalog".
//
// CONCURRENCY: this is the Posture.TrivialBash slot — invoked under the checker's held
// mutex during Check. It is pure and cheap and must never call back into Check.
//
// TODO(task-22+): replace this interim allowlist with the shared HardApproveRules
// prefix classifier (Phase-0 decision §13.2) so trivial-command rules live in one place.
func trivialBash(command string) bool {
	// Reject on the RAW command first: newlines are shell command separators and must
	// be caught before whitespace normalization (strings.Fields) would fold them away.
	// The metacharacter set covers chaining (; | &), redirection (< >), and command
	// substitution (backtick, and "$(" checked below).
	if strings.ContainsAny(command, ";|&<>`\n\r") || strings.Contains(command, "$(") {
		return false
	}
	nc := normalizeBashCommand(command)
	if nc == "" {
		return false
	}
	for _, p := range trivialBashPrefixes {
		if nc == p || strings.HasPrefix(nc, p+" ") {
			return true
		}
	}
	return false
}

// normalizeBashCommand trims and collapses internal runs of whitespace to single
// spaces, mirroring the harness denied-prefix normalization so "git   status"
// classifies identically to "git status".
func normalizeBashCommand(command string) string {
	return strings.Join(strings.Fields(command), " ")
}
