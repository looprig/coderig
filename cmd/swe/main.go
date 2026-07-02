// Command swe is the SWE-Swarm TUI entry point and composition root. It parses the CLI
// invocation (--list / --resume), opens the session-store factory (which opens one isolated
// embedded JetStream engine per session on demand), and either prints the engine-free
// filesystem session list (--list) or hands the shared CLI runtime (internal/cli.Run) a
// thunk that opens/resumes the PERSISTED swarm session. It is wiring only: all runtime
// behavior (logging, signal teardown, the TUI) lives in internal/cli, and all
// session/persistence behavior lives in swarms/swe.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ciram-co/looprig-console/cli"
	"github.com/ciram-co/looprig-console/tui"
	"github.com/ciram-co/looprig/pkg/uuid"
	"github.com/ciram-co/swe/swarms/swe"
)

// bannerName is the SWE-Swarm's user-facing banner name shown in the TUI session-ready
// notice (passed through internal/cli.Banner).
const bannerName = "SWE"

// Process exit codes main returns via os.Exit. exitOK / exitRuntime mirror the runtime's
// codes; exitUsage is the boundary-failure code for a malformed invocation or a
// persistence/list failure (distinct from a TUI run error, which internal/cli.Run owns).
const (
	exitOK     = 0
	exitUsage  = 2
	exitFailed = 1
)

// cliFlags is the parsed CLI invocation: whether to list sessions and exit (--list),
// which session to resume (--resume <uuid>; zero = new session), whether to enable
// the untrusted, human-gated workspace skill source (--runtime-skills; off by default,
// §7a), and whether to show the optional UI-only startup greeting (--greeting; off by
// default, §5a). There is no positional agent name — swe is a single swarm.
type cliFlags struct {
	list          bool
	resume        uuid.UUID
	runtimeSkills bool
	greeting      bool
	purgeLegacy   bool
}

// FlagParseError reports a malformed CLI invocation (an unknown flag, a non-UUID --resume
// value, the mutually-exclusive --list + --resume combination, or an unexpected positional
// arg). It is a typed boundary error: untrusted CLI input is validated here, before any
// wiring runs, and is errors.As-recoverable.
type FlagParseError struct {
	Reason string
	Cause  error
}

func (e *FlagParseError) Error() string {
	if e.Cause != nil {
		return "swe: " + e.Reason + ": " + e.Cause.Error()
	}
	return "swe: " + e.Reason
}
func (e *FlagParseError) Unwrap() error { return e.Cause }

// parseFlags parses args (os.Args[1:]) into a cliFlags, validating every value at this
// boundary: --resume must be a canonical UUID (parsed via uuid.UnmarshalText, fail-closed),
// --list and --resume are mutually exclusive (a list-and-resume request is ambiguous), and
// no positional args are accepted (swe is a single swarm — there is no agent to name). It
// uses an isolated FlagSet (ContinueOnError, discarded output) so a bad flag returns a
// typed error rather than calling os.Exit, keeping main the single exit point and making
// the parser unit-testable.
func parseFlags(args []string) (cliFlags, error) {
	fs := flag.NewFlagSet("swe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		list          = fs.Bool("list", false, "list resumable sessions and exit")
		resume        = fs.String("resume", "", "resume the session with this id")
		runtimeSkills = fs.Bool("runtime-skills", false, "enable the untrusted, human-gated workspace skill source (.skills/) for read-only agents")
		greeting      = fs.Bool("greeting", false, "show a UI-only startup greeting listing the swarm's agents and skills")
		purgeLegacy   = fs.Bool("purge-legacy-sessions", false, "DESTRUCTIVE: permanently delete the pre-isolation shared StoreDir (~/.looprig/jetstream) and exit")
	)
	if err := fs.Parse(args); err != nil {
		return cliFlags{}, &FlagParseError{Reason: "invalid flags", Cause: err}
	}

	// swe takes no positional args: reject any so a typo'd flag (e.g. a bare "list"
	// instead of "-list") fails loud at the boundary rather than being silently ignored.
	if fs.NArg() > 0 {
		return cliFlags{}, &FlagParseError{Reason: "unexpected argument " + strconv.Quote(fs.Arg(0))}
	}

	out := cliFlags{list: *list, runtimeSkills: *runtimeSkills, greeting: *greeting, purgeLegacy: *purgeLegacy}

	// Detect whether --resume was explicitly given (vs left at its empty default): an
	// explicit --resume with an empty/whitespace value is a malformed invocation, rejected
	// at the boundary rather than silently treated as "no resume".
	var resumeGiven bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "resume" {
			resumeGiven = true
		}
	})
	if resumeGiven {
		v := strings.TrimSpace(*resume)
		if v == "" {
			return cliFlags{}, &FlagParseError{Reason: "--resume requires a session id"}
		}
		var id uuid.UUID
		if err := id.UnmarshalText([]byte(v)); err != nil {
			return cliFlags{}, &FlagParseError{Reason: "invalid --resume session id", Cause: err}
		}
		out.resume = id
	}

	if out.list && !out.resume.IsZero() {
		return cliFlags{}, &FlagParseError{Reason: "--list and --resume are mutually exclusive"}
	}
	// The destructive purge runs alone: it neither lists nor resumes, so combining it with
	// either is an ambiguous request rejected at the boundary.
	if out.purgeLegacy && (out.list || !out.resume.IsZero()) {
		return cliFlags{}, &FlagParseError{Reason: "--purge-legacy-sessions cannot be combined with --list or --resume"}
	}
	return out, nil
}

// listSessions prints the engine-free filesystem session list (id, status, last-active,
// title) to w, newest-updated first. It is the --list path: it reads the directory manifests
// only — no engine, no replay — so it is cheap and cannot contend a running session. A
// missing or corrupt manifest is shown as "metadata-invalid"; an empty store prints a
// friendly note rather than nothing.
func listSessions(factory *swe.SessionStoreFactory, w io.Writer) error {
	entries, err := factory.List()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(w, "no sessions yet")
		return nil
	}
	for _, e := range entries {
		if e.Err != nil {
			fmt.Fprintf(w, "%s  %s\n", e.Meta.ID, "metadata-invalid")
			continue
		}
		title := e.Meta.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(w, "%s  %-7s  %s  %s\n",
			e.Meta.ID, e.Meta.Status, e.Meta.UpdatedAt.Format(time.RFC3339), title)
	}
	return nil
}

// purgeLegacy deletes the pre-isolation shared StoreDir and reports the outcome to w. It
// prints the exact removed path ONLY after a successful deletion; an absent store is a
// no-op with a friendly note and no path. A symlinked or escaping legacy path is refused by
// the store (returned as an error), so the target is never followed. It opens no engine.
func purgeLegacy(factory *swe.SessionStoreFactory, w io.Writer) error {
	result, err := factory.PurgeLegacy()
	if err != nil {
		return err
	}
	if result.Removed {
		fmt.Fprintln(w, "removed legacy session store:", result.Path)
		return nil
	}
	fmt.Fprintln(w, "no legacy session store to remove")
	return nil
}

// openThunk builds the tui.OpenAgent the runtime drives. It returns a closure that opens a
// PERSISTED swarm session: the FIRST call honors resume (a non-zero id restores that
// session); every later call (a /clear reopen) starts a fresh NEW session, so /clear never
// re-restores the same id. The first-call latch is guarded so a reopen is deterministically
// a new session. cfg carries the human-set construction modes (RuntimeSkills) and applies to
// every open, including a /clear reopen (the launch flag holds for the whole process). Each
// open mints (or, on the first call, resumes) a session over its OWN isolated engine, so a
// /clear reopen's new session never shares a StoreDir with the one it replaces. The returned
// thunk yields a tui.Agent (the persisted *sessionAgent satisfies it).
func openThunk(factory *swe.SessionStoreFactory, resume uuid.UUID, cfg swe.Config) tui.OpenAgent {
	var opened bool
	return func(c context.Context) (tui.Agent, error) {
		sel := swe.SessionSelector{}
		if !opened {
			sel.Resume = resume // only the first open resumes; /clear reopens start fresh
		}
		opened = true
		return factory.Open(c, sel, cfg)
	}
}

// run is the testable composition root: it parses flags, opens the session-store factory,
// handles --list (print + exit) or builds the persisted openThunk and delegates to
// internal/cli.Run. It returns a process exit code and never calls os.Exit, so main stays
// the single exit point. ctx is the process root (signal-aware); out/errOut are the list
// + error sinks.
func run(ctx context.Context, args []string, out, errOut io.Writer) int {
	flags, ferr := parseFlags(args)
	if ferr != nil {
		fmt.Fprintln(errOut, ferr)
		return exitUsage
	}

	// Open the session-store factory: the session-scoped composition root that owns the
	// confined session store and opens one isolated embedded engine per session on demand
	// (each agent closes its own engine on teardown). A failure to open the store fails loud
	// — persistence is the point.
	factory, perr := swe.NewSessionStoreFactory()
	if perr != nil {
		fmt.Fprintln(errOut, "persistence:", perr)
		return exitFailed
	}

	// --purge-legacy-sessions: delete the pre-isolation shared StoreDir and exit. It is
	// destructive but confined (the store derives + containment-checks the path) and opens no
	// engine. Handled before --list because the two are mutually exclusive at the boundary.
	if flags.purgeLegacy {
		if err := purgeLegacy(factory, out); err != nil {
			fmt.Fprintln(errOut, "purge:", err)
			return exitFailed
		}
		return exitOK
	}

	// --list: print the engine-free filesystem session list and exit (no TUI, no engine). It
	// reads only the directory manifests, so it is cheap even with many sessions.
	if flags.list {
		if err := listSessions(factory, out); err != nil {
			fmt.Fprintln(errOut, "list:", err)
			return exitFailed
		}
		return exitOK
	}

	// The initial open honors --resume; every /clear reopen starts a FRESH persisted
	// session over a fresh isolated engine. The --runtime-skills and --greeting modes apply
	// to every open. The startup greeting (§5a) is built ONCE here from the registry
	// (deterministic, no LLM call) — empty unless --greeting is set — and handed to the TUI
	// as an opening transcript entry via the Banner; it is NOT a turn, NOT a command, and
	// never enters the model's context. internal/cli.Run owns logging, signal teardown, the
	// TUI, and bounded Close.
	cfg := swe.Config{RuntimeSkills: flags.runtimeSkills, Greeting: flags.greeting}
	open := openThunk(factory, flags.resume, cfg)
	return cli.Run(ctx, open, cli.Banner{Name: bannerName, Greeting: swe.Greeting(cfg)})
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
