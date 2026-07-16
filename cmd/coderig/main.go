// Command coderig is the CodeRig TUI entry point and composition root. It parses the CLI
// invocation (--list / --resume / --data-dir), opens the session-store factory (one on-disk
// fsstore-backed session store shared by every session), and either prints the session list
// (--list) or hands the shared TUI runtime (runtime.Run) a thunk that opens/resumes the PERSISTED
// swarm session. It is wiring only: all runtime behavior (logging, signal teardown, the TUI)
// lives in tui, and all Session/persistence behavior lives in the internal app package.
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

	coderig "github.com/looprig/coderig/internal/app"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/sandbox"
	"github.com/looprig/tui"
	"github.com/looprig/tui/runtime"
)

// bannerName is the CodeRig's user-facing banner name shown in the TUI session-ready
// notice (passed through runtime.Banner).
const bannerName = "CodeRig"

// Process exit codes main returns via os.Exit. exitOK / exitRuntime mirror the runtime's
// codes; exitUsage is the boundary-failure code for a malformed invocation or a
// persistence/list failure (distinct from a TUI run error, which runtime.Run owns).
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
// security limit (--security-mode; default write, §8). There is no positional agent name because
// CodeRig is one fixed Rig.
type cliFlags struct {
	list          bool
	resume        uuid.UUID
	runtimeSkills bool
	greeting      bool
	dataDir       string
	// securityLimit is the Session security-mode limit ordinal (min(role, this) is
	// each leaf's effective mode; see coderig.Config.SecurityLimit). Parsed from the
	// --security-mode NAME flag, defaulting to coderig.DefaultSecurityMode (Write).
	securityLimit uint8
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
		return "coderig: " + e.Reason + ": " + e.Cause.Error()
	}
	return "coderig: " + e.Reason
}
func (e *FlagParseError) Unwrap() error { return e.Cause }

// parseFlags parses args (os.Args[1:]) into a cliFlags, validating every value at this
// boundary: --resume must be a canonical UUID (parsed via uuid.UnmarshalText, fail-closed),
// --list and --resume are mutually exclusive (a list-and-resume request is ambiguous), and
// no positional args are accepted because CodeRig is one fixed Rig. It
// uses an isolated FlagSet (ContinueOnError, discarded output) so a bad flag returns a
// typed error rather than calling os.Exit, keeping main the single exit point and making
// the parser unit-testable.
func parseFlags(args []string) (cliFlags, error) {
	fs := flag.NewFlagSet("coderig", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		list          = fs.Bool("list", false, "list resumable sessions and exit")
		resume        = fs.String("resume", "", "resume the session with this id")
		runtimeSkills = fs.Bool("runtime-skills", false, "enable the untrusted, human-gated workspace skill source (.skills/) for read-only agents")
		greeting      = fs.Bool("greeting", false, "show a UI-only startup greeting listing the swarm's agents and skills")
		dataDir       = fs.String("data-dir", "", "session store root (default ~/.looprig/store)")
		securityMode  = fs.String("security-mode", "write", "session security limit: zerotrust|readonly|write|trusted (caps per-role auto-approval)")
	)
	if err := fs.Parse(args); err != nil {
		return cliFlags{}, &FlagParseError{Reason: "invalid flags", Cause: err}
	}

	// CodeRig takes no positional args: reject any so a typo'd flag (e.g. a bare "list"
	// instead of "-list") fails loud at the boundary rather than being silently ignored.
	if fs.NArg() > 0 {
		return cliFlags{}, &FlagParseError{Reason: "unexpected argument " + strconv.Quote(fs.Arg(0))}
	}

	// Validate the security-mode name at this boundary (untrusted CLI input): an unknown
	// mode fails closed rather than silently defaulting to a surprising permissiveness.
	securityLimitOrd, ok := coderig.ParseSecurityMode(strings.ToLower(strings.TrimSpace(*securityMode)))
	if !ok {
		return cliFlags{}, &FlagParseError{Reason: "invalid --security-mode " + strconv.Quote(*securityMode) + " (want zerotrust|readonly|write|trusted)"}
	}

	out := cliFlags{list: *list, runtimeSkills: *runtimeSkills, greeting: *greeting, dataDir: strings.TrimSpace(*dataDir), securityLimit: securityLimitOrd}

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
func listSessions(ctx context.Context, factory *coderig.SessionStoreFactory, w io.Writer) error {
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

// sessionOpen is the SessionStoreFactory.Open-shaped process composition seam. Production
// binds it directly to the shared factory; tests can observe selector decisions without
// opening an on-disk store.
type sessionOpen func(context.Context, coderig.SessionSelector, coderig.Config) (tui.Agent, error)

// openThunk builds the tui.OpenAgent the runtime drives. It returns a closure that opens a
// PERSISTED swarm session: the FIRST call honors resume (a non-zero id restores that
// session); every later call (a /clear reopen) starts a fresh NEW session, so /clear never
// re-restores the same id. The CLI serializes lifecycle handoff by closing the live session
// before invoking this opener for /clear. cfg carries the human-set construction modes
// (RuntimeSkills) and applies to
// every open, including a /clear reopen (the launch flag holds for the whole process). Every
// open (or, on the first call, resume) addresses its session by name in the SHARED store, so a
// /clear reopen's new session is independent of the one it replaces. The returned thunk yields
// a tui.Agent (the persisted session adapter exposes current active selection and independent
// focused-loop routing for the CLI).
func openThunk(openSession sessionOpen, resume uuid.UUID, cfg coderig.Config) tui.OpenAgent {
	var opened bool
	return func(c context.Context) (tui.Agent, error) {
		sel := coderig.SessionSelector{}
		if !opened {
			sel.Resume = resume // only the first open resumes; /clear reopens start fresh
		}
		opened = true
		return openSession(c, sel, cfg)
	}
}

// cliRunner is the runtime.Run-shaped runtime seam used to prove process ownership order.
type cliRunner func(context.Context, tui.OpenAgent, runtime.Banner) int

// runCLIWithStore runs the CLI while the shared session store is live. runtime.Run closes its
// current session before returning; the store close therefore always happens after session
// shutdown, including runtime-error exits. A store teardown error maps to process failure.
func runCLIWithStore(ctx context.Context, open tui.OpenAgent, banner runtime.Banner, runCLI cliRunner, closeStore func() error) int {
	exit := runCLI(ctx, open, banner)
	if err := closeStore(); err != nil {
		return exitFailed
	}
	return exit
}

// run is the testable composition root: it parses flags, resolves the store root, opens the
// session-store factory (closed once on return), handles --list (print + exit) or builds the
// persisted openThunk and delegates to runtime.Run. It returns a process exit code and never calls
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
		dd, derr := coderig.DefaultDataDir()
		if derr != nil {
			fmt.Fprintln(errOut, "persistence:", derr)
			return exitFailed
		}
		dataDir = dd
	}

	// Open the session-store factory: the process-level composition root that owns the single
	// on-disk store shared by every session. A failure to open it fails loud — persistence is
	// the point. It is closed once here on return, after runtime.Run (and every session it opened)
	// finishes.
	factory, perr := coderig.NewSessionStoreFactory(dataDir)
	if perr != nil {
		fmt.Fprintln(errOut, "persistence:", perr)
		return exitFailed
	}
	// --list: print the session list and exit (no TUI). It reads only the listing catalog, so
	// it is cheap even with many sessions.
	if flags.list {
		if err := listSessions(ctx, factory, out); err != nil {
			_ = factory.Close()
			fmt.Fprintln(errOut, "list:", err)
			return exitFailed
		}
		if err := factory.Close(); err != nil {
			fmt.Fprintln(errOut, "persistence close:", err)
			return exitFailed
		}
		return exitOK
	}

	// The initial open honors --resume; every /clear reopen starts a FRESH persisted session.
	// The --runtime-skills and --greeting modes apply to every open. The startup greeting (§5a)
	// is built once from the fixed definitions (deterministic, no LLM call) and is empty unless --greeting
	// is set — and handed to the TUI as an opening transcript entry via the Banner; it is NOT a
	// turn, NOT a command, and never enters the model's context. runtime.Run owns logging, signal
	// teardown, the TUI, and bounded Close.
	cfg := coderig.Config{RuntimeSkills: flags.runtimeSkills, Greeting: flags.greeting, SecurityLimit: flags.securityLimit}
	open := openThunk(func(ctx context.Context, sel coderig.SessionSelector, cfg coderig.Config) (tui.Agent, error) {
		return factory.Open(ctx, sel, cfg)
	}, flags.resume, cfg)
	runCLI := func(ctx context.Context, open tui.OpenAgent, banner runtime.Banner) int {
		return runtime.Run(ctx, open, banner, tui.WithSessionBrowser(factory.SessionBrowser(cfg)))
	}
	return runCLIWithStore(ctx, open, runtime.Banner{Name: bannerName, Greeting: coderig.Greeting(cfg)}, runCLI, factory.Close)
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
