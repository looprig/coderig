# CodeRig contributor instructions

CodeRig is the reference coding Rig built from looprig modules. This repository owns coding behavior and product assembly. Reusable runtime, presentation, tools, confinement, storage, and inference machinery belongs in the module that defines that abstraction.

## Architecture

- `internal/app/swarm.go` assembles the primary operator and the fixed leaf Loops.
- `internal/catalog/operator` and `internal/catalog/reviewer` own role identity and prompts.
- `cmd/coderig` imports the private `internal/app` composition boundary. The module root has no Go package.
- The primary operator may delegate to a non-delegating operator or reviewer. Leaves do not receive delegation capability.
- Each Loop receives only the individual tools it needs. The reviewer has no file mutation tools.
- `github.com/looprig/tools` provides optional standard tools.
- `github.com/looprig/confinement` connects standard tools to `github.com/looprig/sandbox`.
- `github.com/looprig/tui/sessionadapter` adapts a session controller to the TUI.
- Session creation and restore use the same assembly path.

Do not add a generic agent registry or model tier catalog. The roster is a small fixed set of Loop definitions. Runtime choices belong in Loop modes and model effort.

## Placement

Keep behavior here when it is specific to a coding Rig, such as prompts, role tool selection, coding modes, model defaults, and product flags.

Move behavior to its owning module when it is reusable across products. Examples include session adapters, standard tool implementations, confinement wiring, persistence mechanics, and generic Loop or Rig lifecycle behavior.

Prefer direct assembly over local wrappers that only rename another module's API.

## Security

- Give each Loop the minimum tool set and maximum confinement mode it needs.
- Keep mutating, command, and network effects human-gated unless enforced guarantees justify automatic approval.
- Treat `Bash` as intentionally shell-based. Permission checks and OS confinement are its boundaries.
- Validate CLI input before constructing the Rig.
- Never log secrets or place them in audit summaries.
- Fail closed when permission, confinement, identity, or durable policy state is uncertain.

## Code and tests

- Keep packages cohesive. Split code when ownership or invariants differ, not to satisfy an arbitrary size rule.
- Introduce interfaces at consumer boundaries or when multiple implementations justify them.
- Use typed errors when callers need to classify or recover. Wrapped ordinary errors are fine for contextual failures.
- Use table-driven tests when cases share setup and assertions. Focused tests are fine for singular behavior.
- Add integration tests for process, filesystem, network, or durable storage boundaries.
- Run `gofmt` on changed Go files and `go test -race ./...` before committing.

## Commands

```bash
make build
make test
make lint
make secure
```

The binary and command are both named `coderig`.

## Dependencies

Prefer the standard library. Ask before adding a new third-party dependency. Sibling looprig modules already in `go.mod` are approved architecture dependencies.

The following development-only analysis tools are approved and declared through
the Go toolchain:

- `github.com/securego/gosec/v2/cmd/gosec`
- `golang.org/x/vuln/cmd/govulncheck`
- `honnef.co/go/tools/cmd/staticcheck`

Run `make secure` before every commit. It verifies formatting, runs `go vet`,
Staticcheck, Gosec, `go mod verify`, and Govulncheck.

Do not commit, push, or rename a remote repository unless the user explicitly asks.
