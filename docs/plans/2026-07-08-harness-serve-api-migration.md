# SWE ↔ harness serve/session API migration

**Date:** 2026-07-08
**Target repo:** `github.com/looprig/swe` (code under `swarms/swe/`)
**Trigger:** `github.com/looprig/harness` reshaped its HTTP/session surface. This spec is written against the **current local harness** at `../harness` (verified file-by-file) and is executed **after** the harness change has landed.

---

## Context

Harness reshaped its serve/session surface as part of the gate-directory + flow-observability work. Three of those changes are source-breaking for `swe`: the event fan-in now yields a `Delivery` wrapper instead of a bare `Event`; `SubscribeEvents` now returns the `event.Subscription` *interface*; and the legacy per-loop gate trio (`Approve`/`Deny`/`ProvideUserInput`) has been **deleted** from `session.Session` in favour of a single directory-addressed `Session.RespondGate(ctx, gate.GateResponse)`. `swe`'s `sessionAgent` wrapper and several tests consume all three surfaces, so they must be updated for `swe` to build and test against the new harness.

---

## The three API deltas (verified against `../harness`)

### Delta 1 — `Subscription.Events()` element type

`harness/pkg/event/event.go`:

```go
type Subscription interface {
	Events() <-chan Delivery   // was: <-chan Event
	Close() error
	Err() error
}

// Delivery is one fan-in delivery: the event plus its durable journal sequence.
type Delivery struct {
	Event      Event
	JournalSeq uint64
}
```

Every read/range of `sub.Events()` now yields a `event.Delivery`; the event is `d.Event`. `JournalSeq` is new and `swe` does not need it (leave it unread).

### Delta 2 — `Session.SubscribeEvents` return type

`Session.SubscribeEvents(filter event.EventFilter) (event.Subscription, error)` now returns the **`event.Subscription` interface** (previously `*hub.EventSubscription`).

**swe impact: none beyond Delta 1.** `swarms/swe/agent.go:142` already declares the return type as the interface:

```go
func (a *sessionAgent) Subscribe(filter event.EventFilter) (event.Subscription, error) {
	return a.session.SubscribeEvents(filter)
}
```

`*hub.EventSubscription` satisfied `event.Subscription` structurally, and the concrete return now *is* the interface, so this line compiles unchanged. Only the channel element type (Delta 1) changes at the consumers.

### Delta 3 — gate trio deleted; replaced by `RespondGate`

**Deleted from `session.Session`:** `Approve(ctx, loopID, callID, scope)`, `Deny(ctx, loopID, callID)`, `ProvideUserInput(ctx, loopID, callID, answer)`.

**Replacement** (`harness/pkg/session/gates.go`):

```go
func (s *Session) RespondGate(ctx context.Context, response gate.GateResponse) error
```

`gate.GateResponse` (`harness/pkg/gate/response.go`):

```go
type GateResponse struct {
	GateID ID                         // gate.ID = uuid.UUID — addresses the gate in the session directory
	Action string                     // "approve" | "deny" | "answer"
	Values map[string]json.RawMessage
	Source ResponseSource             // {Kind: gate.ResponseFromUser}
}
```

**Critical semantic shift.** The trio addressed a gate by `(loopID, toolExecutionID/callID)` and delivered a command to a parked runner. `RespondGate` addresses a gate by **`GateID`** and requires the gate to be **open** in the session's gate directory (`entry.state == gateOpen`); otherwise it returns a typed `*session.GateError` (`GateNotFound` / `GateNotReady`). `RespondGate` then validates `Action` against the gate's `Prompt.Controls`, translates `Values` into the internal `ApproveToolCall`/`DenyToolCall`/`ProvideUserInput` command, durably resolves the gate, and dispatches to the loop.

**Action / Values conventions** (verified in `harness/pkg/session/gates.go` `translatePermissionResponse` / `translateAskUserResponse` / `validatePermissionApprove`, and the gate templates in `harness/pkg/loop/gate.go`):

| Old trio call | `Action` | `Values` |
|---|---|---|
| `Approve(…, scope)` | `"approve"` | `{"scope": "<once\|session\|workspace>"}` (+ optional `{"accepted_grants": ["<token>",…]}` for Bash grants) |
| `Deny(…)` | `"deny"` | none |
| `ProvideUserInput(…, answer)` | `"answer"` | `{"answer": "<text>"}` |

- The scope string comes from `tool.ApprovalScopeValue(scope) (string, bool)` (`harness/pkg/tool/permission_request.go:38`), and is decoded back by `tool.ParseApprovalScopeValue` (`"once"`/`"session"`/`"workspace"`).
- Permission gates carry Controls `approve`/`deny` and a required `scope` select field; AskUser gates carry Control `answer` and an `answer` field (`harness/pkg/loop/gate.go` `permissionGate` / `askUserGate`). `RespondGate` rejects an action not present in the gate's Controls with `GateError{Kind: GateActionInvalid}`.
- Provenance: always set `Source: gate.ResponseSource{Kind: gate.ResponseFromUser}` — the trio was inherently user-initiated.

`session.GateError` is a typed error (`errors.As`-able) with `Kind` one of `not_found` / `not_ready` / `kind_mismatch` / `action_invalid` / `capacity` / `append_failed`.

---

## The crux — where the gate.ID comes from

**Finding: the gate.ID is available at the reply site with NO new plumbing.** `swe`'s trio methods keep their `(loopID, callID)` signatures; the gate.ID is resolved internally from the session's live gate directory.

Why this works (verified in harness):

1. The loop registers every interactive gate in the **session gate directory** before emitting the render event. `harness/pkg/loop/runner.go:405-426` (permission) and `harness/pkg/loop/gate.go` `RequestUserInput` (ask-user) both follow **register → ack → emit**: they `PrepareGateOpen` + `ActivateGate` (so `ListGates` returns the gate) and only **then** emit `event.PermissionRequested` / `event.UserInputRequested`. So by the time any observer sees the request event, the gate is already `gateOpen` and listable.
2. Each gate's `Subject.ToolExecutionID` is exactly the `callID` (`harness/pkg/loop/gate.go` `permissionGate`/`askUserGate`: `Subject: gate.Subject{ToolExecutionID: callID}`), and `gate.Gate.ID` is the directory `gate.ID`.
3. `Session.ListGates(ctx) []gate.Gate` returns the public envelopes of all **open** gates, each with `.ID`, `.Kind`, and `.Subject.ToolExecutionID`.

So `swe` resolves the gate.ID by scanning `ListGates` for the open gate whose `Subject.ToolExecutionID == callID` and whose `Kind` matches the operation (`gate.KindPermission` for approve/deny; `gate.KindAskUser` for answer). `ToolExecutionID` is a globally-unique UUID, so `callID` alone disambiguates; `loopID` is retained only for the fail-secure log/error and is not needed for the lookup (`ListGates` does not expose the owning loop).

**Alternative considered (and rejected for the minimal migration):** change the three `sessionAgent` method signatures to take a `gate.ID` directly and have each caller source it from a new `event.GateOpened` observation (`GateOpened.Gate.ID`). This is *cleaner* long-term (no O(n) scan, no reliance on `Subject.ToolExecutionID` uniqueness) but it ripples the signature change into every caller — the tests here, **and any consumer in the `cli` module** — so it is deferred. The `ListGates` approach keeps the `sessionAgent` public surface identical, so no caller outside `agent.go` changes shape.

**Open question (flag, do not guess):** if the `cli` module (or any other consumer) is being migrated in lockstep to the directory/`GateID` model and would prefer `sessionAgent` to expose a `RespondGate(ctx, gate.GateResponse)` passthrough (or gate.ID-typed methods) instead of the `(loopID, callID)` trio, prefer that shape and drop the `ListGates` scan. Decide this **with** the cli migration owner before implementing; this spec assumes the signature-preserving `ListGates` approach absent that decision.

---

## File-by-file change list

### A. `.Events()` unwrap (mechanical — Delta 1)

Each site changes the loop variable from an `event.Event` to an `event.Delivery` and reads `.Event`.

**`swarms/swe/persistence.go:349`** (`watchSessionEvents`):
```go
// BEFORE
for ev := range sub.Events() {
	if _, ok := ev.(event.SessionIdle); !ok {
		continue
	}
	…
}
// AFTER
for d := range sub.Events() {
	ev := d.Event
	if _, ok := ev.(event.SessionIdle); !ok {
		continue
	}
	…
}
```

**`swarms/swe/acceptance_test.go:246` and `:627`** (recorder goroutines — identical shape at both sites):
```go
// BEFORE
for ev := range sub.Events() {
	rec.mu.Lock()
	rec.events = append(rec.events, ev)
	rec.mu.Unlock()
}
// AFTER
for d := range sub.Events() {
	rec.mu.Lock()
	rec.events = append(rec.events, d.Event)
	rec.mu.Unlock()
}
```

**`swarms/swe/operator_eval_integration_test.go:62`**:
```go
// BEFORE
case ev, ok := <-sub.Events():
	if !ok { return "", sub.Err() }
	switch e := ev.(type) { … }
// AFTER
case d, ok := <-sub.Events():
	if !ok { return "", sub.Err() }
	switch e := d.Event.(type) { … }
```

**`swarms/swe/persistence_integration_test.go:65` and `:98`** (both `case ev, ok := <-sub.Events():` then `switch ev.(type)` / `if _, ok := ev.(event.WorkspaceCheckpointed)`):
```go
// BEFORE
case ev, ok := <-sub.Events():
	if !ok { t.Fatal(…) }
	switch ev.(type) { case event.TurnDone, event.TurnFailed, event.TurnInterrupted: return }
// AFTER
case d, ok := <-sub.Events():
	if !ok { t.Fatal(…) }
	switch d.Event.(type) { case event.TurnDone, event.TurnFailed, event.TurnInterrupted: return }
```
(Apply the same `d.Event` swap to the `event.WorkspaceCheckpointed` check at `:98`.)

> Note: `swarms/swe/acceptance_test.go:578/585` and `runtime_skills_integration_test.go:111/118` type-assert `ev.(event.PermissionRequested)` where `ev` comes from a **recorder snapshot** (`rec.events []event.Event`), *not* directly from `sub.Events()`. Because the recorder goroutine now stores `d.Event` (an `event.Event`), those assertion sites are unchanged. Confirm `rec.events` remains typed `[]event.Event` when applying the recorder change above.

### B. Gate trio rewrite (Delta 3) — `swarms/swe/agent.go:230-245`

Add imports `"github.com/looprig/harness/pkg/gate"` and `"encoding/json"` to `agent.go`. Introduce one private helper on `*sessionAgent` and rewrite the three methods. The helper resolves the open gate for a `callID`:

```go
// findOpenGate returns the open gate in the session directory whose subject is
// callID and whose kind matches want. It is the bridge from the legacy
// (loopID, callID) addressing to the directory's GateID addressing.
func (a *sessionAgent) findOpenGate(ctx context.Context, callID uuid.UUID, want gate.Kind) (gate.Gate, bool) {
	for _, g := range a.session.ListGates(ctx) {
		if g.Kind == want && g.Subject.ToolExecutionID == callID {
			return g, true
		}
	}
	return gate.Gate{}, false
}
```

**Approve** (`agent.go:230`):
```go
// BEFORE
func (a *sessionAgent) Approve(ctx context.Context, loopID, callID uuid.UUID, scope tool.ApprovalScope) error {
	return a.session.Approve(ctx, loopID, callID, scope)
}
// AFTER
func (a *sessionAgent) Approve(ctx context.Context, loopID, callID uuid.UUID, scope tool.ApprovalScope) error {
	g, ok := a.findOpenGate(ctx, callID, gate.KindPermission)
	if !ok {
		return &gateNotOpenError{loopID: loopID, callID: callID} // see note below
	}
	scopeVal, ok := tool.ApprovalScopeValue(scope)
	if !ok {
		return &invalidScopeError{scope: scope}
	}
	raw, err := json.Marshal(scopeVal)
	if err != nil {
		return err
	}
	return a.session.RespondGate(ctx, gate.GateResponse{
		GateID: g.ID,
		Action: "approve",
		Values: map[string]json.RawMessage{"scope": raw},
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	})
}
```

**Deny** (`agent.go:237`):
```go
// AFTER
func (a *sessionAgent) Deny(ctx context.Context, loopID, callID uuid.UUID) error {
	g, ok := a.findOpenGate(ctx, callID, gate.KindPermission)
	if !ok {
		return &gateNotOpenError{loopID: loopID, callID: callID}
	}
	return a.session.RespondGate(ctx, gate.GateResponse{
		GateID: g.ID,
		Action: "deny",
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	})
}
```

**ProvideAnswer** (`agent.go:244`):
```go
// AFTER
func (a *sessionAgent) ProvideAnswer(ctx context.Context, loopID, callID uuid.UUID, answer string) error {
	g, ok := a.findOpenGate(ctx, callID, gate.KindAskUser)
	if !ok {
		return &gateNotOpenError{loopID: loopID, callID: callID}
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		return err
	}
	return a.session.RespondGate(ctx, gate.GateResponse{
		GateID: g.ID,
		Action: "answer",
		Values: map[string]json.RawMessage{"answer": raw},
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	})
}
```

**Typed errors (CLAUDE.md compliance).** Per `swe`'s rules ("all errors must be typed"), define concrete error structs (do not return `errors.New`/`fmt.Errorf`) for the two new failure modes above — e.g. `gateNotOpenError{loopID, callID uuid.UUID}` ("no open gate for tool call …") and `invalidScopeError{scope tool.ApprovalScope}`. Place them alongside the methods in `agent.go`. The `RespondGate` error is already a typed `*session.GateError` and is returned unwrapped.

> Semantic note to preserve in a code comment: unlike the old trio (which routed onto a parked runner and could return a `session.SessionError` on a loop-exited path), the new path fails with `gateNotOpenError` when the directory has no matching open gate (e.g. the session is closed, the gate was already resolved, or the tool call is unknown). This is intentional and fail-secure.

---

## Test-update list

| Test file | Break | Required change |
|---|---|---|
| `swarms/swe/persistence.go` (prod) | `.Events()` range | `d.Event` unwrap (§A) |
| `swarms/swe/acceptance_test.go` (×2 recorder ranges) | `.Events()` range | `d.Event` unwrap (§A). Assertions on `event.PermissionRequested` from the recorder are unchanged. |
| `swarms/swe/operator_eval_integration_test.go` | `.Events()` channel read | `d.Event` unwrap (§A) |
| `swarms/swe/persistence_integration_test.go` (×2) | `.Events()` channel reads | `d.Event` unwrap (§A) |
| `swarms/swe/acceptance_test.go:586` | `agent.Approve(ctx, pr.Coordinates.LoopID, pr.ToolExecutionID, tool.ScopeOnce)` | **No signature change** — the `Approve(loopID, callID, scope)` shape is preserved, so this call compiles as-is once §B lands. It exercises the new `ListGates`→`RespondGate` path end-to-end (the gate is open in the directory when `PermissionRequested` is observed). Verify it still passes under `-tags integration -race`. |
| `swarms/swe/runtime_skills_integration_test.go:120` | same `agent.Approve(...)` shape | Same as above — no source change; verify at runtime. |
| `swarms/swe/agent_test.go` `TestSessionAgentGateTrioDelegatesToSession` (`:266/:272/:280`) | **Semantics change** | This test drives `Approve`/`Deny`/`ProvideAnswer` on a **closed** agent with a **random** `callID` and asserts the error is a `*session.SessionError` (the old loop-exited delegation path). Under §B, a closed session's `ListGates` is empty, so each method returns `gateNotOpenError` **before** reaching the session — it never produces a `session.SessionError`. **Rewrite the assertion** to expect `gateNotOpenError` (via `errors.As`). Rename/retitle the test away from "DelegatesToSession" since the delegation contract it pinned no longer exists. |

No other `swe` tests reference the trio or `.Events()` (verified via `rg`).

---

## go.mod / vendoring

- `swe/go.mod` pins `github.com/looprig/harness v0.5.0` with `replace github.com/looprig/harness => ../harness`. The build resolves harness **from the local replace**, so the `v0.5.0` version string is cosmetic for building against `../harness`.
- **No `vendor/` tree** exists in `swe` — so **no `go mod vendor`** step is required.
- A `go mod tidy` is **not required for the build** (the replace covers it). If the harness change bumped harness's module version tag and `swe` wants the pin to reflect it, bump the `require` line and run `go mod tidy` as a cosmetic follow-up — but this is optional and does **not** gate the migration. Do not run a tidy that would rewrite unrelated pins.

---

## Build / verify steps (in order)

1. Land the harness change first (this spec assumes `../harness` already exposes `Delivery`, the `Subscription` interface return, and `RespondGate` — all verified present today).
2. Apply §A (`.Events()` unwrap) and §B (gate trio rewrite + typed errors) to `swe`.
3. `cd /Users/ipotter/code/looprig/swe`
4. `CGO_ENABLED=0 go build -trimpath ./...`
5. `go test -race ./...`
6. `go test -tags integration -race ./...` (covers `persistence_integration_test.go`, `operator_eval_integration_test.go`, `runtime_skills_integration_test.go`, and the gated acceptance paths).
7. `make secure` / `make fmt-check` per `swe`'s makefile before committing.

**Ordering:** this migration lands **after** the harness change. If the `cli` module depends on `swe`'s gate methods (or on a future `RespondGate` passthrough — see the crux open question), coordinate so this `swe` change lands **before** `cli`'s migration.

---

## Open questions (do not guess — resolve before/while implementing)

1. **cli coupling / method shape.** Whether to keep the signature-preserving `(loopID, callID)` trio + `ListGates` scan (this spec's default) or pivot `sessionAgent` to gate.ID-typed methods / a `RespondGate` passthrough sourced from `event.GateOpened`. Decide with the `cli` migration owner. If pivoting, the tests in §B/Test-list that pass `pr.ToolExecutionID` must instead source `GateOpened.Gate.ID`, and the crux `ListGates` helper is dropped.
2. **`accepted_grants` for Bash.** The migration does not thread Bash grant tokens through `Approve` (the current signature has no grants parameter, and today's callers pass none). If `swe` later needs to accept specific Bash sub-command grants, `Approve` must gain a `[]string` grants argument feeding `Values["accepted_grants"]`; out of scope here — flag if a caller needs it.
3. **Race window (believed closed).** The `ListGates`→`RespondGate` path assumes the gate is still `gateOpen` at reply time. Install-before-emit guarantees it is open *when the request event is observed*; a second/duplicate reply after resolution returns `GateNotFound` (fail-secure, acceptable). Confirm no `swe` caller relies on idempotent double-approve.
