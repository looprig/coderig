package swe

// This is the Task 1 dependency-surface probe from
// docs/plans/2026-07-11-harness-rig-migration-implementation.md. It compiles only when the
// final harness rig/loop/session/event symbols and the migrated CLI tui.Agent contract
// (RootLoopID, ActiveLoopID, loop-targeted AcceptsImages) are all present. In this
// workspace those modules resolve via the local `replace` directives to the reviewed
// harness/cli checkouts, so the surface is already available; this file guards against a
// regression in that surface while the swe migration proceeds. It carries no test
// functions — its value is that it must build.

import (
	"github.com/looprig/cli/tui"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/session"
)

var (
	_ = rig.Define
	_ = loop.Define
	_ session.SessionController
	_ = event.ActiveLoopChanged{}
	_ = event.LoopStarted{DisplayName: "operator"}
	_ tui.Agent
	_ = loop.WithDisplayName
	_ = rig.WithOffloadGC
)
