package swe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/looprig/cli/tui"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
)

const sessionAgentInitShutdownTimeout = 10 * time.Second

// sessionAgent adapts a rig session.SessionController to the CLI's tui.Agent surface. It
// owns three things and nothing else: the live session controller, a replay dependency used
// for the one cold restore replay, and a concurrency-safe gate index that folds
// GateOpened/GateResolved events into a (loopID, toolExecutionID)→GateID map so the CLI's
// per-loop Approve/Deny/ProvideAnswer calls can resolve the harness-minted gate id. It holds
// no root context or GC ticker — the rig owns the session lifetime, workspace snapshots, and
// GC — so Close is a single session Shutdown.
type sessionAgent struct {
	sess     session.SessionController
	replayer journal.EventReplayer // nil for a new/headless session with no replay

	isRestore bool
	backlog   []event.Event // restored all-loop Enduring history; nil for new/headless

	// mu guards the gate indexes below. foldGate mutates them from the cold replay (at
	// construction) and from every Subscribe forwarding goroutine; the gate-reply trio reads
	// them. gateKey pairs the loop with the per-call tool-execution id so the SAME
	// ToolExecutionID appearing in two loops resolves to two distinct gates.
	mu      sync.Mutex
	forward map[gateKey]gate.ID
	reverse map[gate.ID]gateKey

	shutdownOnce sync.Once
	shutdownErr  error
}

// gateKey identifies an open gate by the loop that opened it plus the per-call
// tool-execution id, so the same ToolExecutionID in two loops keys two distinct gates.
type gateKey struct {
	loopID          uuid.UUID
	toolExecutionID uuid.UUID
}

// replayOpener is the narrow slice of the session store the adapter needs: open a durable
// event replayer for one session. *sessionstore.Store satisfies it.
type replayOpener interface {
	OpenEventReplayer(uuid.UUID, sessionstore.ReplayRequest) (journal.EventReplayer, error)
}

// newSessionAgent wraps a live rig session controller as a tui.Agent. A restored session
// performs one unnarrowed cold replay, folding every loop's gate events and materializing
// all-loop Enduring history for uniform CLI projections.
func newSessionAgent(ctx context.Context, sess session.SessionController, store replayOpener, isRestore bool) (*sessionAgent, error) {
	a := &sessionAgent{
		sess:      sess,
		isRestore: isRestore,
		forward:   make(map[gateKey]gate.ID),
		reverse:   make(map[gate.ID]gateKey),
	}
	if replayer, err := store.OpenEventReplayer(sess.SessionID(), sessionstore.ReplayRequest{}); err != nil {
		if isRestore {
			return nil, a.failInitialization(err) // restore cannot rebuild without replay
		}
	} else {
		a.replayer = replayer
	}
	if isRestore {
		if err := a.coldReplay(ctx); err != nil {
			return nil, a.failInitialization(err)
		}
		return a, nil
	}
	return a, nil
}

// failInitialization releases controller ownership after the rig has successfully created or
// restored it but the adapter cannot finish initialization. Cleanup is bounded and detached
// from the caller because replay failures commonly arrive through an already-canceled context.
func (a *sessionAgent) failInitialization(primary error) error {
	ctx, cancel := context.WithTimeout(context.Background(), sessionAgentInitShutdownTimeout)
	defer cancel()
	return errors.Join(primary, a.Close(ctx))
}

// coldReplay drains the whole durable log once (all loops), folding every gate event into
// the index and collecting all Enduring events in session order as the restore backlog.
func (a *sessionAgent) coldReplay(ctx context.Context) error {
	if a.replayer == nil {
		return nil
	}
	cursor, err := a.replayer.Open(ctx, journal.ReplayRequest{From: journal.Beginning()})
	if err != nil {
		return err
	}
	defer func() { _ = cursor.Close() }()

	for {
		ev, _, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err // typed fail-secure error (object missing/corrupt) — surfaced unchanged
		}
		a.foldGate(ev)
		if ev.Class() == event.Enduring {
			a.backlog = append(a.backlog, ev)
		}
	}
}

// foldGate updates the gate index from a single event: a GateOpened inserts the
// (loopID, toolExecutionID)→GateID forward entry and the reverse removal entry; a
// GateResolved removes both by the resolved gate id. Any other event is ignored. It is
// concurrency-safe (mu) so the cold replay and every Subscribe forwarder can call it.
func (a *sessionAgent) foldGate(ev event.Event) {
	switch e := ev.(type) {
	case event.GateOpened:
		key := gateKey{loopID: e.EventHeader().LoopID, toolExecutionID: e.Gate.Subject.ToolExecutionID}
		a.mu.Lock()
		a.forward[key] = e.Gate.ID
		a.reverse[e.Gate.ID] = key
		a.mu.Unlock()
	case event.GateResolved:
		a.mu.Lock()
		if key, ok := a.reverse[e.GateID]; ok {
			delete(a.forward, key)
			delete(a.reverse, e.GateID)
		}
		a.mu.Unlock()
	}
}

// Submit delivers a multimodal user message fire-and-forget to the ACTIVE loop and returns
// the InputID the resulting Reply events carry (Cause.CommandID).
func (a *sessionAgent) Submit(ctx context.Context, blocks []content.Block) (uuid.UUID, error) {
	return a.sess.Submit(ctx, blocks)
}

// SubmitToLoop delivers a user message fire-and-forget to a SPECIFIC loop (the focused
// loop) and returns the InputID the resulting Reply events carry.
func (a *sessionAgent) SubmitToLoop(ctx context.Context, loopID uuid.UUID, blocks []content.Block) (uuid.UUID, error) {
	return a.sess.SubmitToLoop(ctx, loopID, blocks)
}

// ActiveLoopID returns the session's current default input target directly.
func (a *sessionAgent) ActiveLoopID() uuid.UUID { return a.sess.ActiveLoop().ID() }

// AcceptsImages reports whether the CURRENT model bound to loopID accepts image blocks. It
// is dynamic and per-target (a multi-loop session runs heterogeneous models) and fails
// closed (false) for an unknown loop.
func (a *sessionAgent) AcceptsImages(loopID uuid.UUID) bool {
	handle, ok := a.sess.Loop(loopID)
	if !ok {
		return false
	}
	return handle.Model().Caps.AcceptsImages
}

// Subscribe returns ONE wrapping subscription over the session fan-in that folds
// GateOpened/GateResolved into the adapter's gate index BEFORE forwarding each event to the
// consumer, so a live gate is answerable the instant the CLI observes its request. It never
// opens a second subscription.
func (a *sessionAgent) Subscribe(filter event.EventFilter) (event.Subscription, error) {
	inner, err := a.sess.SubscribeEvents(filter)
	if err != nil {
		return nil, err
	}
	sub := &gateFoldingSubscription{inner: inner, out: make(chan event.Delivery), done: make(chan struct{})}
	go func() {
		defer close(sub.out)
		for delivery := range inner.Events() {
			a.foldGate(delivery.Event)
			select {
			case sub.out <- delivery:
			case <-sub.done:
				return
			}
		}
	}()
	return sub, nil
}

// ReplayBacklog returns the restored session's all-loop Enduring history materialized at
// construction, in session order. A NEW/headless session returns nil (the CLI skips the
// repaint). ctx is accepted for the contract; the backlog was folded once at restore.
func (a *sessionAgent) ReplayBacklog(_ context.Context) ([]event.Event, error) {
	if !a.isRestore {
		return nil, nil
	}
	return a.backlog, nil
}

// SessionID returns the underlying session's id (the composition root prints it and keys the
// catalog on it).
func (a *sessionAgent) SessionID() uuid.UUID { return a.sess.SessionID() }

// Interrupt cancels the running turn, returning true if a turn was cancelled.
func (a *sessionAgent) Interrupt(ctx context.Context) (bool, error) { return a.sess.Interrupt(ctx) }

// GateNotOpenError reports that no open gate matches the (loop, tool-execution) a gate reply
// addressed. It is fail-secure: an Approve/Deny/ProvideAnswer for a call with no live gate
// touches nothing and returns this. It is errors.As-able.
type GateNotOpenError struct {
	LoopID          uuid.UUID
	ToolExecutionID uuid.UUID
}

func (e *GateNotOpenError) Error() string {
	return "swe: no open gate for loop " + e.LoopID.String() + " tool-execution " + e.ToolExecutionID.String()
}

// gateIDFor resolves the harness gate id of the open gate opened by loopID for callID (the
// per-call tool-execution id), from the adapter-owned index. An unmatched call fails secure
// with *GateNotOpenError.
func (a *sessionAgent) gateIDFor(loopID, callID uuid.UUID) (gate.ID, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id, ok := a.forward[gateKey{loopID: loopID, toolExecutionID: callID}]; ok {
		return id, nil
	}
	return gate.ID{}, &GateNotOpenError{LoopID: loopID, ToolExecutionID: callID}
}

// Approve resolves a pending tool-call permission gate, granting it at scope. loopID names
// the gate-opening loop; callID is the tool-execution id.
func (a *sessionAgent) Approve(ctx context.Context, loopID, callID uuid.UUID, scope tool.ApprovalScope) error {
	gateID, err := a.gateIDFor(loopID, callID)
	if err != nil {
		return err
	}
	scopeValue, ok := tool.ApprovalScopeValue(scope)
	if !ok {
		return &GateNotOpenError{LoopID: loopID, ToolExecutionID: callID}
	}
	rawScope, err := json.Marshal(scopeValue)
	if err != nil {
		return err
	}
	return a.sess.RespondGate(ctx, gate.GateResponse{
		GateID: gateID,
		Action: "approve",
		Values: map[string]json.RawMessage{"scope": rawScope},
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	})
}

// Deny resolves a pending tool-call permission gate by failing it closed (fail-secure).
func (a *sessionAgent) Deny(ctx context.Context, loopID, callID uuid.UUID) error {
	gateID, err := a.gateIDFor(loopID, callID)
	if err != nil {
		return err
	}
	return a.sess.RespondGate(ctx, gate.GateResponse{
		GateID: gateID,
		Action: "deny",
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	})
}

// ProvideAnswer supplies the user's reply to a pending AskUser request.
func (a *sessionAgent) ProvideAnswer(ctx context.Context, loopID, callID uuid.UUID, answer string) error {
	gateID, err := a.gateIDFor(loopID, callID)
	if err != nil {
		return err
	}
	rawAnswer, err := json.Marshal(answer)
	if err != nil {
		return err
	}
	return a.sess.RespondGate(ctx, gate.GateResponse{
		GateID: gateID,
		Action: "answer",
		Values: map[string]json.RawMessage{"answer": rawAnswer},
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	})
}

// Close shuts the session down exactly once (the rig owns every other lifetime concern — the
// workspace lease, snapshots, and GC — so there is no second root or watcher cancel).
func (a *sessionAgent) Close(ctx context.Context) error {
	a.shutdownOnce.Do(func() { a.shutdownErr = a.sess.Shutdown(ctx) })
	return a.shutdownErr
}

// CompactToLoop forwards a manual compaction request to the exact loop selected
// by the CLI. The session owns command identity and error semantics.
func (a *sessionAgent) CompactToLoop(ctx context.Context, loopID uuid.UUID) (uuid.UUID, error) {
	return a.sess.CompactToLoop(ctx, loopID)
}

// gateFoldingSubscription is the single wrapping event.Subscription Subscribe returns. Its
// forwarding goroutine folds each event's gate transitions into the adapter index before
// handing the event to the consumer channel; Close tears down both the inner subscription
// and the forwarder (via done) so a consumer that stops reading cannot leak the goroutine.
type gateFoldingSubscription struct {
	inner event.Subscription
	out   chan event.Delivery
	done  chan struct{}
	once  sync.Once
}

func (s *gateFoldingSubscription) Events() <-chan event.Delivery { return s.out }

func (s *gateFoldingSubscription) Close() error {
	s.once.Do(func() { close(s.done) })
	return s.inner.Close()
}

func (s *gateFoldingSubscription) Err() error { return s.inner.Err() }

// compile-time assertions: the adapter satisfies the CLI's tui.Agent surface, and the
// wrapping subscription satisfies event.Subscription.
var (
	_ tui.Agent          = (*sessionAgent)(nil)
	_ event.Subscription = (*gateFoldingSubscription)(nil)
)
