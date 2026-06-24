package swe

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/ciram-co/looprig/pkg/journal"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/persistence"
	"github.com/ciram-co/looprig/pkg/session"
	"github.com/ciram-co/looprig/pkg/uuid"
	"github.com/nats-io/nats.go"
)

// persistence.go is the SWE-Swarm's composition-root wiring for the durable session
// journal: it turns the bare session.New / session.Restore into a fully-persisted
// orchestrator-primary session over an embedded JetStream context. It is salvaged from
// the prior coding agent's persistence.go and adapted to the swarm's orchestratorConfig builder, so
// the persistence layer owns ONLY the new-vs-resume decision, the sessionID
// chicken-and-egg resolution (mint first → build journal → inject the SAME id into
// session.New), and GC scheduling. The single-writer lease's lifecycle is owned by the
// SESSION (it releases the lease on Shutdown — for a new session via the WithLeaseRelease
// hook installed here; for a restore via the hook Restore installs from the lease it
// acquired), so this layer only schedules + stops GC. It depends on a bound
// nats.JetStreamContext (the embedded server is started in internal/persistence at the
// cmd/swe root, never here), so the swarm package never imports nats-server.
//
// Subagent wiring: the persisted + restored sessions are both built from the SAME
// orchestratorWiring (leaf registry + unbound spawner + primary cfg with Subagent
// wired) that the headless New path uses, via buildOrchestratorWiring. Both openNew and
// openResume bind the live session onto the wiring's spawner AFTER the session is built
// (before any turn runs), so a persisted or restored orchestrator can spawn leaves by
// name exactly like a headless one. There is one wiring per Open call (its spawner is
// session-scoped), so a /clear reopen builds a fresh wiring for the fresh session.

// gcInterval is how often the background GC ticker runs one lease-guarded orphan-GC pass.
// GC is idempotent + lease-guarded, so the cadence only trades disk reclaim latency for a
// little background work; a few minutes is plenty for a local single-user CLI.
const gcInterval = 5 * time.Minute

// leaseReleaseTimeout bounds the best-effort lease release on a construction-failure path.
// The bucket TTL is the backstop if it fails.
const leaseReleaseTimeout = 5 * time.Second

// Persistence is the per-SESSION durable-journal context built fresh over one isolated
// embedded engine's JetStreamContext, every time the factory opens a session (a new session
// or a /clear reopen mints a fresh id → a fresh engine → a fresh Persistence). It owns the
// session-local journal infrastructure: the JetStream context, the lease manager, and the
// catalog. Per-session objects (the stream journal, object store, replayer, GC) are built by
// the persisted constructors. It is read-only after construction.
type Persistence struct {
	js      nats.JetStreamContext
	leases  *journal.LeaseManager
	catalog *journal.Catalog
}

// NewPersistence provisions the shared lease bucket + session catalog over a bound
// JetStream context and returns the reusable Persistence context. js must come from a
// live embedded engine (internal/persistence). It fails closed (typed journal setup
// error) if the buckets cannot be provisioned. The catalog is wired with a replayer so a
// missing entry can be repaired from the authoritative stream.
func NewPersistence(js nats.JetStreamContext) (*Persistence, error) {
	leases, err := journal.NewLeaseManager(js)
	if err != nil {
		return nil, err
	}
	// The catalog's RepairCatalog scans the stream through a replayer (event subjects
	// only — the events repair folds, SessionStarted/TurnStarted/RestoreDone/…, are never
	// offloaded in practice, so a nil object store on this repair-only replayer is safe).
	// The listing hot path never replays.
	catalog, err := journal.NewCatalog(js, journal.WithCatalogReplayer(journal.NewEventReplayer(js, nil)))
	if err != nil {
		return nil, err
	}
	return &Persistence{js: js, leases: leases, catalog: catalog}, nil
}

// sessionEngine is the per-session embedded engine the factory builds journal dependencies
// from and closes on the agent's teardown. *persistence.SessionEngine satisfies it.
type sessionEngine interface {
	JetStream() nats.JetStreamContext
	Close() error
}

// engineOpener opens an isolated embedded engine for a session id. The production opener
// wraps *persistence.SessionStoreRoot; unit tests inject a fake that records the id and
// returns a fake engine, so they never start NATS.
type engineOpener interface {
	OpenSessionEngine(id uuid.UUID) (sessionEngine, error)
}

// storeRootEngineOpener adapts *persistence.SessionStoreRoot to engineOpener (the concrete
// *persistence.SessionEngine it returns satisfies the sessionEngine interface).
type storeRootEngineOpener struct{ root *persistence.SessionStoreRoot }

func (o storeRootEngineOpener) OpenSessionEngine(id uuid.UUID) (sessionEngine, error) {
	return o.root.OpenSessionEngine(id)
}

// agentBuilder turns a live session engine's JetStream context into a persisted
// *sessionAgent. isNew selects the new-vs-resume construction; id is the factory-resolved
// session id. Production builds the real journal-backed agent; unit tests inject a fake that
// returns a headless agent without starting NATS.
type agentBuilder func(ctx context.Context, js nats.JetStreamContext, client llm.LLM, factory ModelFactory, id uuid.UUID, isNew bool, sel SessionSelector, cfg Config) (*sessionAgent, error)

// SessionStoreFactory is the session-scoped composition root that replaces the former
// process-global startPersistence + shared Persistence. It owns the confined session store
// and, on each open, mints (for a new session) or receives (for a resume) a session id,
// opens that session's isolated embedded engine, builds the journal dependencies from that
// engine, and installs engine close as the persisted agent's teardown. Two distinct sessions
// can therefore be active at once over independent StoreDirs.
type SessionStoreFactory struct {
	root        *persistence.SessionStoreRoot
	opener      engineOpener
	build       agentBuilder
	buildClient func() (llm.LLM, ModelFactory, error)
}

// NewSessionStoreFactory opens the confined session store and returns the production factory:
// real per-session engines, the real journal-backed agent builder, and the real provider
// client builder (reads LLM_API_KEY). It fails closed if the session store cannot be opened.
func NewSessionStoreFactory() (*SessionStoreFactory, error) {
	root, err := persistence.OpenSessionStoreRoot()
	if err != nil {
		return nil, err
	}
	return newSessionStoreFactory(root), nil
}

// newSessionStoreFactory wires the production collaborators around an already-opened store
// root. It is shared by NewSessionStoreFactory and the integration tests (which open the
// root under a temp XDG home and then inject a fake client via openWithClient).
func newSessionStoreFactory(root *persistence.SessionStoreRoot) *SessionStoreFactory {
	f := &SessionStoreFactory{
		root:        root,
		opener:      storeRootEngineOpener{root: root},
		buildClient: buildClient,
	}
	f.build = f.buildPersistedAgent
	return f
}

// Open builds a fully-persisted SWE-Swarm session for sel (new or resumed) and returns it as
// a tui.Agent. It builds the provider client + ModelFactory exactly like the headless New
// path (reads LLM_API_KEY, refuses an unclassified provider, fails loud on a missing key),
// then delegates to the session-scoped construction seam. The returned *sessionAgent
// satisfies tui.Agent.
func (f *SessionStoreFactory) Open(ctx context.Context, sel SessionSelector, cfg Config) (*sessionAgent, error) {
	client, factory, err := f.buildClient()
	if err != nil {
		return nil, err
	}
	return f.openWithClient(ctx, client, factory, sel, cfg)
}

// openWithClient resolves the session id (minting one for a new session BEFORE any engine is
// constructed), opens that id's isolated engine, builds the persisted agent from the engine's
// JetStream context, and installs engine close as the agent teardown so a clean Close drains
// the journal and then releases the session's lock + StoreDir. It is the seam the integration
// tests drive with an injected fake client.
func (f *SessionStoreFactory) openWithClient(ctx context.Context, client llm.LLM, factory ModelFactory, sel SessionSelector, cfg Config) (*sessionAgent, error) {
	id, isNew := sel.Resume, false
	if id.IsZero() {
		isNew = true
		minted, err := uuid.New()
		if err != nil {
			return nil, &session.SessionError{Kind: session.SessionIDGenerationFailed, Cause: err}
		}
		id = minted
	}

	engine, err := f.opener.OpenSessionEngine(id)
	if err != nil {
		return nil, err
	}

	agent, err := f.build(ctx, engine.JetStream(), client, factory, id, isNew, sel, cfg)
	if err != nil {
		_ = engine.Close()
		return nil, err
	}
	agent.teardown = appendEngineClose(agent.teardown, engine)
	return agent, nil
}

// List returns the session manifests read directly from the filesystem session store
// (most-recently-updated first), the engine-free source the CLI --list path prints. It
// never opens an embedded engine, so it stays cheap and cannot contend a running session.
func (f *SessionStoreFactory) List() ([]persistence.SessionListEntry, error) {
	return f.root.ListSessionMeta()
}

// buildPersistedAgent is the production agentBuilder: it builds the per-session journal
// dependencies over the engine's JetStream context and constructs the persisted (new or
// resumed) agent under the factory-resolved id.
func (f *SessionStoreFactory) buildPersistedAgent(ctx context.Context, js nats.JetStreamContext, client llm.LLM, factory ModelFactory, id uuid.UUID, isNew bool, sel SessionSelector, cfg Config) (*sessionAgent, error) {
	p, err := NewPersistence(js)
	if err != nil {
		return nil, err
	}
	return p.openResolved(ctx, client, factory, id, isNew, sel, cfg)
}

// appendEngineClose composes the existing persistence teardown (the GC-stop closure) with
// the session engine's Close, running engine Close AFTER the prior teardown — and therefore
// after session.Shutdown's final append — so the journal is flushed before the StoreDir is
// closed and the lock released. An engine close error is joined so neither error is lost.
func appendEngineClose(prev func(context.Context) error, engine sessionEngine) func(context.Context) error {
	return func(ctx context.Context) error {
		var err error
		if prev != nil {
			err = prev(ctx)
		}
		if cerr := engine.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return err
	}
}

// SessionSelector chooses which session a persisted Open opens. The zero value (Resume
// zero) opens a NEW session; a non-zero Resume opens (restores) that existing session.
// AllowConfigMismatch is the resume-only opt-in to proceed despite a config fingerprint
// change (otherwise a mismatch is rejected fail-secure). It mirrors the prior coding agent's
// SessionSelector shape so the swarm's --list/--resume wiring matches the coding CLI.
type SessionSelector struct {
	Resume              uuid.UUID
	AllowConfigMismatch bool
}

// openResolved is the persisted construction seam shared by the factory and the integration
// tests (which inject a fake llm.LLM + key-bound ModelFactory). It branches on isNew: a NEW
// open builds over the factory-minted id (the journal must use the SAME id the session
// directory + engine were opened with); a RESUME restores sel.Resume. It resolves the
// workspace root once (fail-fast on os.Getwd error) and builds the SAME orchestratorWiring
// the headless New uses (leaf registry + unbound spawner + primary cfg with Subagent
// wired) under cfg (the human-set modes — RuntimeSkills), so both branches construct an
// identical orchestrator and both bind the live session onto the spawner after building it.
func (p *Persistence) openResolved(ctx context.Context, client llm.LLM, factory ModelFactory, id uuid.UUID, isNew bool, sel SessionSelector, cfg Config) (*sessionAgent, error) {
	// The workspace root is the process working directory: file tools are confined to it
	// and the PermissionChecker uses it for containment + path relativisation.
	root, err := os.Getwd()
	if err != nil {
		return nil, &WorkspaceRootError{Cause: err}
	}
	wiring, err := buildOrchestratorWiring(client, factory, root, cfg)
	if err != nil {
		return nil, err
	}

	// The swarm-level config-fingerprint fields (AgentKind + RuntimeSkills mode + canonical
	// workspace-root id) are computed once here, where root + cfg are in scope, and threaded
	// into both construction paths so a NEW session stamps them and a RESUMED session compares
	// them (a different skill-trust mode or workspace then rejects). Same fields the headless
	// New path injects, so the persisted and headless fingerprints cannot drift.
	fields := orchestratorFingerprintFields(root, cfg)

	if isNew {
		return p.openNew(ctx, wiring, id, fields)
	}
	return p.openResume(ctx, wiring, sel, fields)
}

// openNew opens a NEW persisted session over the factory-minted sessionID (the engine and
// session directory were already opened on that id). It acquires the lease, constructs the
// journal (which writes the opening LeaseFence), builds the catalog-backed event appender +
// the command appender, and finally calls newPersistentSessionAgent with the INJECTED
// sessionID + both appenders + the lease-release hook (so a clean Shutdown frees ownership) +
// the orchestrator spawn caps. On any failure before the session is built it releases the
// lease so a retry can re-acquire without waiting out the TTL.
func (p *Persistence) openNew(ctx context.Context, wiring orchestratorWiring, sessionID uuid.UUID, fields session.ConfigFingerprintFields) (*sessionAgent, error) {
	// (1) The sessionID was minted by the factory BEFORE the engine was opened — the session
	// directory, lock, and embedded StoreDir are already keyed on it. The journal binds the
	// stream + writes the opening LeaseFence under that SAME id. (chicken-and-egg resolution)

	// (2) Acquire the single-writer lease, then build the journal (opening LeaseFence).
	lease, err := p.leases.Acquire(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	j, err := journal.NewSessionJournal(p.js, sessionID, lease)
	if err != nil {
		releaseLeaseBestEffort(lease)
		return nil, err
	}

	// (3) Build the appenders: the REQUIRED event tap (catalog-backed so the listing index
	// stays current) and the AUDIT-ONLY command appender.
	eventAppender, err := journal.NewJournalEventAppenderChecked(j, journal.WithCatalog(p.catalog))
	if err != nil {
		releaseLeaseBestEffort(lease)
		return nil, err
	}
	cmdAppender, err := journal.NewJournalCommandAppenderChecked(j)
	if err != nil {
		releaseLeaseBestEffort(lease)
		return nil, err
	}

	// (4) Build the persisted session with the INJECTED sessionID + appenders +
	// lease-release hook + spawn caps → a fully-persisted session that frees ownership on
	// a clean Shutdown.
	agent, err := newPersistentSessionAgent(ctx, wiring.cfg,
		session.WithSessionID(sessionID),
		session.WithEventAppender(eventAppender),
		session.WithCommandAppender(cmdAppender),
		session.WithLeaseRelease(lease.Release),
		session.WithLimits(orchestratorLimits()),
		session.WithConfigFingerprintFields(fields),
	)
	if err != nil {
		releaseLeaseBestEffort(lease)
		return nil, err
	}
	wiring.spawner.bind(agent.session) // late-bind before any turn runs

	// A NEW session has no backlog to repaint: the replayer stays nil → ReplayBacklog nil.
	agent.teardown = stopGCTeardown(scheduleGC(agent.rootCtx, p.js, sessionID, lease))
	return agent, nil
}

// openResume RESTORES an existing session via session.Restore: it binds the per-session
// object store, then Restore acquires the lease, folds the durable log, brings the primary
// loop up idle, and installs its own lease-release-on-Shutdown hook (so a clean Shutdown
// frees ownership). The resumed agent's replayer is wired so ReplayBacklog can repaint the
// restored transcript under the orchestrator spawn caps.
func (p *Persistence) openResume(ctx context.Context, wiring orchestratorWiring, sel SessionSelector, fields session.ConfigFingerprintFields) (*sessionAgent, error) {
	objects, err := p.js.ObjectStore(journal.SessionObjectBucket(sel.Resume))
	if err != nil {
		return nil, err
	}

	// Inject the SAME swarm-level fingerprint fields the original run stamped, so Restore's
	// live fingerprint is computed identically; a different skill-trust mode or workspace
	// then rejects (unless WithAllowConfigMismatch).
	opts := []session.Option{
		session.WithLimits(orchestratorLimits()),
		session.WithConfigFingerprintFields(fields),
	}
	if sel.AllowConfigMismatch {
		opts = append(opts, session.WithAllowConfigMismatch())
	}

	agent, err := newRestoredSessionAgent(ctx, wiring.cfg, sel.Resume, p.js, objects, p.leases, opts...)
	if err != nil {
		return nil, err
	}
	wiring.spawner.bind(agent.session) // late-bind before any turn runs

	// GC for a RESUMED session is a documented follow-on: orphan-GC needs a journal.Lease
	// handle to gate each pass, but session.Restore acquires + owns the lease internally
	// (it installs its own lease-release-on-Shutdown hook) and does not hand the handle
	// back. Threading a lease handle out of Restore is a small follow-on; until then the
	// resumed session schedules NO GC (orphan offload objects, if any, are reclaimed when
	// the session is next opened NEW — GC is best-effort reclaim, never load-bearing).
	slog.Debug("swe: GC not scheduled for resumed session (lease is session-owned; follow-on)", "session", sel.Resume)
	agent.teardown = stopGCTeardown(nil)
	agent.replayer = journal.NewEventReplayer(p.js, objects)
	agent.restoredSessionID = sel.Resume
	agent.restoredPrimaryLoopID = agent.session.PrimaryLoopID()
	return agent, nil
}

// scheduleGC starts a background goroutine that runs one lease-guarded orphan-GC pass
// every gcInterval, stopped by the returned (idempotent) stop func. Each pass builds a
// fresh ObjectGC over the session's object store; a build or pass error is logged and the
// ticker continues (GC is best-effort reclaim, never load-bearing). It runs under rootCtx
// so a session-root cancel also stops it.
func scheduleGC(rootCtx context.Context, js nats.JetStreamContext, sessionID uuid.UUID, lease journal.Lease) func() {
	objects, oerr := js.ObjectStore(journal.SessionObjectBucket(sessionID))
	if oerr != nil {
		// No object store yet → nothing to GC (a session that never offloaded). No-op
		// stopper; not an error (GC is best-effort).
		slog.Debug("swe: GC disabled (no object store)", "session", sessionID, "err", oerr)
		return func() {}
	}
	return runGCTicker(rootCtx, func(ctx context.Context) {
		runGCPass(ctx, js, objects, lease, sessionID)
	})
}

// runGCTicker launches the ticker goroutine that calls pass every gcInterval until the
// returned stop func is called or rootCtx is done. The stop func is idempotent and blocks
// until the goroutine has exited (so teardown is deterministic).
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

// runGCPass builds an ObjectGC and runs one pass, logging (never propagating) any error.
// A pass that finds the lease not held (a successor took over, or the lease was released
// at teardown) is expected and logged at debug; any scan/list/delete error is logged at
// warn. GC is idempotent + lease-guarded.
func runGCPass(ctx context.Context, js nats.JetStreamContext, objects nats.ObjectStore, lease journal.Lease, sessionID uuid.UUID) {
	gc, err := journal.NewObjectGC(js, objects, lease, sessionID)
	if err != nil {
		slog.Warn("swe: GC build failed", "session", sessionID, "err", err)
		return
	}
	if _, err := gc.GC(ctx); err != nil {
		slog.Debug("swe: GC pass error (best-effort)", "session", sessionID, "err", err)
	}
}

// stopGCTeardown wraps a GC stop func as the sessionAgent teardown closure: it stops the
// GC ticker (so no pass runs after the session is gone) and returns nil. The single-writer
// lease is released by the SESSION on Shutdown (the WithLeaseRelease hook for a new
// session, or the hook Restore installed), so teardown owns only the GC lifecycle.
func stopGCTeardown(gcStop func()) func(context.Context) error {
	return func(context.Context) error {
		if gcStop != nil {
			gcStop()
		}
		return nil
	}
}

// releaseLeaseBestEffort releases a lease on a bounded context, swallowing the error (the
// bucket TTL is the backstop). Used on the NEW-session construction-failure paths so a
// partly-built session does not strand its lease until the TTL.
func releaseLeaseBestEffort(lease journal.Lease) {
	rctx, cancel := context.WithTimeout(context.Background(), leaseReleaseTimeout)
	defer cancel()
	_ = lease.Release(rctx)
}
