package swe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
)

// sessionAgent is a thin wrapper over a session.Session that exposes the tui.Agent
// surface (the streaming/lifecycle methods plus the Approve/Deny/ProvideAnswer gate
// trio). It is salvaged from the prior coding agent's Coding wrapper, generalized over an
// arbitrary primary loop.Config so the SAME wrapper drives the operator (the
// swarm's primary) and any other primary configuration. The caller owns it and must
// call Close to release the underlying actor goroutine.
//
// It holds no submit/gate/subscribe state of its own — every method delegates to
// the session — so the wrapper's sole responsibility is lifetime ownership (the
// agent-owned root cancel) and reporting the static AcceptsImages modality.
type sessionAgent struct {
	session       *session.Session
	rootCtx       context.Context    // the agent-owned root the session runs under; persistence schedules GC under it
	cancel        context.CancelFunc // cancels the session's root context; called by Close
	acceptsImages bool               // captured from the primary spec at construction; reported by AcceptsImages

	// teardown is the composition-root persistence teardown the persisted constructors
	// install: it stops the GC ticker. It is nil for a non-persisted (headless / fake-only)
	// agent, so Close is unchanged in that mode. Run AFTER session.Shutdown so the journal
	// has finished its last append before teardown. The single-writer lease is released by
	// the SESSION on Shutdown (the WithLeaseRelease hook for a new session, or the hook
	// Restore installed), so teardown owns only the GC lifecycle. Idempotent (guarded by
	// teardownOnce) so Close can safely be called more than once.
	teardown     func(context.Context) error
	teardownOnce sync.Once

	// replayer is the journal-backed read side a RESTORED session's ReplayBacklog drains
	// for the TUI's cold-restore repaint. It is nil for a NEW or headless session (no
	// backlog to repaint → ReplayBacklog returns nil). restoredSessionID/
	// restoredPrimaryLoopID scope the cold replay to the primary loop's session view.
	replayer              journal.EventReplayer
	restoredSessionID     uuid.UUID
	restoredPrimaryLoopID uuid.UUID
}

// newSessionAgent constructs a sessionAgent from a finished primary loop.Config and
// optional session options (e.g. session.WithLimits). It gives the session a root
// context derived from context.Background() — INDEPENDENT of the caller's ctx — so a
// request-scoped or timeout ctx passed in cannot later tear the session down; ctx
// bounds only this construction call. Because the session root is background-derived,
// session.New cannot observe a cancelled caller ctx, so newSessionAgent checks the
// caller ctx itself and fails fast with a typed *session.SessionError on a cancelled
// ctx (fail secure). On any session.New failure it cancels the root so nothing leaks.
func newSessionAgent(ctx context.Context, primary loop.Config, opts ...session.Option) (*sessionAgent, error) {
	if err := ctx.Err(); err != nil {
		return nil, &session.SessionError{Kind: session.SessionContextDone, Cause: err}
	}

	// The session's root context — independent of the caller's ctx — owns the actor's
	// lifetime (and, transitively, every sub-loop the session spawns).
	rootCtx, cancel := context.WithCancel(context.Background())

	sess, err := session.New(rootCtx, primary, opts...)
	if err != nil {
		cancel()
		return nil, err
	}
	return &sessionAgent{session: sess, rootCtx: rootCtx, cancel: cancel, acceptsImages: primary.Model.Caps.AcceptsImages}, nil
}

// newPersistentSessionAgent constructs a sessionAgent over a NEW persisted session: it is
// the persisted counterpart to newSessionAgent (same agent-owned, background-derived root
// + fail-fast on a cancelled caller ctx) that calls session.New with the composition
// root's persistence options (the injected sessionID, event + command appenders, and the
// lease-release hook) plus the spawn caps. It is the single place persistence.go turns a
// finished primary cfg + persistence opts into the wrapper, so the new-vs-headless paths
// cannot drift. The caller (persistence.go) installs the GC teardown after this returns.
func newPersistentSessionAgent(ctx context.Context, primary loop.Config, opts ...session.Option) (*sessionAgent, error) {
	if err := ctx.Err(); err != nil {
		return nil, &session.SessionError{Kind: session.SessionContextDone, Cause: err}
	}
	rootCtx, cancel := context.WithCancel(context.Background())
	sess, err := session.New(rootCtx, primary, opts...)
	if err != nil {
		cancel()
		return nil, err
	}
	return &sessionAgent{session: sess, rootCtx: rootCtx, cancel: cancel, acceptsImages: primary.Model.Caps.AcceptsImages}, nil
}

// newRestoredSessionAgent constructs a sessionAgent over a RESTORED session via
// session.Restore: it mirrors newSessionAgent's lifetime ownership (background-derived
// root, fail-fast on a cancelled caller ctx, cancel-on-failure) but seeds the primary loop
// from the durable log instead of minting a fresh one. Restore acquires + owns the
// single-writer lease internally and installs its own lease-release-on-Shutdown hook. The
// caller (persistence.go) wires the replayer + restored ids + GC teardown after this
// returns so ReplayBacklog can repaint the restored transcript.
func newRestoredSessionAgent(ctx context.Context, primary loop.Config, sessionID uuid.UUID, store *sessionstore.Store, opts ...session.Option) (*sessionAgent, error) {
	if err := ctx.Err(); err != nil {
		return nil, &session.SessionError{Kind: session.SessionContextDone, Cause: err}
	}
	rootCtx, cancel := context.WithCancel(context.Background())
	sess, err := session.Restore(rootCtx, primary, sessionID, store, opts...)
	if err != nil {
		cancel()
		return nil, err
	}
	return &sessionAgent{session: sess, rootCtx: rootCtx, cancel: cancel, acceptsImages: primary.Model.Caps.AcceptsImages}, nil
}

// Submit delivers a multimodal user message FIRE-AND-FORGET as a queueable
// UserInput and returns the InputID — the Cause.CommandID the resulting Reply
// events carry on the session fan-in. The Go error is non-nil only when the command
// could not be handed to the loop (loop gone, or ctx done); the turn outcome is
// observed on the Subscribe stream, never returned. Delegates to the session.
func (a *sessionAgent) Submit(ctx context.Context, blocks []content.Block) (uuid.UUID, error) {
	return a.session.Submit(ctx, blocks)
}

// SubmitToLoop is the loop-targeted counterpart of Submit: it delivers a user message
// FIRE-AND-FORGET to a SPECIFIC loop (the modern viewport's focused loop) rather than the
// primary, and returns the InputID the resulting Reply events carry (Cause.CommandID). As
// with Submit the Go error is non-nil only when the command could not be handed to the
// loop (loop gone, unknown loop id, or ctx done); the turn outcome is observed on the
// Subscribe stream, never returned. Delegates to the session.
func (a *sessionAgent) SubmitToLoop(ctx context.Context, loopID uuid.UUID, blocks []content.Block) (uuid.UUID, error) {
	return a.session.SubmitToLoop(ctx, loopID, blocks)
}

// Subscribe attaches a whole-session event consumer to the session fan-in with
// filter and returns its event.Subscription. It is the seam a TUI/CLI uses to
// observe events across the whole session (every loop, spanning turns). The caller
// Closes the returned subscription when done.
func (a *sessionAgent) Subscribe(filter event.EventFilter) (event.Subscription, error) {
	return a.session.SubscribeEvents(filter)
}

// PrimaryLoopID returns the session's primary loop id, so a subscriber can build its
// EventFilter (primary-only Ephemeral + all-loop Enduring).
func (a *sessionAgent) PrimaryLoopID() uuid.UUID { return a.session.PrimaryLoopID() }

// ReplayBacklog returns the RESTORED session's historical Enduring events for the TUI's
// cold-restore repaint, in session order. A NEW or headless session has no replayer wired
// (a.replayer is nil), so this returns nil and the TUI skips the repaint — the
// new/headless behavior is unchanged. A RESTORED session opens the primary loop's Enduring
// view (session subject + that loop's event subject), drains the EventCursor to io.EOF into
// a materialized slice, and surfaces the journal's typed fail-secure errors (a
// missing/corrupt offload object) unchanged — the TUI shows a non-fatal restore-error
// notice; the live stream is unaffected. ctx bounds the read.
func (a *sessionAgent) ReplayBacklog(ctx context.Context) ([]event.Event, error) {
	if a.replayer == nil {
		return nil, nil // not a restore (new/headless session) — nothing to repaint
	}
	cursor, err := a.replayer.Open(ctx, journal.ReplayRequest{
		SessionID: a.restoredSessionID,
		LoopID:    a.restoredPrimaryLoopID,
		From:      journal.Beginning(),
		Follow:    false, // cold restore: io.EOF at the backlog end
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close() }()

	var out []event.Event
	for {
		ev, _, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err // typed fail-secure error (object missing/corrupt) — surfaced unchanged
		}
		out = append(out, ev)
	}
}

// SessionID returns the underlying session's id — the composition root reads it to print
// the session being resumed and to key the catalog/lease. It is read-only identity.
func (a *sessionAgent) SessionID() uuid.UUID { return a.session.SessionID }

// Interrupt cancels the running turn. Returns true if a turn was cancelled.
func (a *sessionAgent) Interrupt(ctx context.Context) (bool, error) {
	return a.session.Interrupt(ctx)
}

// AcceptsImages reports whether the underlying model accepts image blocks, captured
// from the primary spec at construction.
func (a *sessionAgent) AcceptsImages() bool { return a.acceptsImages }

// GateNotOpenError reports that no open gate matches the tool-execution id a gate
// reply addressed. It is fail-secure: an Approve/Deny/ProvideAnswer for a call with
// no live gate (already resolved, unknown, or never opened) touches nothing and
// returns this rather than silently succeeding. It is errors.As-able.
type GateNotOpenError struct{ ToolExecutionID uuid.UUID }

func (e *GateNotOpenError) Error() string {
	return "swe: no open gate for tool-execution id " + e.ToolExecutionID.String()
}

// gateIDForCall resolves the session gate.ID of the open gate whose subject matches
// toolExecutionID. The harness mints a fresh gate.ID per gate (distinct from the
// tool-execution id the TUI keys on), so the reply path must translate. A
// tool-execution id is a per-call UUID unique across loops, so matching on it alone
// is unambiguous (the loop id the TUI also carries is redundant here — RespondGate
// routes by the gate's own stored route). The loop's install-before-emit ordering
// guarantees the gate is listable by the time the TUI observes the request event, so
// this lookup never races the open. An unmatched call fails secure with
// *GateNotOpenError.
func (a *sessionAgent) gateIDForCall(ctx context.Context, toolExecutionID uuid.UUID) (gate.ID, error) {
	for _, g := range a.session.ListGates(ctx) {
		if g.Subject.ToolExecutionID == toolExecutionID {
			return g.ID, nil
		}
	}
	return gate.ID{}, &GateNotOpenError{ToolExecutionID: toolExecutionID}
}

// Approve resolves a pending tool-call permission gate, granting it at scope. It
// resolves the gate.ID from callID (the tool-execution id) and submits a
// user-sourced "approve" GateResponse to the session. loopID is retained for the
// tui.Agent contract but is not needed for routing — RespondGate dispatches by the
// gate's stored route.
func (a *sessionAgent) Approve(ctx context.Context, loopID, callID uuid.UUID, scope tool.ApprovalScope) error {
	_ = loopID
	gateID, err := a.gateIDForCall(ctx, callID)
	if err != nil {
		return err
	}
	scopeValue, ok := tool.ApprovalScopeValue(scope)
	if !ok {
		return &GateNotOpenError{ToolExecutionID: callID}
	}
	rawScope, err := json.Marshal(scopeValue)
	if err != nil {
		return err
	}
	return a.session.RespondGate(ctx, gate.GateResponse{
		GateID: gateID,
		Action: "approve",
		Values: map[string]json.RawMessage{"scope": rawScope},
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	})
}

// Deny resolves a pending tool-call permission gate by failing it closed
// (fail-secure); nothing is persisted. It resolves the gate.ID from callID and
// submits a user-sourced "deny" GateResponse. loopID is retained for the tui.Agent
// contract but is not needed for routing.
func (a *sessionAgent) Deny(ctx context.Context, loopID, callID uuid.UUID) error {
	_ = loopID
	gateID, err := a.gateIDForCall(ctx, callID)
	if err != nil {
		return err
	}
	return a.session.RespondGate(ctx, gate.GateResponse{
		GateID: gateID,
		Action: "deny",
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	})
}

// ProvideAnswer supplies the user's reply to a pending AskUser request. It resolves
// the gate.ID from callID and submits a user-sourced "answer" GateResponse carrying
// the reply. loopID is retained for the tui.Agent contract but is not needed for
// routing.
func (a *sessionAgent) ProvideAnswer(ctx context.Context, loopID, callID uuid.UUID, answer string) error {
	_ = loopID
	gateID, err := a.gateIDForCall(ctx, callID)
	if err != nil {
		return err
	}
	rawAnswer, err := json.Marshal(answer)
	if err != nil {
		return err
	}
	return a.session.RespondGate(ctx, gate.GateResponse{
		GateID: gateID,
		Action: "answer",
		Values: map[string]json.RawMessage{"answer": rawAnswer},
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	})
}

// Close gracefully shuts the session down and releases the session's root context.
// It blocks until the actor exits (or ctx is done), then cancels the root as a
// backstop so the actor goroutine cannot leak even if Shutdown timed out on ctx.
// Cancelling the root also tears down every in-session sub-loop (they run under the
// same session root). Safe to call more than once.
//
// For a PERSISTED agent it then runs the composition-root teardown ONCE (stopping the GC
// ticker) — AFTER session.Shutdown so the journal has finished its last append before
// teardown. The single-writer lease is released by the SESSION on Shutdown (the
// WithLeaseRelease hook for a new session, or the hook Restore installed), so teardown
// owns only the GC lifecycle. The teardown runs even when Shutdown returns an error. A
// teardown error is joined onto the Shutdown error so neither is lost.
func (a *sessionAgent) Close(ctx context.Context) error {
	err := a.session.Shutdown(ctx)
	a.cancel()
	if a.teardown != nil {
		a.teardownOnce.Do(func() {
			if terr := a.teardown(ctx); terr != nil {
				err = errors.Join(err, terr)
			}
		})
	}
	return err
}
