package main

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/cli/cli"
	"github.com/looprig/cli/tui"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/swe/swarms/swe"
)

// TestOpenThunkSelectsResumeOnlyOnce proves the process opener restores only its first
// session and all later opens (including concurrent /clear reopens) select a fresh session.
// The function-shaped seam keeps SessionStoreFactory as the production owner while making
// the NewSession/RestoreSession selector decision observable without opening a real store.
func TestOpenThunkSelectsResumeOnlyOnce(t *testing.T) {
	resume := mustUUID(t)
	const opens = 16
	var (
		mu        sync.Mutex
		selectors []swe.SessionSelector
	)
	openSession := func(_ context.Context, sel swe.SessionSelector, _ swe.Config) (tui.Agent, error) {
		mu.Lock()
		selectors = append(selectors, sel)
		mu.Unlock()
		return nil, nil
	}
	open := openThunk(openSession, resume, swe.Config{})

	var wg sync.WaitGroup
	for range opens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := open(context.Background()); err != nil {
				t.Errorf("open: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(selectors) != opens {
		t.Fatalf("opens = %d, want %d", len(selectors), opens)
	}
	var restores int
	for _, sel := range selectors {
		if sel.Resume == resume {
			restores++
		} else if !sel.Resume.IsZero() {
			t.Errorf("unexpected resume selector %v", sel.Resume)
		}
	}
	if restores != 1 {
		t.Errorf("restore selections = %d, want exactly 1; all /clear opens must be new", restores)
	}
}

// TestOpenThunkSelectsNewForLaunchAndClear covers the no-resume launch explicitly: both the
// initial open and the later /clear reopen carry a zero selector, which SessionStoreFactory
// maps to Rig.NewSession.
func TestOpenThunkSelectsNewForLaunchAndClear(t *testing.T) {
	var selectors []swe.SessionSelector
	openSession := func(_ context.Context, sel swe.SessionSelector, _ swe.Config) (tui.Agent, error) {
		selectors = append(selectors, sel)
		return nil, nil
	}
	open := openThunk(openSession, uuid.UUID{}, swe.Config{})
	for i := 0; i < 2; i++ {
		if _, err := open(context.Background()); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	for i, sel := range selectors {
		if !sel.Resume.IsZero() {
			t.Errorf("selector %d resume = %v, want zero for NewSession", i, sel.Resume)
		}
	}
}

// TestRunCLIClosesSessionBeforeStore pins the process ownership order: the CLI runtime
// closes the live session before it returns, then the composition root closes the shared
// SessionStoreFactory. This remains true for a non-zero runtime exit code.
func TestRunCLIClosesSessionBeforeStore(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	record := func(step string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, step)
	}
	agent := &orderingAgent{close: func() { record("session") }}
	open := func(context.Context) (tui.Agent, error) { return agent, nil }
	runner := func(ctx context.Context, open tui.OpenAgent, _ cli.Banner) int {
		live, err := open(ctx)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := live.Close(ctx); err != nil {
			t.Fatalf("close live session: %v", err)
		}
		return exitFailed
	}
	closeStore := func() error { record("store"); return nil }

	if got := runCLIWithStore(context.Background(), open, cli.Banner{Name: bannerName}, runner, closeStore); got != exitFailed {
		t.Fatalf("exit = %d, want %d", got, exitFailed)
	}
	if got, want := strings.Join(order, ","), "session,store"; got != want {
		t.Fatalf("shutdown order = %q, want %q", got, want)
	}
}

// orderingAgent is the smallest complete migrated CLI contract used by the process-order
// test. The explicit Root/Active/loop-targeted image methods keep this command package
// compiled against the same multi-loop surface as the production session adapter.
type orderingAgent struct{ close func() }

func (*orderingAgent) Submit(context.Context, []content.Block) (uuid.UUID, error) {
	return uuid.UUID{}, nil
}
func (*orderingAgent) SubmitToLoop(context.Context, uuid.UUID, []content.Block) (uuid.UUID, error) {
	return uuid.UUID{}, nil
}
func (*orderingAgent) RootLoopID() uuid.UUID                                   { return uuid.UUID{} }
func (*orderingAgent) ActiveLoopID() uuid.UUID                                 { return uuid.UUID{} }
func (*orderingAgent) Interrupt(context.Context) (bool, error)                 { return false, nil }
func (a *orderingAgent) Close(context.Context) error                           { a.close(); return nil }
func (*orderingAgent) AcceptsImages(uuid.UUID) bool                            { return false }
func (*orderingAgent) Subscribe(event.EventFilter) (event.Subscription, error) { return nil, nil }
func (*orderingAgent) ReplayBacklog(context.Context) ([]event.Event, error)    { return nil, nil }
func (*orderingAgent) Approve(context.Context, uuid.UUID, uuid.UUID, tool.ApprovalScope) error {
	return nil
}
func (*orderingAgent) Deny(context.Context, uuid.UUID, uuid.UUID) error                  { return nil }
func (*orderingAgent) ProvideAnswer(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }

var _ tui.Agent = (*orderingAgent)(nil)

// TestRunPreservesPublicIdentity pins the process-facing name independently from the rig's
// internal operator-primary topology key.
func TestRunPreservesPublicIdentity(t *testing.T) {
	if bannerName != "SWE" {
		t.Errorf("bannerName = %q, want %q", bannerName, "SWE")
	}
	greeting := swe.Greeting(swe.Config{Greeting: true})
	if !strings.Contains(greeting, "operator") {
		t.Fatalf("greeting missing public operator identity:\n%s", greeting)
	}
	if strings.Contains(greeting, "operator-primary") {
		t.Fatalf("greeting leaked internal operator-primary topology key:\n%s", greeting)
	}
}

// TestRunHasNoSWEServeAdapter guards the Task 6 process boundary: the command and swarm
// packages do not directly import the generic harness HTTP layer. A future HTTP entry point
// should pass the real rig to serve.Handler instead of adding a SWE Runner adapter here.
func TestRunHasNoSWEServeAdapter(t *testing.T) {
	cmd := exec.Command("go", "list", "-f", `{{join .Imports "\n"}}`, "./cmd/swe", "./swarms/swe")
	cmd.Dir = "../.."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list direct imports: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "github.com/looprig/harness/pkg/serve") {
		t.Fatalf("SWE process packages directly import serve; no SWE serve adapter belongs in this migration:\n%s", out)
	}
}

// TestParseFlags covers the SWE CLI flag parser: --list, --resume <uuid>, --runtime-skills,
// --greeting, --data-dir, and the boundary validation (an invalid/empty resume id fails at the
// boundary, not deep in the wiring; --list and --resume are mutually exclusive). The swarm has
// no positional agent name (it is a single swarm), so an unexpected positional arg is rejected.
func TestParseFlags(t *testing.T) {
	t.Parallel()

	validID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	tests := []struct {
		name              string
		args              []string
		wantList          bool
		wantResume        uuid.UUID
		wantRuntimeSkills bool
		wantGreeting      bool
		wantDataDir       string
		wantErr           bool
	}{
		{name: "no flags → new session", args: nil},
		{name: "list flag", args: []string{"-list"}, wantList: true},
		{name: "list flag double dash", args: []string{"--list"}, wantList: true},
		{name: "resume a session", args: []string{"-resume", validID.String()}, wantResume: validID},
		{name: "resume double dash", args: []string{"--resume", validID.String()}, wantResume: validID},
		{name: "runtime-skills off by default", args: nil, wantRuntimeSkills: false},
		{name: "runtime-skills flag", args: []string{"-runtime-skills"}, wantRuntimeSkills: true},
		{name: "runtime-skills flag double dash", args: []string{"--runtime-skills"}, wantRuntimeSkills: true},
		{name: "runtime-skills with resume", args: []string{"-runtime-skills", "-resume", validID.String()}, wantResume: validID, wantRuntimeSkills: true},
		{name: "greeting off by default", args: nil, wantGreeting: false},
		{name: "greeting flag", args: []string{"-greeting"}, wantGreeting: true},
		{name: "greeting flag double dash", args: []string{"--greeting"}, wantGreeting: true},
		{name: "greeting with resume", args: []string{"-greeting", "-resume", validID.String()}, wantResume: validID, wantGreeting: true},
		{name: "data-dir default empty", args: nil, wantDataDir: ""},
		{name: "data-dir flag", args: []string{"-data-dir", "/tmp/swe-store"}, wantDataDir: "/tmp/swe-store"},
		{name: "data-dir double dash", args: []string{"--data-dir", "/tmp/swe-store"}, wantDataDir: "/tmp/swe-store"},
		{name: "data-dir whitespace trimmed to empty", args: []string{"-data-dir", "   "}, wantDataDir: ""},
		{name: "data-dir with resume", args: []string{"-data-dir", "/tmp/s", "-resume", validID.String()}, wantResume: validID, wantDataDir: "/tmp/s"},
		{name: "invalid resume id rejected", args: []string{"-resume", "not-a-uuid"}, wantErr: true},
		{name: "empty resume id rejected", args: []string{"-resume", ""}, wantErr: true},
		{name: "list and resume are mutually exclusive", args: []string{"-list", "-resume", validID.String()}, wantErr: true},
		{name: "unknown flag rejected", args: []string{"-nope"}, wantErr: true},
		{name: "unexpected positional rejected", args: []string{"extra"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseFlags(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseFlags(%v) err = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.list != tt.wantList {
				t.Errorf("list = %v, want %v", got.list, tt.wantList)
			}
			if got.resume != tt.wantResume {
				t.Errorf("resume = %v, want %v", got.resume, tt.wantResume)
			}
			if got.runtimeSkills != tt.wantRuntimeSkills {
				t.Errorf("runtimeSkills = %v, want %v", got.runtimeSkills, tt.wantRuntimeSkills)
			}
			if got.greeting != tt.wantGreeting {
				t.Errorf("greeting = %v, want %v", got.greeting, tt.wantGreeting)
			}
			if got.dataDir != tt.wantDataDir {
				t.Errorf("dataDir = %q, want %q", got.dataDir, tt.wantDataDir)
			}
		})
	}
}

// TestFlagParseErrorIsTyped proves FlagParseError carries its reason and unwraps its cause,
// so the boundary failure is errors.As-recoverable rather than a bare string.
func TestFlagParseErrorIsTyped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       *FlagParseError
		wantMsg   string
		wantCause bool
	}{
		{name: "reason only", err: &FlagParseError{Reason: "boom"}, wantMsg: "swe: boom"},
		{
			name:      "reason with cause",
			err:       &FlagParseError{Reason: "bad id", Cause: errStub{}},
			wantMsg:   "swe: bad id: stub",
			wantCause: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.wantMsg {
				t.Errorf("Error() = %q, want %q", got, tt.wantMsg)
			}
			if (tt.err.Unwrap() != nil) != tt.wantCause {
				t.Errorf("Unwrap() non-nil = %v, want %v", tt.err.Unwrap() != nil, tt.wantCause)
			}
		})
	}
}

// errStub is a minimal error for the cause-chaining assertion.
type errStub struct{}

func (errStub) Error() string { return "stub" }

// TestPrintSessions proves the --list formatter renders each catalog row (id, status,
// last-active, title) in the order given, shows "(untitled)" for a title-less session, and
// prints a friendly note for an empty store. Ordering is the catalog's responsibility; the CLI
// prints in the order it receives.
func TestPrintSessions(t *testing.T) {
	t.Parallel()

	newer := mustUUID(t)
	older := mustUUID(t)
	untitled := mustUUID(t)
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		metas     []sessionstore.SessionMeta
		wantNote  string   // exact single-line note (empty store)
		wantOrder []string // substrings that must appear, in this order
		wantParts []string // substrings that must be present
	}{
		{
			name:     "empty store prints a friendly note",
			metas:    nil,
			wantNote: "no sessions yet\n",
		},
		{
			name: "rows render newest-first in the order given",
			metas: []sessionstore.SessionMeta{
				{SessionID: newer, Title: "newer work", Status: sessionstore.StatusActive, LastActiveAt: now},
				{SessionID: older, Title: "older work", Status: sessionstore.StatusStopped, LastActiveAt: now.Add(-time.Hour)},
			},
			wantOrder: []string{newer.String(), older.String()},
			wantParts: []string{"newer work", "older work", "active", "stopped", now.Format(time.RFC3339)},
		},
		{
			name: "untitled session shows a placeholder",
			metas: []sessionstore.SessionMeta{
				{SessionID: untitled, Status: sessionstore.StatusActive, LastActiveAt: now},
			},
			wantParts: []string{untitled.String(), "(untitled)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := printSessions(&buf, tt.metas); err != nil {
				t.Fatalf("printSessions: %v", err)
			}
			out := buf.String()
			if tt.wantNote != "" && out != tt.wantNote {
				t.Fatalf("printSessions(empty) = %q, want %q", out, tt.wantNote)
			}
			for _, part := range tt.wantParts {
				if !strings.Contains(out, part) {
					t.Errorf("output missing %q:\n%s", part, out)
				}
			}
			prev := -1
			for _, want := range tt.wantOrder {
				i := strings.Index(out, want)
				if i < 0 {
					t.Fatalf("output missing ordered token %q:\n%s", want, out)
				}
				if i < prev {
					t.Errorf("token %q out of order:\n%s", want, out)
				}
				prev = i
			}
		})
	}
}

// mustUUID mints a random UUID for a test row or fails the test.
func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return id
}
