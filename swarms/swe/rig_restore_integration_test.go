//go:build integration

package swe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/swe/agents/operator"
)

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
	childTurn := 0
	var first, second queuedHandle
	var firstWait, secondWait, firstStatus, interrupted string
	client := &managedScript{}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !strings.Contains(req.System, operatorDelegation) {
			childTurn++
			return finalText(fmt.Sprintf("independent child result %d", childTurn)), nil
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
			step++
			return toolCall("fs-async-2", `{"action":"start","agent":"operator","message":"second","wait":false}`), nil
		case 2:
			var err error
			second, err = parseQueued(prior)
			if err != nil {
				return nil, err
			}
			step++
			return toolCall("fs-status-1", fmt.Sprintf(`{"action":"status","delegate_id":%q}`, first.DelegateID)), nil
		case 3:
			firstStatus = prior
			step++
			return toolCall("fs-wait-2", fmt.Sprintf(`{"action":"wait","delegate_id":%q,"request_id":%q}`, second.DelegateID, second.RequestID)), nil
		case 4:
			secondWait = prior
			step++
			return toolCall("fs-wait-1", fmt.Sprintf(`{"action":"wait","delegate_id":%q,"request_id":%q}`, first.DelegateID, first.RequestID)), nil
		case 5:
			firstWait = prior
			step++
			return toolCall("fs-interrupt-2", fmt.Sprintf(`{"action":"interrupt","delegate_id":%q}`, second.DelegateID)), nil
		default:
			interrupted = prior
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
	if firstWait == secondWait || !strings.Contains(firstWait, "independent child result") || !strings.Contains(secondWait, "independent child result") {
		t.Fatalf("reversed waits crossed: first=%q second=%q", firstWait, secondWait)
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
