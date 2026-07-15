# CodeRig Assembly Specification

## Purpose

CodeRig is the reference coding Rig built with looprig. It should contain coding behavior and product choices, while reusable runtime machinery lives in the appropriate modules.

## Name

Rename the repository, module, command, binary, and live product references from SWE or CodeRig to CodeRig:

```text
folder:  swe -> coderig
module:  github.com/looprig/coderig -> github.com/looprig/coderig
command: cmd/coderig -> cmd/coderig
binary:  bin/coderig -> bin/coderig
remote:  git@github.com:looprig/swe.git -> git@github.com:looprig/coderig.git
```

No source or session compatibility layer is required.

## Product-owned behavior

CodeRig owns:

- operator and reviewer prompts
- which tools each Loop receives
- which Loops may delegate to which Loops
- Loop modes and reasoning effort
- model and provider defaults
- Session security limit exposed by the command
- greeting and product identity
- storage location and command flags

## Reusable behavior moved out

CodeRig does not own:

- the `SessionController` to `tui.Agent` adapter
- generic tool definition wrappers
- permission checker construction mechanics
- sandbox executor binding and posture mapping
- a generic agent Registry
- model economy, standard, or premium tiers
- separate headless and persisted assembly graphs

## Loops and Rig assembly

CodeRig defines its Loops explicitly, then assembles one Rig from them.

```go
operator, err := operatorLoop(deps)
reviewer, err := reviewerLoop(deps)

assembly, err := rig.Define(
    rig.WithLoops(operator, reviewer),
    rig.WithPrimers(operator.Name()),
    rig.WithActivePrimer(operator.Name()),
    // storage, workspace, security limit, snapshots, and limits
)
```

There is no Registry. A static slice may be derived directly from the two definitions when the greeting needs ordered display metadata. Duplicate Loop names are already rejected by Rig definition validation.

## Modes and effort

Delete `ModelCatalog`, its resolver, and the economy, standard, and premium vocabulary.

Each Loop declares purposeful modes such as `build`, `review`, or `quick`, using `loop.WithModes`. A mode may change the model, reasoning effort, instructions, tools, or tool limits. Reasoning strength uses `inference.Effort`.

The base model remains an explicit CodeRig configuration dependency. There is no unused premium tier and no economy tier that is validated but not selected.

## One construction path

CodeRig has one session-opening function with injected storage and an explicit selector:

```go
type OpenConfig struct {
    Stores    Stores
    Workspace string
    SessionID uuid.UUID
    Resume    bool
}

func Open(ctx context.Context, app Config, open OpenConfig) (tui.Agent, error)
```

Production injects fsstore-backed stores. Tests inject memstore-backed stores. New and restore share model resolution, Loop definitions, Rig definition, workspace policy, and adapter selection.

If a public headless helper remains, it is a small wrapper that supplies memstore. It must not define another assembly graph.

## Repository rules

Replace rigid ceremony with engineering guidance:

- prefer cohesive responsibilities, but do not split code based on sentence grammar
- introduce interfaces at consumer boundaries or when multiple implementations are useful
- use typed errors when callers need classification or recovery
- use table-driven tests when several cases share the same setup and assertion shape
- use focused tests when a single scenario is clearer
- split functions based on ownership and readability, not an arbitrary line count
- preserve fail-secure behavior, context cancellation, race testing, and explicit validation

Apply the same correction to CLI and harness guidance where the current absolute rules created the misplaced abstractions.

## Historical documents

Existing plans that describe SWE remain historical records. Update live code, READMEs, package comments, examples, and current website content. Do not rewrite old plan documents as if CodeRig existed when they were written.
