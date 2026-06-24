package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/persistence"
	"github.com/ciram-co/looprig/pkg/uuid"
	"github.com/ciram-co/swe/swarms/swe"
)

// TestParseFlags covers the SWE CLI flag parser: --list, --resume <uuid>, and the boundary
// validation (an invalid/empty resume id fails at the boundary, not deep in the wiring;
// --list and --resume are mutually exclusive). The swarm has no positional agent name (it
// is a single swarm), so an unexpected positional arg is rejected.
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

// TestParseFlagsPurgeLegacy covers the destructive --purge-legacy-sessions flag: it parses
// on its own but is mutually exclusive with --list and --resume (a list/resume-and-purge
// request is ambiguous and must fail at the boundary).
func TestParseFlagsPurgeLegacy(t *testing.T) {
	t.Parallel()

	validID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	tests := []struct {
		name      string
		args      []string
		wantPurge bool
		wantErr   bool
	}{
		{name: "no purge by default", args: nil, wantPurge: false},
		{name: "purge alone", args: []string{"-purge-legacy-sessions"}, wantPurge: true},
		{name: "purge double dash", args: []string{"--purge-legacy-sessions"}, wantPurge: true},
		{name: "purge with list rejected", args: []string{"-purge-legacy-sessions", "-list"}, wantErr: true},
		{name: "purge with resume rejected", args: []string{"-purge-legacy-sessions", "-resume", validID.String()}, wantErr: true},
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
			if got.purgeLegacy != tt.wantPurge {
				t.Errorf("purgeLegacy = %v, want %v", got.purgeLegacy, tt.wantPurge)
			}
		})
	}
}

// seedSession writes a titled, active manifest for a fresh session under root and returns
// its id, so the list tests have deterministic, sortable rows.
func seedSession(t *testing.T, root *persistence.SessionStoreRoot, title string, now time.Time) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	store, err := root.OpenSessionMeta(id)
	if err != nil {
		t.Fatalf("OpenSessionMeta: %v", err)
	}
	if _, err := store.Init(now); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := store.SetTitle(title, persistence.TitleSourceGenerated, now); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	return id
}

// TestListSessionsOutput proves --list prints the filesystem manifests newest-updated first
// and renders a missing/corrupt manifest as metadata-invalid — with no engine startup.
func TestListSessionsOutput(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)

	root, err := persistence.OpenSessionStoreRoot()
	if err != nil {
		t.Fatalf("OpenSessionStoreRoot: %v", err)
	}
	older := seedSession(t, root, "older work", time.Now().Add(-time.Hour))
	newer := seedSession(t, root, "newer work", time.Now())

	missing, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	if _, err := root.CreateSessionDir(missing); err != nil {
		t.Fatalf("CreateSessionDir(missing): %v", err)
	}

	factory, err := swe.NewSessionStoreFactory()
	if err != nil {
		t.Fatalf("NewSessionStoreFactory: %v", err)
	}

	var buf bytes.Buffer
	if err := listSessions(factory, &buf); err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	out := buf.String()

	// Newest-updated first.
	iNewer := strings.Index(out, newer.String())
	iOlder := strings.Index(out, older.String())
	if iNewer < 0 || iOlder < 0 {
		t.Fatalf("listing missing a session id:\n%s", out)
	}
	if iNewer > iOlder {
		t.Errorf("newer session listed after older:\n%s", out)
	}
	// The missing manifest is rendered as metadata-invalid.
	if !strings.Contains(out, missing.String()) || !strings.Contains(out, "metadata-invalid") {
		t.Errorf("missing-manifest session not shown as metadata-invalid:\n%s", out)
	}
}

// TestListSessionsEmpty proves an empty store prints a friendly note rather than nothing.
func TestListSessionsEmpty(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	factory, err := swe.NewSessionStoreFactory()
	if err != nil {
		t.Fatalf("NewSessionStoreFactory: %v", err)
	}
	var buf bytes.Buffer
	if err := listSessions(factory, &buf); err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	if !strings.Contains(buf.String(), "no sessions") {
		t.Errorf("empty store output = %q, want a friendly note", buf.String())
	}
}

// TestPurgeLegacyOutput covers the destructive purge: a present legacy store is removed and
// its exact path printed, an absent store is a no-op with no path, and a symlinked legacy
// path is refused (its target never followed).
func TestPurgeLegacyOutput(t *testing.T) {
	t.Run("removes legacy store and prints path", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_DATA_HOME", xdg)
		legacy := filepath.Join(xdg, "looprig", "jetstream")
		if err := os.MkdirAll(legacy, 0o700); err != nil {
			t.Fatalf("seed legacy: %v", err)
		}

		factory, err := swe.NewSessionStoreFactory()
		if err != nil {
			t.Fatalf("NewSessionStoreFactory: %v", err)
		}
		var buf bytes.Buffer
		if err := purgeLegacy(factory, &buf); err != nil {
			t.Fatalf("purgeLegacy: %v", err)
		}
		if !strings.Contains(buf.String(), legacy) {
			t.Errorf("output %q does not contain the removed path %q", buf.String(), legacy)
		}
		if _, err := os.Stat(legacy); !os.IsNotExist(err) {
			t.Errorf("legacy store not removed: %v", err)
		}
	})

	t.Run("absent store is a no-op without a path", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		factory, err := swe.NewSessionStoreFactory()
		if err != nil {
			t.Fatalf("NewSessionStoreFactory: %v", err)
		}
		var buf bytes.Buffer
		if err := purgeLegacy(factory, &buf); err != nil {
			t.Fatalf("purgeLegacy: %v", err)
		}
		if strings.Contains(buf.String(), string(filepath.Separator)+"jetstream") {
			t.Errorf("printed a path for an absent legacy store: %q", buf.String())
		}
	})

	t.Run("listing survives a legacy purge", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_DATA_HOME", xdg)
		root, err := persistence.OpenSessionStoreRoot()
		if err != nil {
			t.Fatalf("OpenSessionStoreRoot: %v", err)
		}
		id := seedSession(t, root, "kept work", time.Now())
		legacy := filepath.Join(xdg, "looprig", "jetstream")
		if err := os.MkdirAll(legacy, 0o700); err != nil {
			t.Fatalf("seed legacy: %v", err)
		}

		factory, err := swe.NewSessionStoreFactory()
		if err != nil {
			t.Fatalf("NewSessionStoreFactory: %v", err)
		}
		var pbuf bytes.Buffer
		if err := purgeLegacy(factory, &pbuf); err != nil {
			t.Fatalf("purgeLegacy: %v", err)
		}
		if _, err := os.Stat(legacy); !os.IsNotExist(err) {
			t.Errorf("legacy store not removed: %v", err)
		}

		var lbuf bytes.Buffer
		if err := listSessions(factory, &lbuf); err != nil {
			t.Fatalf("listSessions after purge: %v", err)
		}
		if !strings.Contains(lbuf.String(), id.String()) {
			t.Errorf("session missing from listing after purge:\n%s", lbuf.String())
		}
	})

	t.Run("symlinked legacy path is refused", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_DATA_HOME", xdg)
		appDir := filepath.Join(xdg, "looprig")
		if err := os.MkdirAll(appDir, 0o700); err != nil {
			t.Fatalf("seed app dir: %v", err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(appDir, "jetstream")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		factory, err := swe.NewSessionStoreFactory()
		if err != nil {
			t.Fatalf("NewSessionStoreFactory: %v", err)
		}
		var buf bytes.Buffer
		if err := purgeLegacy(factory, &buf); err == nil {
			t.Fatal("purgeLegacy on a symlinked legacy path succeeded, want error")
		}
	})
}
