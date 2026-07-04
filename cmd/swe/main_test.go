package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/sessionstore"
)

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
