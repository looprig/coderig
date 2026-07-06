# CLAUDE.md — Development Guidelines

This is the **SWE-Swarm** (`github.com/looprig/swe`): a multi-agent software-engineering
swarm built on the **looprig** framework. The entire agent runtime — loop, session, tools,
tui, identity, content, journal — lives in looprig (`github.com/looprig/harness`), which swe
consumes as a Go module. swe itself owns the swarm: the model/provider wiring, the agent
roster, the system identity, and the composition root that assembles looprig's loop into a
runnable TUI agent.

## Architecture (swe-specific)

The current, post-consolidation shape (verified against `swarms/swe/swarm.go`,
`agents/operator/operator.go`, `agents/reviewer/reviewer.go`):

- The swarm's **PRIMARY** loop is an **`operator`** (`swarms/swe/swarm.go`). Its toolset is
  read/search (`ReadFile`, `Glob`, `Grep`) + web (`WebSearch`, `Fetch`) + write/edit/`Bash` +
  `Todo`/`AskUser` + **`Subagent`** + the optional code-style **`Skill`** tool.
- The primary can spawn two **LEAF** agents by name:
  - **`operator`** — the same toolset as the primary **MINUS `Subagent`**, so a spawned
    operator cannot spawn again.
  - **`reviewer`** — read/search (`ReadFile`, `Glob`, `Grep`) + `Bash`, critique-only, no
    write/edit tool and no `Subagent`. It reports findings; it never mutates.
- The agent tree is therefore **depth-1**: a primary operator spawns a non-spawning leaf.
  This is enforced **structurally** (leaves carry no `Subagent` tool) with a session **depth
  cap of 2** as a backstop (`operatorSpawnDepth`), plus a total spawn quota
  (`operatorSpawnQuota`).
- **Mutating/network tools** (`WriteFile`, `EditFile`, `Bash`, `WebSearch`, `Fetch`) are
  **human-gated** — they default to **Ask**, so a person reads and approves each call. The
  side-effect-free read/search/plan/ask/spawn tools (`ReadFile`, `Glob`, `Grep`, `Todo`,
  `AskUser`, `Subagent`, and the trusted in-repo `Skill`) auto-approve.

## SOLID Principles (strictly enforced)

**Single Responsibility** — Every struct, function, and package has exactly one reason to change. If you can't describe what it does in one sentence without "and", split it.

**Open/Closed** — Extend behavior via interfaces and composition. Never modify a working type to add new behavior; add a new type or wrap it.

**Liskov Substitution** — Every implementation of an interface must honor the full contract. If a concrete type can't satisfy a method without panicking, returning errors the caller doesn't expect, or silently doing less, redesign the interface.

**Interface Segregation** — Interfaces are small and focused. A caller should never be forced to depend on methods it doesn't use. Prefer many small interfaces over one large one.

**Dependency Inversion** — Depend on interfaces, not concrete types. High-level packages must not import low-level packages directly. Wire dependencies at the composition root (main or a factory), never inside business logic.

## Security — First-Class, Not an Afterthought

**Validate at every boundary.** All external input (HTTP, CLI args, env vars, files, queues) is untrusted until validated. Validate before it enters business logic, not inside it.

**Least privilege always.** Every component, goroutine, and service gets only the permissions it needs. Never pass a full config or god-object when a narrow interface suffices.

**No secrets in code.** No hardcoded tokens, passwords, keys, or connection strings — ever. Use environment variables or a secrets manager. Fail loudly on startup if required secrets are missing.

**Authenticate before authorize, authorize before act.** Every action that crosses a trust boundary must check identity first, then permission, then execute. Never assume a caller is trusted.

**Sanitize before use.** Never interpolate external data into queries, shell commands, file paths, or log messages without sanitization. Use parameterized queries, exec with argument lists, and filepath.Clean.

**Fail secure.** On error or ambiguity, deny by default. A failed permission check must block the action, not fall through.

**Log security events, not secrets.** Audit auth failures, permission denials, and unexpected inputs. Never log credentials, tokens, or PII.

## Dependencies

**Prefer stdlib.** Always reach for the Go standard library first. If a need can be met with stdlib — even with a bit more code — use stdlib.

**External packages require explicit user approval.** Before adding any external dependency, stop and ask the user. State what the package is, why stdlib is insufficient, and what the package adds. Do not `go get` or add to `go.mod` without a clear "yes" from the user in the current conversation.

**Amend this file when approved.** Once a package is approved, add it here so future sessions know it is sanctioned:

<!-- Approved external packages -->
- `github.com/looprig/harness` — the SWE-Swarm framework and the entire agent runtime
  (loop / session [NATS-backed persistence] / tools [`ReadFile`, `Glob`, `Grep`, `WriteFile`,
  `EditFile`, `Bash`, `WebSearch`, `Fetch`, `Subagent`, `Skill`, `Todo`, `AskUser`, +
  `PermissionChecker`] / tui / identity / content / journal). A **direct** dependency.
- `github.com/nats-io/nats.go` — JetStream client for session persistence; used by
  `swarms/swe/persistence.go` and `swarms/swe/agent.go`. A **direct** dependency.
- `github.com/looprig/sandbox` — the OS-sandbox module (Seatbelt/landlock policy +
  `Executor` runner). swe is the ONLY module that may import BOTH harness and sandbox
  (SPEC §2): it wires the sandbox's confined runner into harness's stdlib-typed
  `tool.CommandRunner` seam and maps the session ceiling ordinal → `sandbox.Mode` →
  `tools.Posture` (`swarms/swe/security.go`). Its only transitive dep, `golang.org/x/sys`,
  is already in swe's graph, so it is offline-safe. Resolved via the `../sandbox` local
  replace (unpublished). A **direct** dependency.
- The **Bubble Tea v2** TUI stack, inherited **transitively via looprig's TUI** (all `// indirect`
  in `go.mod`): `charm.land/bubbletea/v2`, `charm.land/bubbles/v2`, `charm.land/lipgloss/v2`,
  `charm.land/glamour/v2`. These use the `charm.land/...` vanity import paths, **not**
  `github.com/charmbracelet/...`.
- `github.com/yuin/goldmark` and `github.com/yuin/goldmark-emoji` — Markdown rendering
  dependencies inherited **transitively via looprig's transcript HTML export**. These are
  shipped in the SWE binary through looprig and remain `// indirect` in `go.mod`.
- **Bubble Tea fork pin.** swe replaces the upstream v2 module with a fork (the "strand-fix"
  fork) via this `go.mod` `replace` directive:

  ```
  replace charm.land/bubbletea/v2 => github.com/looprig/bubbletea/v2 v2.0.0-20260623210731-9571e88971cd
  ```

  The same `replace` is mirrored in the workspace `go.work` so workspace builds use the fork too.

## Secure Coding Patterns

**Randomness** — Use `crypto/rand` for anything security-sensitive (tokens, nonces, IDs). Never use `math/rand` for secrets.

**Queries** — Always use parameterized queries via `database/sql`. Never format SQL with `fmt.Sprintf` or string concatenation.

**HTTP server** — Always set explicit timeouts. No naked `http.ListenAndServe` with default server:
```go
srv := &http.Server{
    ReadTimeout:    5 * time.Second,
    WriteTimeout:   10 * time.Second,
    IdleTimeout:    60 * time.Second,
    MaxHeaderBytes: 1 << 20,
}
```

**TLS** — Never set `InsecureSkipVerify: true`. Never use TLS versions below 1.2. Default to `tls.Config{MinVersion: tls.VersionTLS12}`.

**Context** — Every I/O call (HTTP, DB, file, external service) must use a `context.Context` with a timeout or deadline. No unbounded blocking.

**Shell commands** — Never pass user input to `exec.Command` as a shell string. Always pass args as separate parameters.

> **Documented exception — the `Bash` tool.** swe wires looprig's `Bash` tool (from
> `github.com/looprig/harness/pkg/tools`, `tools.NewBash`) into the **primary operator**, the
> **operator leaf**, and the **reviewer**. `Bash` runs a single command via `sh -c <command>` — a
> deliberate violation of the shell-args rule above, because a coding agent genuinely needs shell
> features (pipes, globs, `&&`, redirects) an argv list can't express. The security boundary is the
> **permission gate**, not the argv shape: `Bash` defaults to **Ask**, so a human reads and approves
> each command before it runs. The real hard boundary — OS-level sandboxing (seccomp/landlock/nsjail)
> — is **out of scope** in looprig and is the prerequisite for ever auto-approving `Bash` broadly;
> until then `Bash` must stay human-gated.

**File paths** — Always call `filepath.Clean` and verify the result stays within the expected root before opening files from user-supplied paths.

## Build & Testing Requirements

All targets live in swe's [`Makefile`](./Makefile).

**Build** — `make build` runs `CGO_ENABLED=0 go build -trimpath -o bin/swe ./cmd/swe`. Never
ship a binary without `-trimpath` (leaks local paths).

**Run** — `make run` loads `.env` (if present) so `LLM_API_KEY` and friends are exported, then
runs `go run ./cmd/swe`.

**Tests** — `make test` runs `go test -race ./...`. Always run with `-race`. A test that passes
without `-race` but not with it is not passing.

**Format** — All Go code must be `gofmt`-clean. `make fmt` runs `gofmt -w` over this module's
package dirs (`go list -f '{{.Dir}}' ./...`, which stops at module boundaries and skips deps —
never reformat dependency files). `make fmt-check` fails if anything is unformatted and is wired
into `make lint`. `make lint` runs `fmt-check` then `go vet ./...`.

> **Not yet wired.** Unlike looprig, swe's `Makefile` has **no** security-scanning targets — there
> is **no `make secure`**, and no `gosec`, `govulncheck`, or `staticcheck`. Do not assume they
> exist. The available targets are exactly: `build`, `run`, `test`, `fmt`, `fmt-check`, `lint`.

The testing **discipline** below still applies in full, regardless of which targets are wired:

**Table-driven tests (mandatory).** Every test function uses a `[]struct{ name string; ... }` table. Each table must cover:
- Happy path (valid, expected input → expected output)
- Boundary values (zero, empty, max, minimum valid)
- Error cases (invalid input, missing required fields, wrong types)
- Edge cases specific to the domain (e.g. nil blocks, empty message threads, unknown block types)

```go
func TestFoo(t *testing.T) {
    tests := []struct {
        name    string
        input   Bar
        want    Baz
        wantErr bool
    }{
        {name: "happy path", ...},
        {name: "empty input", ...},
        {name: "nil field returns error", ..., wantErr: true},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel()
            got, err := Foo(tt.input)
            if (err != nil) != tt.wantErr {
                t.Fatalf("Foo() error = %v, wantErr %v", err, tt.wantErr)
            }
            if !tt.wantErr && got != tt.want {
                t.Errorf("Foo() = %v, want %v", got, tt.want)
            }
        })
    }
}
```

**Integration tests** — Write integration tests (tagged `//go:build integration`) for any code that crosses a process boundary: HTTP providers, database queries, filesystem operations, NATS/JetStream session persistence. Integration tests live in `*_integration_test.go` files and are excluded from the default `go test ./...` run. Run them explicitly with `go test -tags integration -race ./...`.

**Fuzzing** — For any function that parses external input, write a fuzz target: `go test -fuzz=FuzzXxx ./pkg -fuzztime=30s`.

## Workspace (swe-specific)

The workspace file `/Users/ipotter/code/go.work` declares `use ./looprig` and `use ./swe`. So a
normal `go build`/`go test` from inside the workspace compiles swe against the **local looprig
checkout** — the `go.mod` version pin (`github.com/looprig/harness v0.2.0`) is **masked**
in-workspace, and your local looprig edits are picked up directly.

For a **clean, pinned** build (against the `go.mod` version, not the local checkout) set
`GOWORK=off`. looprig is a **private** module, so when fetching/building with the workspace off,
also set `GOPRIVATE='github.com/looprig/*'` (and `GOSUMDB=off`) so the toolchain doesn't try the
public proxy/checksum DB:

```bash
GOWORK=off GOPRIVATE='github.com/looprig/*' GOSUMDB=off go build ./...
```

## Code Rules

- **Strict typing everywhere.** Never use `any` or `interface{}` except at explicit serialization boundaries (JSON unmarshal, plugin APIs). Immediately narrow to a concrete type; never pass `any` deeper into business logic. No untyped magic numbers or strings — use named constants or typed enums. Prefer named types (`type UserID string`) over bare primitives when the value has domain meaning.
- All domain concepts are typed structs — no `map[string]interface{}` for domain data.
- Return errors explicitly; never swallow them with `_`.
- **All errors must be typed.** Define a concrete error struct for every distinct failure mode. Never return `errors.New("...")` or `fmt.Errorf("...")` from package-level APIs — those lose type identity at the call site. Callers must be able to `errors.As` to the concrete type to inspect cause and context. Sentinel errors (`var ErrFoo = errors.New(...)`) are permitted only for leaf errors with no additional context fields.
- Keep packages shallow and cohesive; avoid circular imports.
- Write the interface first, then the implementation.
- If a function exceeds ~30 lines, ask whether it violates SRP before adding more.
