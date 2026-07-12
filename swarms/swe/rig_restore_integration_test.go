//go:build integration

package swe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/inference"
	"github.com/looprig/storage"
	"github.com/looprig/swe/agents/operator"
)

type failingSnapshotBlobs struct {
	storage.Blobs
	mu       sync.Mutex
	fail     bool
	attempts chan struct{}
}

func (b *failingSnapshotBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	b.mu.Lock()
	fail := b.fail
	b.mu.Unlock()
	select {
	case b.attempts <- struct{}{}:
	default:
	}
	if fail {
		return errors.New("injected workspace snapshot failure")
	}
	return b.Blobs.Put(ctx, key, r)
}

func (b *failingSnapshotBlobs) setFail(fail bool) {
	b.mu.Lock()
	b.fail = fail
	b.mu.Unlock()
}

// concurrentManagedScript is the channel-controlled counterpart to managedScript. It
// intentionally does not serialize Stream callbacks: async child inference must be able to
// block while the parent continues issuing status/wait/interrupt actions.
type concurrentManagedScript struct {
	fn func(context.Context, inference.Request) ([]content.Chunk, error)
}

func (*concurrentManagedScript) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("concurrentManagedScript.Invoke not used")
}

func (s *concurrentManagedScript) Stream(ctx context.Context, req inference.Request) (*inference.StreamReader[content.Chunk], error) {
	chunks, err := s.fn(ctx, req)
	if err != nil {
		return nil, err
	}
	i := 0
	return inference.NewStreamReader(func() (content.Chunk, error) {
		if i == len(chunks) {
			return nil, io.EOF
		}
		chunk := chunks[i]
		i++
		return chunk, nil
	}, nil), nil
}

// TestRigRestoreStateWorkspaceAndContinuation exercises the CLI-shaped persistence path
// with two genuinely distinct fsstore instances. It checks every restored projection before
// the first post-restore submit, then proves Submit follows the restored active delegate.
func TestRigRestoreStateWorkspaceAndContinuation(t *testing.T) {
	dataDir := t.TempDir()
	workspace := t.TempDir()
	t.Chdir(workspace)

	phase := "initial"
	primaryCalls := 0
	var restoredEffort inference.Effort
	client := &managedScript{}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if strings.Contains(req.System, operatorDelegation) {
			primaryCalls++
			if primaryCalls == 1 {
				return toolCall("restore-state-child", `{"agent":"operator","message":"work","wait":true}`), nil
			}
			return finalText("operator work complete"), nil
		}
		if phase == "restored" {
			restoredEffort = req.Model.Sampling.Effort
			return finalText("continued on restored delegate"), nil
		}
		return finalText("delegate work complete"), nil
	}

	f1, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	a1, err := f1.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{}, Config{SecurityCeiling: DefaultSecurityMode})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := a1.SessionID()
	_, observed := runManagedTurnObserved(t, a1, "perform operator work")
	var childID uuid.UUID
	for _, ev := range observed {
		if started, ok := ev.(event.LoopStarted); ok && !started.Cause.Coordinates.LoopID.IsZero() {
			childID = started.LoopID
		}
	}
	if childID.IsZero() {
		t.Fatal("managed operator work did not create a delegate")
	}
	if err := a1.sess.SetActiveLoop(context.Background(), childID); err != nil {
		t.Fatalf("SetActiveLoop(delegate): %v", err)
	}
	controller, ok := a1.sess.LoopController(childID)
	if !ok {
		t.Fatal("delegate controller not found")
	}
	changedModel := testModel()
	changedModel.Name = "restored-state-model"
	// SWE production definitions deliberately declare only their base mode. Selecting that
	// declared mode still traverses the real mode-control boundary without inventing a mode.
	if err := controller.SetMode(context.Background(), loop.ModeName("")); err != nil {
		t.Fatalf("SetMode(base): %v", err)
	}
	// Direct inference changes follow the mode selection because a mode change intentionally
	// resets model and effort; restore must reproduce that same last-write-wins precedence.
	if err := controller.Change(context.Background(), loop.ChangeModel(changedModel), loop.ChangeEffort(inference.EffortHigh)); err != nil {
		t.Fatalf("Change(delegate inference): %v", err)
	}
	if err := a1.sess.SetSecurityCeiling(context.Background(), ceiling.Level(1)); err != nil {
		t.Fatalf("SetSecurityCeiling(read-only): %v", err)
	}
	const filename, body = "restore-state.txt", "checkpointed before shutdown"
	if err := os.WriteFile(filepath.Join(workspace, filename), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a1.sess.CheckpointWorkspace(context.Background()); err != nil {
		t.Fatalf("CheckpointWorkspace: %v", err)
	}
	if err := a1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(workspace, filename)); err != nil {
		t.Fatal(err)
	}

	phase = "restored"
	f2, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatalf("fresh NewSessionStoreFactory: %v", err)
	}
	t.Cleanup(func() { _ = f2.Close() })
	a2, err := f2.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{Resume: sessionID}, Config{SecurityCeiling: DefaultSecurityMode})
	if err != nil {
		t.Fatalf("restore from fresh factory: %v", err)
	}
	t.Cleanup(func() { _ = a2.Close(context.Background()) })

	// All assertions in this block intentionally precede the first restored Submit.
	if got := a2.ActiveLoopID(); got != childID {
		t.Errorf("restored active loop = %v, want delegate %v", got, childID)
	}
	child, ok := a2.sess.Loop(childID)
	if !ok {
		t.Fatal("restored delegate missing before submit")
	}
	if got := child.Model().Name; got != changedModel.Name {
		t.Errorf("restored delegate model = %q, want %q", got, changedModel.Name)
	}
	if got := child.Mode(); got != "" {
		t.Errorf("restored delegate mode = %q, want production base mode", got)
	}
	ceilingView, ok := a2.sess.(interface{ CeilingSource() ceiling.Source })
	if !ok || ceilingView.CeilingSource().Current() != ceiling.Level(1) {
		t.Fatalf("restored security ceiling unavailable or incorrect before submit")
	}
	gotBody, err := os.ReadFile(filepath.Join(workspace, filename))
	if err != nil || string(gotBody) != body {
		t.Fatalf("restored workspace before submit = %q, %v; want %q", gotBody, err, body)
	}
	if got := runManagedTurn(t, a2, "continue"); got != "continued on restored delegate" {
		t.Fatalf("restored continuation = %q", got)
	}
	if restoredEffort != inference.EffortHigh {
		t.Fatalf("restored continuation effort = %q, want %q", restoredEffort, inference.EffortHigh)
	}
}

// TestRigRestoreDelegateOwnership uses a fresh fsstore instance to prove the durable owner
// relation, rather than relying on the in-memory restore coverage in managed_delegation_test.
func TestRigRestoreDelegateOwnership(t *testing.T) {
	dataDir := t.TempDir()
	workspace := t.TempDir()
	t.Chdir(workspace)
	phase := "initial"
	step := 0
	var childID uuid.UUID
	var unrelatedResult string
	client := &managedScript{}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !strings.Contains(req.System, operatorDelegation) {
			if phase == "initial" {
				return finalText("initial child"), nil
			}
			return finalText("restored follow-up"), nil
		}
		if phase == "initial" {
			if step == 0 {
				step++
				return toolCall("own-start", `{"agent":"operator","message":"first","wait":true}`), nil
			}
			return finalText("initial parent"), nil
		}
		switch step {
		case 0:
			step++
			return toolCall("own-send", fmt.Sprintf(`{"action":"send","delegate_id":%q,"message":"again","wait":true}`, childID)), nil
		case 1:
			if got := lastToolText(req); got != "restored follow-up" {
				return nil, fmt.Errorf("owned follow-up = %q", got)
			}
			step++
			return toolCall("own-reject", fmt.Sprintf(`{"action":"send","delegate_id":%q,"message":"intrude","wait":true}`, uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))), nil
		default:
			unrelatedResult = lastToolText(req)
			return finalText("ownership checked"), nil
		}
	}

	f1, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	a1, err := f1.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	sid := a1.SessionID()
	_, events := runManagedTurnObserved(t, a1, "start child")
	for _, ev := range events {
		if started, ok := ev.(event.LoopStarted); ok && !started.Cause.Coordinates.LoopID.IsZero() {
			childID = started.LoopID
		}
	}
	if childID.IsZero() {
		t.Fatal("no durable child")
	}
	if err := a1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f1.Close(); err != nil {
		t.Fatal(err)
	}

	phase, step = "restored", 0
	f2, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f2.Close() })
	a2, err := f2.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{Resume: sid}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a2.Close(context.Background()) })
	if got := runManagedTurn(t, a2, "continue"); got != "ownership checked" {
		t.Fatalf("final = %q", got)
	}
	if !strings.Contains(unrelatedResult, "is not owned by this loop") {
		t.Fatalf("unrelated delegate result = %q", unrelatedResult)
	}
}

// TestAsyncDelegatesFSStoreResolveIndependently drives two managed children through the
// production SWE rig over fsstore. Both are started before either request is waited, and
// the waits are intentionally reversed so completion of one request cannot satisfy the
// other request's identity.
func TestAsyncDelegatesFSStoreResolveIndependently(t *testing.T) {
	t.Chdir(t.TempDir())
	step := 0
	var first, second queuedHandle
	var firstWait, secondWait, firstStatus, interrupted string
	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	childCalls := 0
	client := &concurrentManagedScript{}
	client.fn = func(ctx context.Context, req inference.Request) ([]content.Chunk, error) {
		if !strings.Contains(req.System, operatorDelegation) {
			childCalls++ // serialized by the parent barriers below: child 1 enters before child 2 starts
			switch childCalls {
			case 1:
				close(firstEntered)
				select {
				case <-releaseFirst:
					return finalText("independent first child result"), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			default:
				close(secondEntered)
				<-ctx.Done()
				return nil, ctx.Err()
			}
		}
		prior := lastToolText(req)
		switch step {
		case 0:
			step++
			return toolCall("fs-async-1", `{"action":"start","agent":"operator","message":"first","wait":false}`), nil
		case 1:
			var err error
			first, err = parseQueued(prior)
			if err != nil {
				return nil, err
			}
			select {
			case <-firstEntered:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			step++
			return toolCall("fs-async-2", `{"action":"start","agent":"operator","message":"second","wait":false}`), nil
		case 2:
			var err error
			second, err = parseQueued(prior)
			if err != nil {
				return nil, err
			}
			select {
			case <-secondEntered:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			step++
			return toolCall("fs-status-1", fmt.Sprintf(`{"action":"status","delegate_id":%q}`, first.DelegateID)), nil
		case 3:
			firstStatus = prior
			close(releaseFirst)
			step++
			return toolCall("fs-wait-1", fmt.Sprintf(`{"action":"wait","delegate_id":%q,"request_id":%q}`, first.DelegateID, first.RequestID)), nil
		case 4:
			firstWait = prior
			step++
			return toolCall("fs-interrupt-2", fmt.Sprintf(`{"action":"interrupt","delegate_id":%q}`, second.DelegateID)), nil
		case 5:
			interrupted = prior
			step++
			return toolCall("fs-wait-2", fmt.Sprintf(`{"action":"wait","delegate_id":%q,"request_id":%q}`, second.DelegateID, second.RequestID)), nil
		default:
			secondWait = prior
			return finalText("persisted async matrix complete"), nil
		}
	}
	f := newIntegrationFactory(t)
	a, err := f.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	if got := runManagedTurn(t, a, "run two delegates"); got != "persisted async matrix complete" {
		t.Fatalf("final = %q", got)
	}
	if first.DelegateID == second.DelegateID || first.RequestID == second.RequestID {
		t.Fatalf("first=%+v second=%+v, want independent delegate and request ids", first, second)
	}
	if !strings.Contains(firstStatus, first.DelegateID) {
		t.Fatalf("first status = %q", firstStatus)
	}
	if !strings.Contains(firstWait, "independent first child result") {
		t.Fatalf("first wait = %q", firstWait)
	}
	if !strings.Contains(strings.ToLower(secondWait), "interrupt") {
		t.Fatalf("interrupted second wait = %q", secondWait)
	}
	if !strings.Contains(interrupted, second.DelegateID) {
		t.Fatalf("interrupt result = %q, want delegate %s", interrupted, second.DelegateID)
	}
}

// TestManagedDelegateDeclaredModeFSStore uses the production managed-topology shape with
// the one deliberate test-only difference Task 7 permits: the operator leaf declares a
// named mode. It complements the production-definition rejection test by proving a mode
// is accepted only when present in the target definition.
func TestManagedDelegateDeclaredModeFSStore(t *testing.T) {
	t.Chdir(t.TempDir())
	primaryCalls := 0
	var childModel string
	client := &managedScript{}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if strings.Contains(req.System, "mode-test-primary") {
			primaryCalls++
			if primaryCalls == 1 {
				return toolCall("declared-mode", `{"agent":"operator","mode":"build","message":"build it","wait":true}`), nil
			}
			return finalText("declared mode complete"), nil
		}
		childModel = req.Model.Name
		return finalText("mode child complete"), nil
	}
	permission := &typedDelegatePermission{}
	primer, err := loop.Define(
		loop.WithName(operatorPrimaryName),
		loop.WithInference(client, testModel()),
		loop.WithSystem("mode-test-primary"),
		loop.WithPermissionFactory(func(_ context.Context, bindings tool.Bindings) (loop.PermissionGate, error) {
			permission.controller = bindings.Delegate
			return permission, nil
		}),
		loop.WithPolicyRevision("mode-test-primary-v1"),
		loop.WithDelegates(operator.Name),
		loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}),
	)
	if err != nil {
		t.Fatal(err)
	}
	modeModel := testModel()
	modeModel.Name = "declared-build-model"
	leaf, err := loop.Define(
		loop.WithName(operator.Name),
		loop.WithInference(client, testModel()),
		loop.WithModes(loop.Mode{Name: "plan"}, loop.Mode{Name: "build", Model: modeModel, Effort: inference.EffortHigh}),
		loop.WithInitialMode("plan"),
		loop.WithPolicyRevision("mode-test-leaf-v1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	f := newIntegrationFactory(t)
	assembly, err := buildRig([]loop.Definition{primer, leaf}, f.stores, mustCurrentDir(t), Config{}, false)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := assembly.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a, err := newSessionAgent(context.Background(), controller, f.stores.session, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	if got := runManagedTurn(t, a, "use build mode"); got != "declared mode complete" {
		t.Fatalf("final = %q", got)
	}
	if childModel != modeModel.Name {
		t.Fatalf("declared-mode child model = %q, want %q", childModel, modeModel.Name)
	}
}

func mustCurrentDir(t *testing.T) string {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestRigRestoreSnapshotFailureAdmission composes the actual fsstore session journal and
// leases with a deterministic failing workspace blob seam. This keeps the complete SWE
// topology/bindings while proving the two documented snapshot priorities at admission.
func TestRigRestoreSnapshotFailureAdmission(t *testing.T) {
	for _, tc := range []struct {
		name     string
		priority rig.SnapshotPriority
		required bool
	}{
		{name: "required faults future admission", priority: rig.SnapshotRequired, required: true},
		{name: "best effort permits admission and retries", priority: rig.SnapshotBestEffort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			f := newIntegrationFactory(t)
			blobs := &failingSnapshotBlobs{Blobs: f.fs.Backend().Blobs, fail: true, attempts: make(chan struct{}, 4)}
			workspace, err := workspacestore.Open(blobs)
			if err != nil {
				t.Fatal(err)
			}
			client := &managedScript{fn: func(context.Context, inference.Request) ([]content.Chunk, error) {
				return finalText("snapshot turn complete"), nil
			}}
			definitions, err := swarmDefinitions(client, testModel(), Config{})
			if err != nil {
				t.Fatal(err)
			}
			assembly, err := rig.Define(
				rig.WithLoops(definitions...),
				rig.WithPrimers(string(operatorPrimaryName)),
				rig.WithActivePrimer(string(operatorPrimaryName)),
				rig.WithSessionStore(f.stores.session),
				rig.WithExclusiveWorkspace(workspace, root, f.stores.leaser),
				rig.WithSnapshots(rig.SnapshotPolicy{Trigger: rig.SnapshotOnIdle, Priority: tc.priority, Timeout: 5 * time.Second}),
				rig.WithDelegationLimits(rig.DelegationLimits{Depth: operatorSpawnDepth, Quota: operatorSpawnQuota}),
				rig.WithFingerprintFields(operatorFingerprintFields(Config{})),
				rig.WithCeilingFactory(newCeilingFactory(0)),
			)
			if err != nil {
				t.Fatal(err)
			}
			controller, err := assembly.NewSession(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			a, err := newSessionAgent(context.Background(), controller, f.stores.session, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = a.Close(context.Background()) })
			runManagedTurn(t, a, "trigger failing snapshot")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			select {
			case <-blobs.attempts:
			case <-ctx.Done():
				t.Fatalf("snapshot attempt not observed: %v", ctx.Err())
			}
			idle := a.sess.(interface{ WaitIdle(context.Context) error })
			idleErr := idle.WaitIdle(ctx)
			if tc.required {
				if idleErr == nil {
					t.Fatal("required snapshot failure did not fault WaitIdle")
				}
				if _, err := a.Submit(ctx, []content.Block{&content.TextBlock{Text: "must reject"}}); err == nil {
					t.Fatal("required snapshot failure admitted a later submit")
				}
				return
			}
			if idleErr != nil {
				t.Fatalf("best-effort WaitIdle = %v", idleErr)
			}
			blobs.setFail(false)
			runManagedTurn(t, a, "retry snapshot")
			select {
			case <-blobs.attempts:
			case <-ctx.Done():
				t.Fatalf("best-effort snapshot did not retry: %v", ctx.Err())
			}
			if err := idle.WaitIdle(ctx); err != nil {
				t.Fatalf("best-effort retry WaitIdle = %v", err)
			}
		})
	}
}

// TestRigRestoreSiblingOwnershipScopes builds two managed primer parents over the same
// real fsstore session. Each owns one worker. After a fresh-factory restore, parent A can
// still address A's worker but cannot send, wait, or interrupt parent B's real worker.
func TestRigRestoreSiblingOwnershipScopes(t *testing.T) {
	dataDir, root := t.TempDir(), t.TempDir()
	t.Chdir(root)
	client := &managedScript{fn: func(context.Context, inference.Request) ([]content.Chunk, error) {
		return finalText("scoped worker complete"), nil
	}}
	var parentA, parentB tool.DelegateController
	definitions := func(t *testing.T) []loop.Definition {
		t.Helper()
		parent := func(name string, capture *tool.DelegateController) loop.Definition {
			permission := &typedDelegatePermission{}
			def, err := loop.Define(
				loop.WithName(identity.AgentName(name)),
				loop.WithInference(client, testModel()),
				loop.WithPermissionFactory(func(_ context.Context, bindings tool.Bindings) (loop.PermissionGate, error) {
					permission.controller = bindings.Delegate
					*capture = bindings.Delegate
					return permission, nil
				}),
				loop.WithPolicyRevision(name+"-v1"),
				loop.WithDelegates("scoped-worker"),
				loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}),
			)
			if err != nil {
				t.Fatal(err)
			}
			return def
		}
		worker, err := loop.Define(loop.WithName("scoped-worker"), loop.WithInference(client, testModel()), loop.WithPolicyRevision("scoped-worker-v1"))
		if err != nil {
			t.Fatal(err)
		}
		return []loop.Definition{parent("parent-a", &parentA), parent("parent-b", &parentB), worker}
	}
	build := func(t *testing.T, f *SessionStoreFactory) *rig.Rig {
		t.Helper()
		assembly, err := rig.Define(
			rig.WithLoops(definitions(t)...),
			rig.WithPrimers("parent-a", "parent-b"),
			rig.WithActivePrimer("parent-a"),
			rig.WithSessionStore(f.stores.session),
			rig.WithExclusiveWorkspace(f.stores.workspace, root, f.stores.leaser),
			rig.WithSnapshots(rig.SnapshotPolicy{Trigger: rig.SnapshotOnIdle, Priority: rig.SnapshotBestEffort}),
			rig.WithDelegationLimits(rig.DelegationLimits{Depth: 2, Quota: 8}),
			rig.WithFingerprintFields(rig.ConfigFingerprintFields{AgentKind: "swe:scoped-ownership-test"}),
		)
		if err != nil {
			t.Fatal(err)
		}
		return assembly
	}

	f1, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	s1, err := build(t, f1).NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sid := s1.SessionID()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	aChild, err := parentA.Execute(ctx, tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "scoped-worker", Message: "a", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	bChild, err := parentB.Execute(ctx, tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "scoped-worker", Message: "b", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if aChild.DelegateID == bChild.DelegateID {
		t.Fatal("distinct parents received the same child")
	}
	if err := s1.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f1.Close(); err != nil {
		t.Fatal(err)
	}

	parentA, parentB = nil, nil
	f2, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f2.Close() })
	s2, err := build(t, f2).RestoreSession(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Shutdown(context.Background()) })
	if parentA == nil || parentB == nil {
		t.Fatal("restore did not rebind both scoped controllers")
	}
	if got, err := parentA.Execute(ctx, tool.DelegateRequest{Operation: tool.DelegateSend, DelegateID: aChild.DelegateID, Message: "owned", Wait: true}); err != nil || got.Output != "scoped worker complete" {
		t.Fatalf("restored own child send = %+v, %v", got, err)
	}
	for _, request := range []tool.DelegateRequest{
		{Operation: tool.DelegateSend, DelegateID: bChild.DelegateID, Message: "cross-owner", Wait: true},
		{Operation: tool.DelegateWait, DelegateID: bChild.DelegateID, RequestID: &bChild.RequestID},
		{Operation: tool.DelegateInterrupt, DelegateID: bChild.DelegateID},
	} {
		if _, err := parentA.Execute(ctx, request); err == nil || !strings.Contains(err.Error(), "not owned") {
			t.Fatalf("parent A cross-owner %v error = %v, want ownership rejection", request.Operation, err)
		}
	}
}

// TestRigRestoreDelegateGateSandboxRoot restores an active production operator delegate,
// then drives its real Bash tool through the restored ceiling-aware permission/sandbox
// binding. The gate is persisted, routed by the delegate loop id, resolved through the
// adapter, and removed; pwd proves the bound executor retained the restored checkout root.
// This intentionally opens a NEW gate after restore. Gates that were open at a crash are
// non-restorable by contract and restore closes them with CloseRestoreUnavailable because
// their blocked in-process continuation no longer exists.
func TestRigRestoreDelegateGateSandboxRoot(t *testing.T) {
	dataDir, root := t.TempDir(), t.TempDir()
	t.Chdir(root)
	phase, primaryCalls, childCalls := "initial", 0, 0
	var childID uuid.UUID
	var bashResult string
	client := &managedScript{}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if strings.Contains(req.System, operatorDelegation) {
			primaryCalls++
			if primaryCalls == 1 {
				return toolCall("sandbox-child", `{"agent":"operator","message":"prepare","wait":true}`), nil
			}
			return finalText("parent prepared"), nil
		}
		if phase == "initial" {
			return finalText("child prepared"), nil
		}
		childCalls++
		if childCalls == 1 {
			return []content.Chunk{&content.ToolUseChunk{Index: 0, ID: "restored-pwd", Name: "Bash", InputJSON: `{"command":"pwd"}`}}, nil
		}
		bashResult = lastToolText(req)
		return finalText("restored bash complete"), nil
	}
	f1, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	a1, err := f1.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{}, Config{SecurityCeiling: DefaultSecurityMode})
	if err != nil {
		t.Fatal(err)
	}
	sid := a1.SessionID()
	_, events := runManagedTurnObserved(t, a1, "create delegate")
	for _, ev := range events {
		if started, ok := ev.(event.LoopStarted); ok && !started.Cause.Coordinates.LoopID.IsZero() {
			childID = started.LoopID
		}
	}
	if childID.IsZero() {
		t.Fatal("delegate missing")
	}
	if err := a1.sess.SetActiveLoop(context.Background(), childID); err != nil {
		t.Fatal(err)
	}
	if err := a1.sess.SetSecurityCeiling(context.Background(), ceiling.Level(1)); err != nil {
		t.Fatal(err)
	}
	if err := a1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f1.Close(); err != nil {
		t.Fatal(err)
	}

	phase = "restored"
	f2, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f2.Close() })
	a2, err := f2.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{Resume: sid}, Config{SecurityCeiling: DefaultSecurityMode})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a2.Close(context.Background()) })
	if a2.ActiveLoopID() != childID {
		t.Fatalf("restored active loop = %v, want %v", a2.ActiveLoopID(), childID)
	}
	if got := a2.sess.(interface{ CeilingSource() ceiling.Source }).CeilingSource().Current(); got != 1 {
		t.Fatalf("restored ceiling = %d", got)
	}
	sub, err := a2.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	commandID, err := a2.Submit(ctx, []content.Block{&content.TextBlock{Text: "show restored root"}})
	if err != nil {
		t.Fatal(err)
	}
	var turnID, gateID, callID uuid.UUID
	resolved := false
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("restored Bash timed out: %v", ctx.Err())
		case delivery := <-sub.Events():
			switch ev := delivery.Event.(type) {
			case event.TurnStarted:
				if ev.Cause.CommandID == commandID {
					turnID = ev.TurnID
				}
			case event.GateOpened:
				if ev.EventHeader().LoopID != childID {
					t.Fatalf("gate loop = %v, want %v", ev.EventHeader().LoopID, childID)
				}
				gateID = ev.Gate.ID
				callID = ev.Gate.Subject.ToolExecutionID
				if err := a2.Approve(ctx, childID, callID, tool.ScopeOnce); err != nil {
					t.Fatal(err)
				}
			case event.GateResolved:
				if ev.GateID == gateID && !gateID.IsZero() {
					resolved = true
				}
			case event.TurnFailed:
				t.Fatalf("restored Bash turn failed: %v", ev.Err)
			case event.TurnDone:
				if ev.TurnID != turnID || turnID.IsZero() {
					continue
				}
				if gateID.IsZero() || !resolved {
					t.Fatalf("gate lifecycle incomplete: id=%v resolved=%v", gateID, resolved)
				}
				if !strings.Contains(bashResult, root) {
					t.Fatalf("restored Bash pwd result = %q, want root %q", bashResult, root)
				}
				if _, err := a2.gateIDFor(childID, callID); err == nil {
					t.Fatal("resolved gate remained indexed")
				}
				return
			}
		}
	}
}
