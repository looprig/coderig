// Command swe is the SWE-Swarm TUI entry point and composition root. It parses the CLI
// invocation (--list / --resume / --data-dir), opens the session-store factory (one on-disk
// fsstore-backed session store shared by every session), and either prints the session list
// (--list) or hands the shared CLI runtime (cli.Run) a thunk that opens/resumes the PERSISTED
// swarm session. It is wiring only: all runtime behavior (logging, signal teardown, the TUI)
// lives in cli, and all session/persistence behavior lives in swarms/swe.
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

	"github.com/looprig/cli/cli"
	"github.com/looprig/cli/tui"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/sandbox"
	"github.com/looprig/swe/swarms/swe"
)

// bannerName is the SWE-Swarm's user-facing banner name shown in the TUI session-ready
// notice (passed through cli.Banner).
const bannerName = "SWE"

// Process exit codes main returns via os.Exit. exitOK / exitRuntime mirror the runtime's
// codes; exitUsage is the boundary-failure code for a malformed invocation or a
// persistence/list failure (distinct from a TUI run error, which cli.Run owns).
const (
	exitOK     = 0
	exitUsage  = 2
	exitFailed = 1
)

// cliFlags is the parsed CLI invocation: whether to list sessions and exit (--list), which
// session to resume (--resume <uuid>; zero = new session), whether to enable the untrusted,
// human-gated workspace skill source (--runtime-skills; off by default, §7a), whether to show
// the optional UI-only startup greeting (--greeting; off by default, §5a), the session
// store root (--data-dir; empty = the ~/.looprig/store default), and the session security
// ceiling (--security-mode; default write, §8). There is no positional agent name — swe is a
// single swarm.
type cliFlags struct {
	list          bool
	resume        uuid.UUID
	runtimeSkills bool
	greeting      bool
	dataDir       string
	// securityCeiling is the session security-mode ceiling ordinal (min(role, this) is
	// each leaf's effective mode; see swe.Config.SecurityCeiling). Parsed from the
	// --security-mode NAME flag, defaulting to swe.DefaultSecurityMode (Write).
	securityCeiling uint8
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
		dataDir       = fs.String("data-dir", "", "session store root (default ~/.looprig/store)")
		securityMode  = fs.String("security-mode", "write", "session security ceiling: zerotrust|readonly|write|trusted (caps per-role auto-approval)")
	)
	if err := fs.Parse(args); err != nil {
		return cliFlags{}, &FlagParseError{Reason: "invalid flags", Cause: err}
	}

	// swe takes no positional args: reject any so a typo'd flag (e.g. a bare "list"
	// instead of "-list") fails loud at the boundary rather than being silently ignored.
	if fs.NArg() > 0 {
		return cliFlags{}, &FlagParseError{Reason: "unexpected argument " + strconv.Quote(fs.Arg(0))}
	}

	// Validate the security-mode name at this boundary (untrusted CLI input): an unknown
	// mode fails closed rather than silently defaulting to a surprising permissiveness.
	ceilingOrd, ok := swe.ParseSecurityMode(strings.ToLower(strings.TrimSpace(*securityMode)))
	if !ok {
		return cliFlags{}, &FlagParseError{Reason: "invalid --security-mode " + strconv.Quote(*securityMode) + " (want zerotrust|readonly|write|trusted)"}
	}

	out := cliFlags{list: *list, runtimeSkills: *runtimeSkills, greeting: *greeting, dataDir: strings.TrimSpace(*dataDir), securityCeiling: ceilingOrd}

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
	return out, nil
}

// listSessions prints the session list (id, status, last-active, title) to w, from the store's
// listing catalog (most-recently-active first). It is the --list path: it reads the listing
// index only — no session lease, no replay — so it is cheap and cannot contend a running
// session. The catalog returns a single error (not per-entry), and an empty store prints a
// friendly note rather than nothing.
func listSessions(ctx context.Context, factory *swe.SessionStoreFactory, w io.Writer) error {
	metas, err := factory.List(ctx)
	if err != nil {
		return err
	}
	return printSessions(w, metas)
}

// printSessions renders the session rows (id, status, last-active, title) to w in the order
// given (the catalog's own most-recently-active-first ordering — the CLI does not re-sort). An
// empty list prints a friendly note; an untitled session shows "(untitled)". It is pure
// formatting, unit-testable without a store.
func printSessions(w io.Writer, metas []sessionstore.SessionMeta) error {
	if len(metas) == 0 {
		fmt.Fprintln(w, "no sessions yet")
		return nil
	}
	for _, m := range metas {
		title := m.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(w, "%s  %-7s  %s  %s\n",
			m.SessionID, m.Status, m.LastActiveAt.Format(time.RFC3339), title)
	}
	return nil
}

// openThunk builds the tui.OpenAgent the runtime drives. It returns a closure that opens a
// PERSISTED swarm session: the FIRST call honors resume (a non-zero id restores that
// session); every later call (a /clear reopen) starts a fresh NEW session, so /clear never
// re-restores the same id. The first-call latch is guarded so a reopen is deterministically
// a new session. cfg carries the human-set construction modes (RuntimeSkills) and applies to
// every open, including a /clear reopen (the launch flag holds for the whole process). Every
// open (or, on the first call, resume) addresses its session by name in the SHARED store, so a
// /clear reopen's new session is independent of the one it replaces. The returned thunk yields
// a tui.Agent (the persisted *sessionAgent satisfies it).
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

// run is the testable composition root: it parses flags, resolves the store root, opens the
// session-store factory (closed once on return), handles --list (print + exit) or builds the
// persisted openThunk and delegates to cli.Run. It returns a process exit code and never calls
// os.Exit, so main stays the single exit point. ctx is the process root (signal-aware);
// out/errOut are the list + error sinks.
func run(ctx context.Context, args []string, out, errOut io.Writer) int {
	flags, ferr := parseFlags(args)
	if ferr != nil {
		fmt.Fprintln(errOut, ferr)
		return exitUsage
	}

	// Resolve the store root: the explicit --data-dir, or the ~/.looprig/store default. A home
	// directory that cannot be resolved fails loud rather than falling back to a surprising path.
	dataDir := flags.dataDir
	if dataDir == "" {
		dd, derr := swe.DefaultDataDir()
		if derr != nil {
			fmt.Fprintln(errOut, "persistence:", derr)
			return exitFailed
		}
		dataDir = dd
	}

	// Open the session-store factory: the process-level composition root that owns the single
	// on-disk store shared by every session. A failure to open it fails loud — persistence is
	// the point. It is closed once here on return, after cli.Run (and every session it opened)
	// finishes.
	factory, perr := swe.NewSessionStoreFactory(dataDir)
	if perr != nil {
		fmt.Fprintln(errOut, "persistence:", perr)
		return exitFailed
	}
	defer func() { _ = factory.Close() }()

	// --list: print the session list and exit (no TUI). It reads only the listing catalog, so
	// it is cheap even with many sessions.
	if flags.list {
		if err := listSessions(ctx, factory, out); err != nil {
			fmt.Fprintln(errOut, "list:", err)
			return exitFailed
		}
		return exitOK
	}

	// The initial open honors --resume; every /clear reopen starts a FRESH persisted session.
	// The --runtime-skills and --greeting modes apply to every open. The startup greeting (§5a)
	// is built ONCE here from the registry (deterministic, no LLM call) — empty unless --greeting
	// is set — and handed to the TUI as an opening transcript entry via the Banner; it is NOT a
	// turn, NOT a command, and never enters the model's context. cli.Run owns logging, signal
	// teardown, the TUI, and bounded Close.
	cfg := swe.Config{RuntimeSkills: flags.runtimeSkills, Greeting: flags.greeting, SecurityCeiling: flags.securityCeiling}
	open := openThunk(factory, flags.resume, cfg)
	return cli.Run(ctx, open, cli.Banner{Name: bannerName, Greeting: swe.Greeting(cfg)})
}

func main() {
	// MUST be the FIRST line of main() (SPEC §6): a no-op on darwin, but on Linux it
	// re-executes the process as the stage-2 sandbox helper before any other goroutine,
	// fd, or thread state exists. Wiring it from day one means no retrofit later.
	sandbox.Init()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
