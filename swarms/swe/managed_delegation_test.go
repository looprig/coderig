package swe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/tools"
	"github.com/looprig/inference"
	"github.com/looprig/storage/memstore"
	"github.com/looprig/swe/agents/operator"
)

// managedScript is a deterministic provider fake that drives the model-facing Subagent
// tool. The callback receives the real bound inference request, including injected tools
// and prior tool results; it therefore observes the composed SWE rig rather than replacing
// delegation with a test spawner.
type managedScript struct {
	mu sync.Mutex
	fn func(context.Context, inference.Request) ([]content.Chunk, error)
}

func (*managedScript) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("managedScript.Invoke not used")
}

func (s *managedScript) Stream(ctx context.Context, req inference.Request) (*inference.StreamReader[content.Chunk], error) {
	chunks, err := func() ([]content.Chunk, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.fn(ctx, req)
	}()
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

func toolCall(id, input string) []content.Chunk {
	return []content.Chunk{&content.ToolUseChunk{Index: 0, ID: id, Name: "Subagent", InputJSON: input}}
}

func finalText(text string) []content.Chunk { return []content.Chunk{&content.TextChunk{Text: text}} }

func lastToolText(req inference.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		msg, ok := req.Messages[i].(*content.ToolResultMessage)
		if !ok {
			continue
		}
		for _, block := range msg.Blocks {
			if text, ok := block.(*content.TextBlock); ok {
				return text.Text
			}
		}
	}
	return ""
}

type queuedHandle struct {
	DelegateID string `json:"delegate_id"`
	RequestID  string `json:"request_id"`
}

func parseQueued(text string) (queuedHandle, error) {
	var got queuedHandle
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		return queuedHandle{}, fmt.Errorf("queued Subagent result %q: %w", text, err)
	}
	if got.DelegateID == "" || got.RequestID == "" {
		return queuedHandle{}, fmt.Errorf("queued Subagent result missing ids: %q", text)
	}
	return got, nil
}

func runManagedTurn(t *testing.T, agent *sessionAgent, prompt string) string {
	t.Helper()
	text, _ := runManagedTurnObserved(t, agent, prompt)
	return text
}

func runManagedTurnObserved(t *testing.T, agent *sessionAgent, prompt string) (string, []event.Event) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	sub, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	commandID, err := agent.Submit(ctx, []content.Block{&content.TextBlock{Text: prompt}})
	if err != nil {
		t.Fatal(err)
	}
	var turnID uuid.UUID
	var observed []event.Event
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("turn timed out after events %s: %v", eventTypes(observed), ctx.Err())
		case delivery := <-sub.Events():
			observed = append(observed, delivery.Event)
			switch ev := delivery.Event.(type) {
			case event.TurnStarted:
				if ev.Cause.CommandID == commandID {
					turnID = ev.TurnID
				}
			case event.TurnDone:
				if ev.TurnID == turnID && !turnID.IsZero() {
					return aiMessageText(ev.Message), observed
				}
			case event.TurnFailed:
				if ev.TurnID == turnID && !turnID.IsZero() {
					t.Fatalf("turn failed: %v", ev.Err)
				}
			}
		}
	}
}

func eventTypes(events []event.Event) string {
	names := make([]string, len(events))
	for i, ev := range events {
		names[i] = fmt.Sprintf("%T", ev)
	}
	return strings.Join(names, ",")
}

func toolNamesFromRequest(req inference.Request) []string {
	names := make([]string, len(req.Tools))
	for i := range req.Tools {
		names[i] = req.Tools[i].Name
	}
	sort.Strings(names)
	return names
}

// TestOperatorPermissionAntiDrift binds the actual SWE primer and operator leaf
// definitions and compares their private permission gates. The primer's wrapper may
// add exactly the rig-owned Subagent approval; every ordinary tool decision and grant
// must remain identical to the operator leaf's base policy.
func TestOperatorPermissionAntiDrift(t *testing.T) {
	t.Parallel()
	defs := swarmDefs(t, Config{})
	root := t.TempDir()
	bind := func(def loop.Definition) loop.BoundDefinition {
		t.Helper()
		bound, err := def.Bind(context.Background(), tool.Bindings{
			SessionID: mustBindingID(t),
			LoopID:    mustBindingID(t),
			Ceiling:   ceiling.New(),
			Workspace: &tool.WorkspaceBinding{
				Root:         root,
				Coordinator:  &testWorkspaceCoordinator{},
				Observations: tools.NewObservations(),
			},
		})
		if err != nil {
			t.Fatalf("Bind(%s): %v", def.Name(), err)
		}
		return bound
	}
	primer, leaf := bind(defs[0]), bind(defs[1])

	primerTools := invokableToolsByName(t, primer.Tools())
	leafTools := invokableToolsByName(t, leaf.Tools())
	for _, tc := range []struct {
		name string
		args string
	}{
		{name: "Todo", args: `{"action":"list"}`},
		{name: "Bash", args: `{"command":"git status --short"}`},
		{name: "WriteFile", args: `{"path":"notes.txt","content":"x"}`},
	} {
		primerTool, primerOK := primerTools[tc.name]
		leafTool, leafOK := leafTools[tc.name]
		if !primerOK || !leafOK {
			t.Fatalf("%s binding: primer=%v leaf=%v", tc.name, primerOK, leafOK)
		}
		beforePrimer := primer.Permission().Check(context.Background(), primerTool, tc.name, tc.args)
		beforeLeaf := leaf.Permission().Check(context.Background(), leafTool, tc.name, tc.args)
		if beforePrimer != beforeLeaf {
			t.Errorf("%s pre-grant effect: primer=%v leaf=%v", tc.name, beforePrimer, beforeLeaf)
		}
		primerErr := primer.Permission().Grant(context.Background(), tc.name, tc.args, tool.ScopeSession)
		leafErr := leaf.Permission().Grant(context.Background(), tc.name, tc.args, tool.ScopeSession)
		if !sameError(primerErr, leafErr) {
			t.Errorf("%s Grant errors: primer=%v leaf=%v", tc.name, primerErr, leafErr)
		}
		afterPrimer := primer.Permission().Check(context.Background(), primerTool, tc.name, tc.args)
		afterLeaf := leaf.Permission().Check(context.Background(), leafTool, tc.name, tc.args)
		if afterPrimer != afterLeaf {
			t.Errorf("%s post-grant effect: primer=%v leaf=%v", tc.name, afterPrimer, afterLeaf)
		}
	}

	primerSpoof := primer.Permission().Check(context.Background(), primerTools["Todo"], managedSubagentToolName, `{}`)
	leafSpoof := leaf.Permission().Check(context.Background(), leafTools["Todo"], managedSubagentToolName, `{}`)
	if primerSpoof != leafSpoof || primerSpoof == loop.EffectAutoApprove {
		t.Errorf("name-spoofed Todo effect: primer=%v leaf=%v, want identical non-auto result", primerSpoof, leafSpoof)
	}
	if _, ok := leafTools[managedSubagentToolName]; ok {
		t.Fatal("operator leaf received Subagent")
	}
}

func invokableToolsByName(t *testing.T, invokables []tool.InvokableTool) map[string]tool.InvokableTool {
	t.Helper()
	result := make(map[string]tool.InvokableTool, len(invokables))
	for _, invokable := range invokables {
		info, err := invokable.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		result[info.Name] = invokable
	}
	return result
}

func sameError(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return reflect.TypeOf(a) == reflect.TypeOf(b) && a.Error() == b.Error()
}

type permissionCapabilityProbe struct {
	decision loop.PermissionDecision
	grants   []string
}

func (p *permissionCapabilityProbe) Check(context.Context, tool.InvokableTool, string, string) loop.Effect {
	return p.decision.Effect
}

func (p *permissionCapabilityProbe) CheckDecision(context.Context, tool.InvokableTool, string, string) loop.PermissionDecision {
	return p.decision
}

func (*permissionCapabilityProbe) Grant(context.Context, string, string, tool.ApprovalScope) error {
	return nil
}

func (p *permissionCapabilityProbe) ApprovedGrants(context.Context, string, string) []string {
	return append([]string(nil), p.grants...)
}

type inertDelegateController struct{}

func (inertDelegateController) Execute(context.Context, tool.DelegateRequest) (tool.DelegateResult, error) {
	return tool.DelegateResult{}, nil
}

// TestManagedPrimerPermissionPreservesOptionalCapabilities guards the optional interfaces
// used for durable decision reasons and escalation-grant re-minting. Only a structurally
// genuine bound Subagent gets the primer-specific approval; a name-spoofed ordinary tool
// follows the base gate.
func TestManagedPrimerPermissionPreservesOptionalCapabilities(t *testing.T) {
	t.Parallel()
	base := &permissionCapabilityProbe{
		decision: loop.PermissionDecision{Effect: loop.EffectAsk, Reason: "base-reason"},
		grants:   []string{"grant-a", "grant-b"},
	}
	wrapped := managedPrimerPermission{PermissionGate: base}
	ordinary := tools.NewTodo()

	decision := wrapped.CheckDecision(context.Background(), ordinary, "Todo", `{"action":"list"}`)
	if decision != base.decision {
		t.Fatalf("ordinary decision = %+v, want %+v", decision, base.decision)
	}
	if grants := wrapped.ApprovedGrants(context.Background(), "Todo", `{"action":"list"}`); !slices.Equal(grants, base.grants) {
		t.Fatalf("ordinary grants = %v, want %v", grants, base.grants)
	}
	if got := wrapped.Check(context.Background(), ordinary, managedSubagentToolName, `{}`); got != base.decision.Effect {
		t.Fatalf("name-spoofed Todo effect = %v, want delegated %v", got, base.decision.Effect)
	}

	subagentDef := tools.Subagent(loop.DelegationManaged, nil)
	primer, err := swarmDefs(t, Config{})[0].Bind(context.Background(), tool.Bindings{
		SessionID: mustBindingID(t),
		LoopID:    mustBindingID(t),
		Ceiling:   ceiling.New(),
		Workspace: &tool.WorkspaceBinding{
			Root:         t.TempDir(),
			Coordinator:  &testWorkspaceCoordinator{},
			Observations: tools.NewObservations(),
		},
		Delegate:   inertDelegateController{},
		ExtraTools: []tool.Definition{subagentDef},
	})
	if err != nil {
		t.Fatalf("bind production primer with injected Subagent: %v", err)
	}
	subagent, ok := invokableToolsByName(t, primer.Tools())[managedSubagentToolName]
	if !ok {
		t.Fatal("production primer binding has no injected Subagent")
	}
	if got := primer.Permission().Check(context.Background(), subagent, managedSubagentToolName, `{}`); got != loop.EffectAutoApprove {
		t.Fatalf("bound Subagent effect = %v, want auto-approve", got)
	}
	decisionGate, ok := primer.Permission().(interface {
		CheckDecision(context.Context, tool.InvokableTool, string, string) loop.PermissionDecision
	})
	if !ok {
		t.Fatal("production primer permission dropped CheckDecision")
	}
	if got := decisionGate.CheckDecision(context.Background(), subagent, managedSubagentToolName, `{}`); got.Effect != loop.EffectAutoApprove || got.Reason != managedSubagentDecisionReason {
		t.Fatalf("bound Subagent decision = %+v, want explicit auto-approve reason", got)
	}
	grantGate, ok := primer.Permission().(interface {
		ApprovedGrants(context.Context, string, string) []string
	})
	if !ok {
		t.Fatal("production primer permission dropped ApprovedGrants")
	}
	if grants := grantGate.ApprovedGrants(context.Background(), managedSubagentToolName, `{}`); len(grants) != 0 {
		t.Fatalf("bound Subagent grants = %v, want none", grants)
	}
}

// TestOperatorTopologyComposed proves the production primer is the sole delegation-capable
// loop and compares what the provider actually receives on the primer and operator leaf.
func TestOperatorTopologyComposed(t *testing.T) {
	t.Parallel()
	var primaryReq, leafReq inference.Request
	primaryCalls := 0
	client := &managedScript{}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if strings.Contains(req.System, operatorDelegation) {
			primaryReq = req
			primaryCalls++
			if primaryCalls == 1 {
				return toolCall("topology-start", `{"agent":"operator","message":"inspect","wait":true}`), nil
			}
			return finalText("topology done"), nil
		}
		leafReq = req
		return finalText("leaf done"), nil
	}
	agent := newTestAgent(t, client, Config{})
	if got := runManagedTurn(t, agent, "go"); got != "topology done" {
		t.Fatalf("primary final = %q", got)
	}

	primaryTools := toolNamesFromRequest(primaryReq)
	leafTools := toolNamesFromRequest(leafReq)
	if !slices.Contains(primaryTools, "Subagent") {
		t.Fatalf("bound primary tools = %v, want injected Subagent", primaryTools)
	}
	if slices.Contains(leafTools, "Subagent") {
		t.Fatalf("bound operator leaf tools = %v, must not contain Subagent", leafTools)
	}
	withoutSubagent := slices.DeleteFunc(append([]string(nil), primaryTools...), func(name string) bool { return name == "Subagent" })
	if !slices.Equal(withoutSubagent, leafTools) {
		t.Fatalf("bound primary-minus-Subagent tools = %v, leaf = %v", withoutSubagent, leafTools)
	}
	if got := strings.Replace(primaryReq.System, operatorDelegation, "", 1); got != leafReq.System {
		t.Fatal("bound primary-minus-delegation prompt differs from operator leaf prompt")
	}
}

// TestManagedSubagentComposed covers synchronous completion and start validation through the
// actual injected tool. Refusals must not register a child or emit LoopStarted.
func TestManagedSubagentComposed(t *testing.T) {
	t.Run("wait true returns child final text", func(t *testing.T) {
		t.Parallel()
		calls := 0
		var observed string
		client := &managedScript{}
		client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
			if !strings.Contains(req.System, operatorDelegation) {
				return finalText("child final text"), nil
			}
			calls++
			if calls == 1 {
				return toolCall("sync-start", `{"agent":"reviewer","message":"review","wait":true}`), nil
			}
			observed = lastToolText(req)
			return finalText("parent final"), nil
		}
		agent := newTestAgent(t, client, Config{})
		runManagedTurn(t, agent, "go")
		if observed != "child final text" {
			t.Fatalf("wait=true tool result = %q", observed)
		}
	})

	for _, tc := range []struct{ name, args, want string }{
		{"unknown agent", `{"agent":"ghost","message":"go"}`, "not an authorized delegate"},
		{"nonempty mode", `{"agent":"operator","mode":"build","message":"go"}`, "is not declared"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			var result string
			client := &managedScript{}
			client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
				calls++
				if calls == 1 {
					return toolCall("invalid-start", tc.args), nil
				}
				result = lastToolText(req)
				return finalText("done"), nil
			}
			agent := newTestAgent(t, client, Config{})
			_, observed := runManagedTurnObserved(t, agent, "go")
			if !strings.Contains(result, tc.want) {
				t.Fatalf("tool result = %q, want %q", result, tc.want)
			}
			if got := countLoopStarted(observed); got != 0 {
				t.Fatalf("child LoopStarted count = %d, want 0", got)
			}
		})
	}
}

// TestAsyncDelegateComposed drives the full managed action surface and proves the start and
// follow-up request ids resolve independently on the same owned child.
func TestAsyncDelegateComposed(t *testing.T) {
	t.Parallel()
	step := 0
	var started, sent queuedHandle
	var statusResult, startWaitResult, sendWaitResult, interruptResult string
	childTurn := 0
	client := &managedScript{}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !strings.Contains(req.System, operatorDelegation) {
			childTurn++
			return finalText(fmt.Sprintf("child answer %d", childTurn)), nil
		}
		prior := lastToolText(req)
		switch step {
		case 0:
			step++
			return toolCall("async-start", `{"action":"start","agent":"operator","message":"first","wait":false}`), nil
		case 1:
			var err error
			started, err = parseQueued(prior)
			if err != nil {
				return nil, err
			}
			step++
			return toolCall("async-status", fmt.Sprintf(`{"action":"status","delegate_id":%q}`, started.DelegateID)), nil
		case 2:
			statusResult = prior
			step++
			return toolCall("async-send", fmt.Sprintf(`{"action":"send","delegate_id":%q,"message":"second","wait":false}`, started.DelegateID)), nil
		case 3:
			var err error
			sent, err = parseQueued(prior)
			if err != nil {
				return nil, err
			}
			step++
			return toolCall("async-wait-start", fmt.Sprintf(`{"action":"wait","delegate_id":%q,"request_id":%q}`, started.DelegateID, started.RequestID)), nil
		case 4:
			startWaitResult = prior
			step++
			return toolCall("async-wait-send", fmt.Sprintf(`{"action":"wait","delegate_id":%q,"request_id":%q}`, sent.DelegateID, sent.RequestID)), nil
		case 5:
			sendWaitResult = prior
			step++
			return toolCall("async-interrupt", fmt.Sprintf(`{"action":"interrupt","delegate_id":%q}`, started.DelegateID)), nil
		default:
			interruptResult = prior
			return finalText("managed actions done"), nil
		}
	}
	agent := newTestAgent(t, client, Config{})
	if got := runManagedTurn(t, agent, "go"); got != "managed actions done" {
		t.Fatalf("final = %q", got)
	}
	if started.DelegateID != sent.DelegateID || started.RequestID == sent.RequestID {
		t.Fatalf("start=%+v send=%+v; want same child and independent requests", started, sent)
	}
	if !strings.Contains(statusResult, started.DelegateID) {
		t.Fatalf("status=%q, want delegate %s", statusResult, started.DelegateID)
	}
	if !strings.Contains(startWaitResult, "child answer 1") || !strings.Contains(sendWaitResult, "child answer 2") {
		t.Fatalf("start wait=%q send wait=%q", startWaitResult, sendWaitResult)
	}
	if !strings.Contains(interruptResult, started.DelegateID) {
		t.Fatalf("interrupt result = %q", interruptResult)
	}
}

// TestRestoredDelegateComposed proves a delegate started by the SWE primary remains owned
// after rig restore: the restored primary can send a follow-up to it, while an unrelated id
// is rejected without starting another loop. The fsstore restore matrix remains Task 7; this
// is the Task 3 composed-consumer proof over the same memstore used by headless SWE.
func TestRestoredDelegateComposed(t *testing.T) {
	phase := "initial"
	primaryStep := 0
	var unrelatedResult string
	var childID uuid.UUID
	client := &managedScript{}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !strings.Contains(req.System, operatorDelegation) {
			if phase == "initial" {
				return finalText("initial child final"), nil
			}
			return finalText("restored follow-up final"), nil
		}
		if phase == "initial" {
			if primaryStep == 0 {
				primaryStep++
				return toolCall("restore-start", `{"agent":"operator","message":"initial","wait":true}`), nil
			}
			return finalText("initial parent final"), nil
		}
		switch primaryStep {
		case 0:
			primaryStep++
			return toolCall("restore-send", fmt.Sprintf(`{"action":"send","delegate_id":%q,"message":"follow up","wait":true}`, childID.String())), nil
		case 1:
			if got := lastToolText(req); got != "restored follow-up final" {
				return nil, fmt.Errorf("restored follow-up result = %q", got)
			}
			primaryStep++
			return toolCall("restore-unrelated", fmt.Sprintf(`{"action":"send","delegate_id":%q,"message":"intrude","wait":true}`, uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa").String())), nil
		default:
			unrelatedResult = lastToolText(req)
			return finalText("restored parent final"), nil
		}
	}

	definitions, err := swarmDefinitions(client, testModel(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	stores, err := openStores(memstore.New())
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := buildRig(definitions, stores, t.TempDir(), Config{}, false)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := assembly.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	agent, err := newSessionAgent(context.Background(), controller, stores.session, false)
	if err != nil {
		t.Fatal(err)
	}
	_, observed := runManagedTurnObserved(t, agent, "go")
	for _, ev := range observed {
		if started, ok := ev.(event.LoopStarted); ok && !started.Cause.Coordinates.LoopID.IsZero() {
			childID = started.LoopID
		}
	}
	if childID.IsZero() {
		t.Fatal("initial composed delegation emitted no child LoopStarted")
	}
	sid := agent.SessionID()
	if err := agent.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	phase = "restored"
	primaryStep = 0
	restoredController, err := assembly.RestoreSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := newSessionAgent(context.Background(), restoredController, stores.session, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close(context.Background()) })
	if got := runManagedTurn(t, restored, "continue"); got != "restored parent final" {
		t.Fatalf("restored primary final = %q", got)
	}
	if !strings.Contains(unrelatedResult, "is not owned by this loop") {
		t.Fatalf("unrelated delegate result = %q", unrelatedResult)
	}
}

// TestManagedSubagentLimitsComposed captures the real parent-scoped controller that the rig
// binds into a managed primer. Calling that controller directly observes typed session errors
// before the model-facing Subagent tool intentionally renders them as text.
func TestManagedSubagentLimitsComposed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits rig.DelegationLimits
		want   session.SessionErrorKind
	}{
		{"depth one", rig.DelegationLimits{Depth: 1, Quota: operatorSpawnQuota}, session.SessionLoopDepthExceeded},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			agent, stores, controller := newTypedDelegateTestRig(t, tc.limits)
			before := storedLoopStartedCount(t, stores.session, agent.SessionID())
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := controller.Execute(ctx, tool.DelegateRequest{
				Operation: tool.DelegateStart,
				Agent:     string(operator.Name),
				Message:   "go",
				Wait:      true,
			})
			var sessionErr *session.SessionError
			if !errors.As(err, &sessionErr) || sessionErr.Kind != tc.want {
				t.Fatalf("start error = %T %v, want *SessionError{%s}", err, err, tc.want)
			}
			if after := storedLoopStartedCount(t, stores.session, agent.SessionID()); after != before {
				t.Fatalf("durable LoopStarted count = %d, want unchanged %d", after, before)
			}
		})
	}

	t.Run("quota one", func(t *testing.T) {
		t.Parallel()
		agent, stores, controller := newTypedDelegateTestRig(t, rig.DelegationLimits{Depth: operatorSpawnDepth, Quota: 1})
		before := storedLoopStartedCount(t, stores.session, agent.SessionID())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		request := tool.DelegateRequest{
			Operation: tool.DelegateStart,
			Agent:     string(operator.Name),
			Message:   "go",
			Wait:      true,
		}
		if _, err := controller.Execute(ctx, request); err != nil {
			t.Fatalf("first start: %v", err)
		}
		afterFirst := storedLoopStartedCount(t, stores.session, agent.SessionID())
		if afterFirst != before+1 {
			t.Fatalf("durable LoopStarted after first = %d, want %d", afterFirst, before+1)
		}
		_, err := controller.Execute(ctx, request)
		var sessionErr *session.SessionError
		if !errors.As(err, &sessionErr) || sessionErr.Kind != session.SessionLoopQuotaExceeded {
			t.Fatalf("second start error = %T %v, want *SessionError{%s}", err, err, session.SessionLoopQuotaExceeded)
		}
		if after := storedLoopStartedCount(t, stores.session, agent.SessionID()); after != afterFirst {
			t.Fatalf("refused start changed durable LoopStarted count to %d, want %d", after, afterFirst)
		}
	})
}

type typedDelegatePermission struct {
	controller tool.DelegateController
}

func (*typedDelegatePermission) Check(context.Context, tool.InvokableTool, string, string) loop.Effect {
	return loop.EffectAutoApprove
}

func (*typedDelegatePermission) Grant(context.Context, string, string, tool.ApprovalScope) error {
	return nil
}

// newTypedDelegateTestRig composes the same SWE rig path with the smallest managed topology
// needed to expose the public controller capability supplied in real primer bindings.
func newTypedDelegateTestRig(t *testing.T, limits rig.DelegationLimits) (*sessionAgent, *swarmStores, tool.DelegateController) {
	t.Helper()
	client := &managedScript{fn: func(context.Context, inference.Request) ([]content.Chunk, error) {
		return finalText("child done"), nil
	}}
	permission := &typedDelegatePermission{}
	primer, err := loop.Define(
		loop.WithName(operatorPrimaryName),
		loop.WithInference(client, testModel()),
		loop.WithPermissionFactory(func(_ context.Context, bindings tool.Bindings) (loop.PermissionGate, error) {
			permission.controller = bindings.Delegate
			return permission, nil
		}),
		loop.WithPolicyRevision("typed-delegate-test"),
		loop.WithDelegates(operator.Name),
		loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}),
	)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := loop.Define(loop.WithName(operator.Name), loop.WithInference(client, testModel()))
	if err != nil {
		t.Fatal(err)
	}
	stores, err := openStores(memstore.New())
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := buildRigForDelegationCaps([]loop.Definition{primer, leaf}, stores, t.TempDir(), Config{}, false, limits)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := assembly.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	agent, err := newSessionAgent(context.Background(), controller, stores.session, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	if permission.controller == nil {
		t.Fatal("rig supplied no scoped delegate controller to primer permission binding")
	}
	return agent, stores, permission.controller
}

func storedLoopStartedCount(t *testing.T, store *sessionstore.Store, sessionID uuid.UUID) int {
	t.Helper()
	replayer, err := store.OpenEventReplayer(sessionID, sessionstore.ReplayRequest{})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{From: journal.Beginning()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cursor.Close() }()
	count := 0
	for {
		ev, _, err := cursor.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return count
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ev.(event.LoopStarted); ok {
			count++
		}
	}
}

func countLoopStarted(events []event.Event) int {
	count := 0
	for _, ev := range events {
		if _, ok := ev.(event.LoopStarted); ok {
			count++
		}
	}
	return count
}
