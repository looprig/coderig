# swe → harness storekit/sessionstore/workspacestore Migration — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (or superpowers:subagent-driven-development for same-session) to implement this plan task-by-task.

**Goal:** Make `github.com/looprig/swe` build and run against the storekit-extracted `github.com/looprig/harness`, replacing the deleted `pkg/persistence` + direct-NATS wiring with `sessionstore` over `fsstore`, adopting the new `session.Restore` signature, and wiring workspace snapshots (`CheckpointWorkspace` on `SessionIdle`).

**Architecture:** swe's composition root stops driving NATS journal primitives directly. It opens **one** `fsstore` backend and wraps it in a `*sessionstore.Store` (which addresses every session by name, `sessions/<uuid>`, and owns lease/journal/replay/GC/catalog internally). Sessions are constructed with the existing `session.New`/`session.Restore` options; the new `Restore` takes the store and does lease-acquire + journal-open + replay + workspace-materialize internally. The same `fsstore` backing also feeds a `*workspacestore.Store` for durable workspace snapshots, checkpointed at the session's quiescence (`event.SessionIdle`). The per-session embedded-engine abstraction and the whole NATS dependency chain are removed.

**Tech Stack:** Go stdlib; `github.com/looprig/harness` (`pkg/session`, `pkg/sessionstore`, `pkg/workspacestore`, `pkg/event`, `pkg/journal` neutral contracts); `github.com/looprig/storekit` (contracts + `memstore` for tests); `github.com/looprig/fsstore` (laptop backend). No NATS.

**Companion specs (context, in the harness repo):** `docs/plans/2026-07-02-storekit-sessionstore-plan.md` (Phase E / Task E1 — swe session wiring) and `docs/plans/2026-07-02-workspacestore-plan.md` (Phase D / Task D1 — swe workspace wiring, D2 e2e). This plan supersedes those high-level steps with concrete call sites and resolves two decisions they left open (titling, per-session-engine collapse).

---

## Execution notes

- **Workspace mode:** swe builds under the `~/code/go.work` workspace (`go build ./...` from `~/code/swe`, GOWORK on). Harness/storekit/fsstore resolve via the workspace + local `replace`s. Run tests the same way; use `-race` (`go test -race ./...`). Integration tests are `//go:build integration`.
- **First green build is deferred:** swe will NOT compile until Phase 1 + Phase 2 land together (the composition root and the titling feature both import the deleted `pkg/persistence`). Tasks below note the first buildable checkpoint. Until then, verify per-file with `go build ./swarms/swe/` incrementally where possible and lean on `go vet` after each cluster.
- Typed errors, gofmt-clean, table-driven `t.Parallel()` tests, frequent commits — same house rules as harness (`CLAUDE.md`).
- Commit in the `swe` repo (branch off `main`: `git switch -c feat/harness-storekit-migration`). Do NOT push until the full suite is green and the user approves.

---

## Phase 0 — Decisions, dependencies, branch

### Task 0.1: Confirm the two open product decisions

**DECISION A — session titling (RESOLVE BEFORE Phase 2).** swe's LLM titling feature (`session_title.go` + title coordinator + economy model + `installTitleCoordinator` wiring) writes titles via `persistence.SetTitle(title, persistence.TitleSource, now)`. The new `sessionstore.Catalog` **auto-derives** a title from the first user message (`deriveTitle`, `catalog.go`) and exposes **no title-write API**.
- **Recommended (this plan assumes it): DROP the LLM titling feature.** Delete `session_title.go` + its coordinator/economy wiring + tests; the session list uses the catalog's auto-derived title. Smallest blast radius, no harness change, reversible.
- Alternative 1 — **swe-side KV title store:** persist LLM titles in `fsstore`'s `storekit.KV` primitive keyed by session id; read them back for the session list. No harness change; adds a swe-local title store + read path.
- Alternative 2 — **harness title-write API:** add `SetTitle`/override to `sessionstore.Catalog` in harness, rewire swe onto it. Preserves the feature; expands scope into another module + review.
- **If the user picks an alternative, Phase 2 changes accordingly; the rest of the plan is unaffected.**

**DECISION B — session data dir.** The store root formerly lived inside the deleted `pkg/persistence` (default `~/.looprig/jetstream`). swe now owns it.
- **Recommended:** default `~/.looprig/store` (session ledger/journal/blobs under fsstore), overridable via swe's existing config surface / a `--data-dir` flag (mirror the old default's location). Confirm swe's config plumbing in `cmd/swe/main.go` and reuse it.

**DECISION C — legacy purge.** `root.PurgeLegacyStore()` / `PurgeLegacyResult` / the `--purge-legacy-sessions` flag have no target (no legacy jetstream dir). **Remove them** across production + tests. (No user decision needed unless they want a one-time migration tool — out of scope here.)

### Task 0.2: Dependency surgery (`swe/go.mod` + `~/code/go.work`)

**Files:** Modify `swe/go.mod`, possibly `~/code/go.work`.

- **Add** requires: `github.com/looprig/fsstore` and `github.com/looprig/storekit`. Resolve them locally the same way harness does — add `replace github.com/looprig/storekit => ../ciram-co/storekit` and `replace github.com/looprig/fsstore => ../ciram-co/fsstore` to `swe/go.mod` (paths relative to `~/code/swe` → `~/code/ciram-co/*`). Verify: `(cd ~/code/swe && cd ../ciram-co/storekit && pwd)` resolves.
- **Remove** the now-unused NATS chain from `swe/go.mod`: the direct `github.com/nats-io/nats.go` require and the embedded-server indirect chain (`nats-server/v2`, `nats-io/jwt/v2`, `nkeys`, `nuid`, `minio/highwayhash`) — let `go mod tidy` drop them after the code no longer imports NATS (do this at the end of Phase 1, not now).
- Verify workspace resolution: `GOWORK=off go list -m github.com/looprig/storekit` from swe should show the replace target; a workspace `go build` should find fsstore.
- **Step — commit:** `chore(swe): add fsstore/storekit deps, branch for harness migration`.

---

## Phase 1 — Composition root: fsstore + sessionstore

> `swarms/swe/persistence.go` (546 lines) is a near-total rewrite. Approach it as: build the new `Persistence` struct + `NewPersistence` first, then port `openNew`, `openResume`, GC, and delete the per-session-engine layer. swe won't build mid-phase; use `go vet ./swarms/swe/` to track shrinking error counts.

### Task 1.1: `Persistence` struct + `NewPersistence` over `sessionstore`

**Files:** Modify `swarms/swe/persistence.go` (struct + constructor region, ~lines 55–189).

- Replace the NATS-driven `Persistence` fields (`js nats.JetStreamContext`, `leases *journal.LeaseManager`, `catalog *journal.Catalog`) with `store *sessionstore.Store` and `catalog *sessionstore.Catalog` (from `store.OpenCatalog(...)`).
- `NewPersistence` (was `journal.NewLeaseManager(js)` + `journal.NewCatalog(js, WithCatalogReplayer(...))`) becomes:
  ```go
  fs, err := fsstore.Open(fsstore.Options{Root: dataDir})   // dataDir from Decision B
  if err != nil { return nil, &InitError{...} }
  store, err := sessionstore.Open(fs.Backend())             // *storekit.Composite
  if err != nil { return nil, &InitError{...} }
  cat := store.OpenCatalog(sessionstore.WithCatalogReplayer(...)) // if swe needs the replay-backed catalog
  ```
- Delete the per-session engine abstraction: `sessionEngine`, `engineOpener`, `storeRootEngineOpener`, `OpenSessionEngine` usage, and `appendEngineClose` teardown (persistence.go ~84–102, 291–302). One fsstore backend serves all sessions; there is no per-session engine to open/close.
- **Verify:** `go vet ./swarms/swe/` — the `NewLeaseManager`/`NewCatalog`/`SessionEngine` errors are gone (new ones remain downstream).
- **Commit:** `refactor(swe): open sessionstore over fsstore in NewPersistence`.

### Task 1.2: `openNew` over `*sessionstore.Store` methods

**Files:** Modify `swarms/swe/persistence.go` `openNew` (~354–413).

Migrate each construction site (see the migration table at the end):
- `p.leases.Acquire(ctx, id)` → `store.AcquireLease(ctx, id)` (`journal.Lease`).
- `journal.NewSessionJournal(p.js, id, lease)` → `store.OpenJournal(ctx, id, lease)`.
- Drop `p.js.ObjectStore(journal.SessionObjectBucket(id))` (blobs are internal to sessionstore).
- `journal.NewJournalEventAppenderChecked(j, journal.WithCatalog(p.catalog))` — **unchanged call**, now passing the `*sessionstore.Catalog` (it satisfies the `catalogUpdater` interface).
- `journal.NewJournalCommandAppenderChecked(j)` — unchanged.
- Session options passed to `newPersistentSessionAgent` are unchanged (`WithSessionID/WithEventAppender/WithCommandAppender/WithLeaseRelease/WithLimits/WithConfigFingerprintFields`).
- **Verify:** `go vet ./swarms/swe/` — `openNew` errors resolved.
- **Commit:** `refactor(swe): openNew builds session over sessionstore journal`.

### Task 1.3: `openResume` + `newRestoredSessionAgent` new `Restore` signature

**Files:** Modify `swarms/swe/persistence.go` `openResume` (~420–459); `swarms/swe/agent.go` `newRestoredSessionAgent` (~116–127).

- New `session.Restore(ctx, cfg, sessionID, store, opts...)` does lease-acquire + journal-open + replay + workspace-materialize **internally**. So `newRestoredSessionAgent`'s signature collapses from `(ctx, primary, id, js, objects, leases, opts...)` to `(ctx, primary, id, store *sessionstore.Store, opts...)` and its body becomes `session.Restore(ctx, primary, id, store, opts...)`.
- `openResume` drops the `journal.NewEventReplayer`/`NewRecordReplayer`/`ObjectStore` pre-wiring it did for the old Restore; it just passes `p.store` (and the workspace option from Phase 3) through.
- If swe needs a **record replayer** for its own purposes elsewhere (e.g. transcript export), use `store.OpenRecordReplayer(id, sessionstore.ReplayRequest{})` / `store.OpenEventReplayer(...)`.
- **Verify:** `go vet ./swarms/swe/` — Restore-signature + replayer errors resolved.
- **Commit:** `refactor(swe): restore via new session.Restore(store) signature`.

### Task 1.4: GC over `store.OpenObjectGC`

**Files:** Modify `swarms/swe/persistence.go` GC region (~466–524).

- `journal.NewObjectGC(js, objects, lease, id)` → `store.OpenObjectGC(id, lease)` (`*sessionstore.ObjectGC`). Drop the `js.ObjectStore(SessionObjectBucket(id))` plumbing.
- **Verify:** `go vet ./swarms/swe/` — GC errors resolved.
- **Commit:** `refactor(swe): session blob GC via sessionstore.OpenObjectGC`.

### Task 1.5: Drop NATS imports + tidy

**Files:** `swarms/swe/persistence.go`, `swarms/swe/agent.go` imports; `swe/go.mod`.

- Remove `github.com/nats-io/nats.go` imports and any `nats.JetStreamContext`/`nats.ObjectStore` type references remaining. The harness `pkg/journal` neutral contracts swe still uses (`journal.Lease`, `journal.ReplayRequest`, `journal.Beginning`, `journal.EventReplayer`, `journal.RecordReplayer`, `journal.JournalRecord`, `journal.NewEventRecord`, `journal.RecordCursor`, `journal.SessionJournal`, `journal.NewJournal*AppenderChecked`, `journal.WithCatalog`) stay.
- `GOWORK=off go mod tidy` in swe → drops the NATS dependency chain. Verify `grep -rn nats-io swe/go.mod` is empty.
- **Note:** `swarms/swe/` still won't fully build until Phase 2 removes/rewires `session_title.go` (it imports `persistence.SessionMetaStore`). That's expected.
- **Commit:** `chore(swe): drop NATS deps after journal migration`.

---

## Phase 2 — Session list + titling (per Decision A)

### Task 2.1: Session list via `catalog.ListSessions`

**Files:** Modify `swarms/swe/persistence.go` (`ListSessionMeta` region ~265–273) and any list consumer.

- `root.ListSessionMeta()` → `catalog.ListSessions(ctx)` returning `[]sessionstore.SessionMeta`.
- Map the field renames at every consumer: old `SessionListEntry.Meta{ID, UpdatedAt, Title, Status}` + `.Err` → `sessionstore.SessionMeta{SessionID, LastActiveAt, Title, Status, CreatedAt, ConfigFingerprint}` (no per-entry `Err`). Adjust `cmd/swe/main.go:146–157` accordingly (Phase 4).
- Delete `PurgeLegacyStore`/`PurgeLegacyResult` (Decision C).
- **Verify + Commit:** `refactor(swe): session list from sessionstore.Catalog`.

### Task 2.2: Remove the LLM titling feature (Decision A = drop)

**Files:** Delete `swarms/swe/session_title.go`, `swarms/swe/session_title_test.go`; remove `installTitleCoordinator` wiring + title coordinator/economy references in `persistence.go` (~196–222, 229–260 where `watchTitleEvents` writes titles).

- Remove the title-write path entirely. If swe still wants an event subscription (it does — Phase 3 reuses it for `SessionIdle`), **keep the subscription scaffold** but strip the title-setting `case`s; Phase 3 repurposes it.
- Remove references to `persistence.SessionMeta`, `SessionMetaStore`, `TitleSource*`, `metaStore.Init/SetTitle` throughout.
- **(If Decision A = KV or harness-API instead: replace this task with the corresponding write path — do NOT delete `session_title.go`; rewire its `SetTitle` to the chosen backend.)**
- **Verify + Commit:** `refactor(swe)!: drop LLM session titling (no title-write API in sessionstore)`.

---

## Phase 3 — Workspace snapshot wiring (new capability)

### Task 3.1: Build a workspacestore + wire `WithWorkspaceStore`

**Files:** Modify `swarms/swe/persistence.go` (`Persistence` struct + `openNew`/`openResume`).

- Add `ws *workspacestore.Store` to `Persistence`; in `NewPersistence`: `ws, err := workspacestore.Open(fs.Backend().Blobs)` (reuse the **same** fsstore backing as sessions — laptop profile).
- Resolve the workspace **root** (swe already computes `os.Getwd()` at persistence.go:325 — reuse it or swe's configured project root).
- Pass `session.WithWorkspaceStore(p.ws, root)` into BOTH `newPersistentSessionAgent`→`session.New` (Task 1.2 path) and `newRestoredSessionAgent`→`session.Restore` (Task 1.3 path). On restore, the harness auto-materializes the last `WorkspaceCheckpointed` before the session goes live — swe gets that for free once the option is wired.
- **Verify + Commit:** `feat(swe): wire workspacestore into session construction`.

### Task 3.2: `CheckpointWorkspace` at quiescence (`SessionIdle`)

**Files:** Modify the session-event subscription in `swarms/swe/persistence.go` (the `watchTitleEvents`→ renamed `watchSessionEvents` scaffold kept from Task 2.2).

- The subscription already filters `event.EventFilter{Enduring:{All:true}}`. Add:
  ```go
  case event.SessionIdle:
      if ref, err := agent.session.CheckpointWorkspace(ctx); err != nil {
          // log (do not crash the session); WorkspaceNotConfiguredError only if unwired
      } else {
          // optional: log ref
      }
  ```
- `SessionIdle` is the Active→Idle edge (`event.go:182`) — the same point a cloud harness would suspend. Checkpoint-before-suspend is the intended discipline.
- **This is net-new behavior** (swe has no SessionIdle handler today). Add a focused test with a memstore-backed workspace store asserting a `WorkspaceCheckpointed` is appended after a SessionIdle (mirror the harness B1 test shape).
- **Verify + Commit:** `feat(swe): checkpoint workspace on SessionIdle`.

---

## Phase 4 — CLI (`cmd/swe/main.go`)

### Task 4.1: Factory data-dir, list field renames, drop purge-legacy

**Files:** Modify `cmd/swe/main.go` (~86, 124–128, 146–157, 165–176, 215, 224–230).

- `swe.NewSessionStoreFactory()` → `swe.NewSessionStoreFactory(dataDir)` (Decision B); thread the data-dir from swe's config/flag.
- Session list printer (146–157): `e.Meta.ID/.UpdatedAt/.Title` + `e.Err` → `m.SessionID/.LastActiveAt/.Title/.Status`; drop the per-entry error column (the catalog list returns a single error, not per-entry).
- Delete the `--purge-legacy-sessions` flag + its handler (Decision C).
- **Verify + Commit:** `refactor(swe): CLI store factory data-dir + catalog session list`.

---

## Phase 5 — Tests + full verification

### Task 5.1: Rewrite `persistence_test.go` + `persistence_integration_test.go`

**Files:** `swarms/swe/persistence_test.go` (185), `swarms/swe/persistence_integration_test.go` (373).

- Replace NATS-typed fakes (`nats.JetStreamContext`/journal-manager fakes) with a **memstore-backed** `sessionstore.Open(memstore.New())` for unit tests and an **fsstore-backed** store (`t.TempDir()`) for the `//go:build integration` cycle.
- Remove `OpenSessionStoreRoot`/`SessionMeta`/`TitleSource`/`SetTitle` assertions (dropped feature).
- Keep/adapt the round-trip assertions: create session → append → close → new store instance over the same fsstore dir → resume → state equality. (Mirror harness `restore_roundtrip_test.go` + the workspace e2e.)
- **Commit:** `test(swe): sessionstore/fsstore-backed persistence tests`.

### Task 5.2: `agent_test.go` restore-path fakes

**Files:** `swarms/swe/agent_test.go` (351).

- Most survives (the neutral `journal.*` interfaces are unchanged). Update the Restore-path construction to the new `session.Restore(ctx, cfg, id, store, opts...)` signature + `*sessionstore.Store` fakes/reals.
- **Commit:** `test(swe): agent restore path over sessionstore`.

### Task 5.3: `main_test.go` + drop `session_title_test.go`

**Files:** `cmd/swe/main_test.go` (338); delete `swarms/swe/session_title_test.go` (done in 2.2).

- `seedSession` via `SetTitle`/`SessionStoreRoot` → seed via the new store (append events / use the catalog's auto-title). Update list assertions to `sessionstore.SessionMeta` fields.
- **Commit:** `test(swe): CLI tests over sessionstore`.

### Task 5.4: Full verification

- `GOWORK=off go build ./...` (swe) — green.
- Workspace build: `(cd ~/code/swe && go build ./...)` — green (resolves harness/storekit/fsstore via go.work).
- `GOWORK=off go test -race ./...` — green.
- `GOWORK=off go test -tags integration -race ./...` — green.
- `make secure` (if swe has it) / `go vet` / `gofmt -l` clean.
- **Manual smoke (the acceptance test):** run swe, create files in a session workspace + have a turn, reach quiescence (SessionIdle), kill the process, resume — confirm BOTH the conversation state AND the workspace files came back (workspacestore materialize-on-restore). This is the whole point of the extraction.
- **Commit:** `test(swe): full-suite + integration green on harness storekit`.

---

## Migration reference table (old harness API → new)

| swe call site (file:line) | Old | New |
|---|---|---|
| persistence.go NewPersistence | `journal.NewLeaseManager(js)` + `journal.NewCatalog(js, WithCatalogReplayer(NewEventReplayer(js,nil)))` | `fsstore.Open(Options{Root})` → `sessionstore.Open(fs.Backend())`; `store.OpenCatalog(...)` |
| persistence.go:84–102 | per-session `sessionEngine`/`OpenSessionEngine` | **deleted** (one backend, sessions by name) |
| persistence.go:360 | `p.leases.Acquire(ctx, id)` | `store.AcquireLease(ctx, id)` |
| persistence.go:364 | `journal.NewSessionJournal(p.js, id, lease)` | `store.OpenJournal(ctx, id, lease)` |
| persistence.go:369/421 | `p.js.ObjectStore(journal.SessionObjectBucket(id))` | removed (internal to sessionstore) |
| persistence.go:377 | `journal.NewJournalEventAppenderChecked(j, journal.WithCatalog(p.catalog))` | **same**, `WithCatalog(store.OpenCatalog())` |
| persistence.go:382 | `journal.NewJournalCommandAppenderChecked(j)` | unchanged |
| persistence.go:405/454 | `journal.NewRecordReplayer(p.js, objects)` | `store.OpenRecordReplayer(id, ReplayRequest{})` |
| persistence.go:451 | `journal.NewEventReplayer(p.js, objects)` | `store.OpenEventReplayer(id, ReplayRequest{})` |
| agent.go:116/121 | `newRestoredSessionAgent(ctx,cfg,id,js,objects,leases,opts)` → `session.Restore(...,js,objects,leases,opts)` | `newRestoredSessionAgent(ctx,cfg,id,store,opts)` → `session.Restore(ctx,cfg,id,store,opts)` |
| persistence.go:466–524 | `journal.NewObjectGC(js,objects,lease,id)` | `store.OpenObjectGC(id, lease)` |
| persistence.go:265 | `root.ListSessionMeta()` → `[]SessionListEntry` | `catalog.ListSessions(ctx)` → `[]sessionstore.SessionMeta` |
| persistence.go:197/205, session_title.go | `OpenSessionMeta(id)` + `Init` + `SetTitle(t, TitleSource, now)` | **no equivalent** — Decision A (drop / KV / harness-API) |
| persistence.go:272 | `root.PurgeLegacyStore()` | **delete** |
| main.go:215 | `NewSessionStoreFactory()` | `NewSessionStoreFactory(dataDir)` |
| main.go:146–157 | `e.Meta.ID/.UpdatedAt/e.Err` | `m.SessionID/.LastActiveAt` |
| new | — | `workspacestore.Open(fs.Backend().Blobs)`; `session.WithWorkspaceStore(ws, root)`; `case event.SessionIdle: session.CheckpointWorkspace(ctx)` |

**Files touched (~2,800 lines / 9 files):** `swarms/swe/persistence.go` (rewrite), `swarms/swe/agent.go` (sig+imports), `swarms/swe/session_title.go` (delete), `cmd/swe/main.go` (CLI), + the 5 test files. go.mod/go.work dependency surgery.
