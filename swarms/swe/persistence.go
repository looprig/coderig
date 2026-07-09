package swe

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/fsstore"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/inference"
)

// persistence.go is the SWE-Swarm's composition-root wiring for durable session state. It
// turns the bare session.New / session.Restore into a fully-persisted operator-primary
// session over a single on-disk store. Post-split it stops driving journal primitives
// directly over NATS: it opens ONE fsstore backend, wraps it in a *sessionstore.Store (which
// addresses every session by name — sessions/<uuid> — and owns lease/journal/replay/GC/catalog
// internally) and a *workspacestore.Store (durable workspace snapshots over the SAME backend),
// and constructs sessions with the neutral journal appenders (new) or session.Restore (resume).
// The former per-session embedded-engine abstraction and the whole NATS dependency chain are
// gone: one backend serves every session, so two sessions can be active at once without any
// per-session engine.
//
// The single-writer lease's lifecycle is owned by the SESSION (it releases the lease on
// Shutdown — for a new session via the WithLeaseRelease hook installed here; for a restore via
// the hook Restore installs from the lease it acquired), so this layer only schedules + stops
// GC and closes the shared backend once at process teardown. At each session quiescence
// (event.SessionIdle) it checkpoints the workspace so a later restore can materialize it.
//
// Subagent wiring: the persisted + restored sessions are both built from the SAME
// operatorWiring (leaf registry + unbound spawner + primary cfg with Subagent wired) the
// headless New path uses, via buildOperatorWiring. Both openNew and openResume bind the live
// session onto the wiring's spawner AFTER the session is built (before any turn runs), so a
// persisted or restored operator can spawn leaves by name exactly like a headless one.

// gcInterval is how often the background GC ticker runs one lease-guarded orphan-GC pass. GC
// is idempotent + lease-guarded, so the cadence only trades disk reclaim latency for a little
// background work; a few minutes is plenty for a local single-user CLI.
const gcInterval = 5 * time.Minute

// leaseReleaseTimeout bounds the best-effort lease release on a construction-failure path. The
// lease's own expiry is the backstop if it fails.
const leaseReleaseTimeout = 5 * time.Second

// checkpointTimeout bounds one best-effort workspace snapshot at quiescence so a huge or slow
// workspace can never hold the checkpoint watcher open indefinitely.
const checkpointTimeout = 60 * time.Second

// DefaultDataDir is the default root for the on-disk session store: ~/.looprig/store (session
// ledger/journal/blobs + workspace snapshots live under it via fsstore). It mirrors the
// location the former embedded store used and is overridable at the CLI boundary (--data-dir).
// It fails loud with a typed *StoreInitError if the home directory cannot be resolved, so the
// default never silently falls back to a surprising path.
func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", &StoreInitError{Stage: "data-dir", Cause: err}
	}
	return filepath.Join(home, ".looprig", "store"), nil
}

// persistedAgentBuilder turns the factory-resolved session id into a persisted *sessionAgent.
// isNew selects the new-vs-resume construction. Production builds the real journal-backed agent
// (buildPersistedAgent); unit tests inject a fake that returns a headless agent without opening
// the store.
type persistedAgentBuilder func(ctx context.Context, client inference.Client, factory ModelFactory, id uuid.UUID, isNew bool, sel SessionSelector, cfg Config) (*sessionAgent, error)

// SessionStoreFactory is the process-level composition root that owns the single on-disk
// session store and, on each open, mints (for a new session) or receives (for a resume) a
// session id and builds that session over the shared store. It holds the fsstore backend (to
// close once at teardown), the sessionstore facade (lease/journal/replay/GC, sessions by
// name), the listing catalog, and the workspace store — all over the same backend. Read-only
// after construction.
type SessionStoreFactory struct {
	fs          *fsstore.Store        // the on-disk backend; closed once at process teardown
	store       *sessionstore.Store   // session lease/journal/replay/GC over fs, addressed by name
	catalog     *sessionstore.Catalog // listing index, event-tap-backed + replay-repairable
	ws          *workspacestore.Store // durable workspace snapshots over the SAME fs blobs
	buildClient func(catalog ModelCatalog) (inference.Client, ModelFactory, error)
	build       persistedAgentBuilder // production = buildPersistedAgent; tests inject a fake
}

// NewSessionStoreFactory opens the on-disk session store rooted at dataDir and returns the
// production factory: the fsstore backend, the sessionstore + workspacestore facades over it,
// the real journal-backed agent builder, and the real provider client builder (reads
// LLM_API_KEY). It fails closed with a typed *StoreInitError if any store layer cannot be
// opened, closing anything already opened so no directory lock leaks.
func NewSessionStoreFactory(dataDir string) (*SessionStoreFactory, error) {
	fs, err := fsstore.Open(fsstore.Options{Root: dataDir})
	if err != nil {
		return nil, &StoreInitError{Stage: "fsstore", Cause: err}
	}
	f, err := newSessionStoreFactory(fs)
	if err != nil {
		_ = fs.Close()
		return nil, err
	}
	return f, nil
}

// newSessionStoreFactory wires the sessionstore + workspacestore facades and the production
// collaborators around an already-opened fsstore backend. It is shared by
// NewSessionStoreFactory and the integration tests (which open the backend under a temp dir and
// then inject a fake client via openWithClient). The caller owns fs and must Close it (directly
// or via SessionStoreFactory.Close).
func newSessionStoreFactory(fs *fsstore.Store) (*SessionStoreFactory, error) {
	store, err := sessionstore.Open(fs.Backend())
	if err != nil {
		return nil, &StoreInitError{Stage: "sessionstore", Cause: err}
	}
	ws, err := workspacestore.Open(fs.Backend().Blobs)
	if err != nil {
		return nil, &StoreInitError{Stage: "workspacestore", Cause: err}
	}
	// The catalog is wired with a replayer (the store satisfies EventReplayerOpener) so a
	// missing listing entry can be repaired by folding the authoritative session ledger. The
	// listing hot path never replays.
	catalog := store.OpenCatalog(sessionstore.WithCatalogReplayer(store))
	f := &SessionStoreFactory{
		fs:          fs,
		store:       store,
		catalog:     catalog,
		ws:          ws,
		buildClient: buildClient,
	}
	f.build = f.buildPersistedAgent
	return f, nil
}

// Close releases the shared on-disk backend. It is called ONCE at process teardown, after every
// session opened from this factory has been Closed (a session's own teardown stops its GC
// ticker; the SESSION releases its single-writer lease on Shutdown). It is idempotent.
func (f *SessionStoreFactory) Close() error {
	return f.fs.Close()
}

// Open builds a fully-persisted SWE-Swarm session for sel (new or resumed) and returns it as a
// tui.Agent. It builds the provider client + ModelFactory exactly like the headless New path
// (reads LLM_API_KEY, refuses an unclassified provider, fails loud on a missing key), then
// delegates to the id-resolution + construction seam. The returned *sessionAgent satisfies
// tui.Agent.
func (f *SessionStoreFactory) Open(ctx context.Context, sel SessionSelector, cfg Config) (*sessionAgent, error) {
	client, factory, err := f.buildClient(cfg.ModelCatalog)
	if err != nil {
		return nil, err
	}
	return f.openWithClient(ctx, client, factory, sel, cfg)
}

// openWithClient resolves the session id (minting one for a new session) and builds the
// persisted agent over the shared store. It is the seam the integration tests drive with an
// injected fake client. A new session mints a fresh id here; a resume uses sel.Resume. The
// per-session lifecycle (lease, GC, workspace checkpointing) is owned by the built agent, so
// this method holds no per-session state.
func (f *SessionStoreFactory) openWithClient(ctx context.Context, client inference.Client, factory ModelFactory, sel SessionSelector, cfg Config) (*sessionAgent, error) {
	id, isNew := sel.Resume, false
	if id.IsZero() {
		isNew = true
		minted, err := uuid.New()
		if err != nil {
			return nil, &session.SessionError{Kind: session.SessionIDGenerationFailed, Cause: err}
		}
		id = minted
	}
	return f.build(ctx, client, factory, id, isNew, sel, cfg)
}

// SessionSelector chooses which session a persisted Open opens. The zero value (Resume zero)
// opens a NEW session; a non-zero Resume opens (restores) that existing session.
// AllowConfigMismatch is the resume-only opt-in to proceed despite a config fingerprint change
// (otherwise a mismatch is rejected fail-secure).
type SessionSelector struct {
	Resume              uuid.UUID
	AllowConfigMismatch bool
}

// buildPersistedAgent is the production persistedAgentBuilder: it resolves the workspace root
// once (fail-fast on os.Getwd error), builds the SAME operatorWiring the headless New uses (leaf
// registry + unbound spawner + primary cfg with Subagent wired) under cfg, computes the
// swarm-level config-fingerprint fields once (so a NEW session stamps them and a RESUMED session
// compares them), and branches on isNew. After the agent is built it installs the workspace
// checkpoint watcher so the session snapshots its workspace at each quiescence.
func (f *SessionStoreFactory) buildPersistedAgent(ctx context.Context, client inference.Client, factory ModelFactory, id uuid.UUID, isNew bool, sel SessionSelector, cfg Config) (*sessionAgent, error) {
	// The workspace root is the process working directory: file tools are confined to it, the
	// PermissionChecker uses it for containment, and the workspace store snapshots it.
	root, err := os.Getwd()
	if err != nil {
		return nil, &WorkspaceRootError{Cause: err}
	}
	wiring, err := buildOperatorWiring(client, factory, root, cfg)
	if err != nil {
		return nil, err
	}
	fields := operatorFingerprintFields(root, cfg)

	var agent *sessionAgent
	if isNew {
		agent, err = f.openNew(ctx, wiring, id, root, fields)
	} else {
		agent, err = f.openResume(ctx, wiring, sel, root, fields)
	}
	if err != nil {
		return nil, err
	}
	watchSessionEvents(agent)
	return agent, nil
}

// openNew opens a NEW persisted session over the factory-minted sessionID. It acquires the
// single-writer lease, opens the session journal (which writes the opening LeaseFence), builds
// the catalog-backed event appender + the audit command appender, and constructs the session
// with the INJECTED sessionID + both appenders + the lease-release hook (so a clean Shutdown
// frees ownership) + the operator spawn caps + the workspace store (so CheckpointWorkspace can
// snapshot at quiescence). On any failure before the session is built it releases the lease so a
// retry can re-acquire without waiting out the lease expiry.
func (f *SessionStoreFactory) openNew(ctx context.Context, wiring operatorWiring, sessionID uuid.UUID, root string, fields session.ConfigFingerprintFields) (*sessionAgent, error) {
	lease, err := f.store.AcquireLease(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	j, err := f.store.OpenJournal(ctx, sessionID, lease)
	if err != nil {
		releaseLeaseBestEffort(lease)
		return nil, err
	}

	// The REQUIRED event tap is catalog-backed (so the listing index stays current); the command
	// appender is audit-only.
	eventAppender, err := journal.NewJournalEventAppenderChecked(j, journal.WithCatalog(f.catalog))
	if err != nil {
		releaseLeaseBestEffort(lease)
		return nil, err
	}
	cmdAppender, err := journal.NewJournalCommandAppenderChecked(j)
	if err != nil {
		releaseLeaseBestEffort(lease)
		return nil, err
	}

	agent, err := newPersistentSessionAgent(ctx, wiring.cfg,
		session.WithSessionID(sessionID),
		session.WithEventAppender(eventAppender),
		session.WithCommandAppender(cmdAppender),
		session.WithLeaseRelease(lease.Release),
		session.WithLimits(operatorLimits()),
		session.WithConfigFingerprintFields(fields),
		session.WithWorkspaceStore(f.ws, root),
		session.WithCeiling(wiring.ceiling), // SAME ceiling the checkers read → journaled changes are visible
	)
	if err != nil {
		releaseLeaseBestEffort(lease)
		return nil, err
	}
	wiring.spawner.bind(agent.session) // late-bind before any turn runs

	agent.teardown = stopGCTeardown(f.scheduleGC(agent.rootCtx, sessionID, lease))
	return agent, nil
}

// openResume RESTORES an existing session via session.Restore, which acquires the lease, opens
// the journal, folds the durable log, materializes the last checkpointed workspace (because the
// workspace store is wired), brings the primary loop up idle, and installs its own
// lease-release-on-Shutdown hook. The resumed agent's event replayer is wired so ReplayBacklog
// can repaint the restored transcript.
func (f *SessionStoreFactory) openResume(ctx context.Context, wiring operatorWiring, sel SessionSelector, root string, fields session.ConfigFingerprintFields) (*sessionAgent, error) {
	// Inject the SAME swarm-level fingerprint fields the original run stamped, so Restore's live
	// fingerprint is computed identically; a different skill-trust mode or workspace then rejects
	// (unless WithAllowConfigMismatch). Wire the workspace store so Restore materializes the last
	// WorkspaceCheckpointed before the session goes live.
	opts := []session.Option{
		session.WithLimits(operatorLimits()),
		session.WithConfigFingerprintFields(fields),
		session.WithWorkspaceStore(f.ws, root),
		// The SAME ceiling the checkers read. Restore re-seeds it from the folded
		// SecurityCeilingChanged events (clamped by the NewClamped cap), so a resumed
		// session's checker sees the recovered ceiling (SPEC §8).
		session.WithCeiling(wiring.ceiling),
	}
	if sel.AllowConfigMismatch {
		opts = append(opts, session.WithAllowConfigMismatch())
	}

	agent, err := newRestoredSessionAgent(ctx, wiring.cfg, sel.Resume, f.store, opts...)
	if err != nil {
		return nil, err
	}
	wiring.spawner.bind(agent.session) // late-bind before any turn runs

	// GC for a RESUMED session is a documented follow-on: orphan-GC needs a journal.Lease handle
	// to gate each pass, but session.Restore acquires + owns the lease internally (installing its
	// own lease-release-on-Shutdown hook) and does not hand the handle back. Until a lease handle
	// is threaded out of Restore the resumed session schedules NO GC (orphan objects, if any, are
	// reclaimed when the session is next opened NEW — GC is best-effort reclaim, never load-bearing).
	slog.Debug("swe: GC not scheduled for resumed session (lease is session-owned; follow-on)", "session", sel.Resume)
	agent.teardown = stopGCTeardown(nil)

	if er, rerr := f.store.OpenEventReplayer(sel.Resume, sessionstore.ReplayRequest{}); rerr != nil {
		slog.Warn("swe: event replayer unavailable; cold-restore repaint disabled", "session", sel.Resume, "err", rerr)
	} else {
		agent.replayer = er
	}
	agent.restoredSessionID = sel.Resume
	agent.restoredPrimaryLoopID = agent.session.PrimaryLoopID()
	return agent, nil
}

// watchSessionEvents subscribes to the session's enduring events and, on every quiescence
// (event.SessionIdle — the Active→Idle edge, the same point a cloud harness would suspend),
// checkpoints the workspace so a later restore can materialize it. Checkpointing is entirely
// best-effort and non-blocking: it runs on the subscription's own goroutine (never a turn),
// each snapshot is bounded by checkpointTimeout, and every failure is logged. The subscription
// closes when the session shuts down, so the goroutine exits on Close — it is never waited on.
func watchSessionEvents(agent *sessionAgent) {
	sub, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		slog.Warn("swe: session event subscription failed; workspace checkpointing disabled", "err", err)
		return
	}
	go func() {
		defer func() { _ = sub.Close() }()
		for d := range sub.Events() {
			if _, ok := d.Event.(event.SessionIdle); !ok {
				continue
			}
			ctx, cancel := context.WithTimeout(agent.rootCtx, checkpointTimeout)
			ref, cerr := agent.session.CheckpointWorkspace(ctx)
			cancel()
			if cerr != nil {
				var notConfigured *session.WorkspaceNotConfiguredError
				if errors.As(cerr, &notConfigured) {
					// The workspace store is unwired (a headless/fake agent) — no point retrying.
					slog.Debug("swe: workspace store not configured; checkpointing disabled", "err", cerr)
					return
				}
				slog.Warn("swe: workspace checkpoint at quiescence failed (best-effort)", "err", cerr)
				continue
			}
			slog.Debug("swe: workspace checkpointed at quiescence", "ref", string(ref))
		}
	}()
}

// List returns the session catalog (the catalog's own most-recently-active-first ordering), the
// source the CLI --list path prints. It reads the listing index only — no session lease, no
// replay — so it stays cheap and cannot contend a running session. ctx bounds the read.
func (f *SessionStoreFactory) List(ctx context.Context) ([]sessionstore.SessionMeta, error) {
	return f.catalog.ListSessions(ctx)
}

// scheduleGC starts a background goroutine that runs one lease-guarded orphan-GC pass every
// gcInterval, stopped by the returned (idempotent) stop func. Each pass builds a fresh ObjectGC
// over the session's blobs; a build or pass error is logged and the ticker continues (GC is
// best-effort reclaim, never load-bearing). It runs under rootCtx so a session-root cancel also
// stops it.
func (f *SessionStoreFactory) scheduleGC(rootCtx context.Context, sessionID uuid.UUID, lease journal.Lease) func() {
	return runGCTicker(rootCtx, func(ctx context.Context) {
		f.runGCPass(ctx, sessionID, lease)
	})
}

// runGCTicker launches the ticker goroutine that calls pass every gcInterval until the returned
// stop func is called or rootCtx is done. The stop func is idempotent and blocks until the
// goroutine has exited (so teardown is deterministic).
func runGCTicker(rootCtx context.Context, pass func(context.Context)) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(gcInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-rootCtx.Done():
				return
			case <-t.C:
				pass(rootCtx)
			}
		}
	}()
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		close(stop)
		<-done
	}
}

// runGCPass builds an ObjectGC over the session's blobs and runs one pass, logging (never
// propagating) any error. A pass that finds the lease not held (a successor took over, or the
// lease was released at teardown) is expected and logged at debug; any scan/list/delete error is
// logged at warn. GC is idempotent + lease-guarded.
func (f *SessionStoreFactory) runGCPass(ctx context.Context, sessionID uuid.UUID, lease journal.Lease) {
	gc, err := f.store.OpenObjectGC(sessionID, lease)
	if err != nil {
		slog.Warn("swe: GC build failed", "session", sessionID, "err", err)
		return
	}
	if _, err := gc.GC(ctx); err != nil {
		slog.Debug("swe: GC pass error (best-effort)", "session", sessionID, "err", err)
	}
}

// stopGCTeardown wraps a GC stop func as the sessionAgent teardown closure: it stops the GC
// ticker (so no pass runs after the session is gone) and returns nil. The single-writer lease is
// released by the SESSION on Shutdown (the WithLeaseRelease hook for a new session, or the hook
// Restore installed), and the shared backend is closed once by SessionStoreFactory.Close, so
// this teardown owns only the GC lifecycle. A nil gcStop (a resumed session) is a no-op.
func stopGCTeardown(gcStop func()) func(context.Context) error {
	return func(context.Context) error {
		if gcStop != nil {
			gcStop()
		}
		return nil
	}
}

// releaseLeaseBestEffort releases a lease on a bounded context, swallowing the error (the lease
// expiry is the backstop). Used on the NEW-session construction-failure paths so a partly-built
// session does not strand its lease until it expires.
func releaseLeaseBestEffort(lease journal.Lease) {
	rctx, cancel := context.WithTimeout(context.Background(), leaseReleaseTimeout)
	defer cancel()
	_ = lease.Release(rctx)
}
