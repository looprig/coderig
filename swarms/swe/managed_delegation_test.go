package swe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/inference"
	"github.com/looprig/storage/memstore"
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
	s.mu.Lock()
	chunks, err := s.fn(ctx, req)
	s.mu.Unlock()
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

func parseQueued(t *testing.T, text string) queuedHandle {
	t.Helper()
	var got queuedHandle
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("queued Subagent result %q: %v", text, err)
	}
	if got.DelegateID == "" || got.RequestID == "" {
		t.Fatalf("queued Subagent result missing ids: %q", text)
	}
	return got
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
	var statusResult, waitResult, interruptResult string
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
			started = parseQueued(t, prior)
			step++
			return toolCall("async-status", fmt.Sprintf(`{"action":"status","delegate_id":%q}`, started.DelegateID)), nil
		case 2:
			statusResult = prior
			step++
			return toolCall("async-send", fmt.Sprintf(`{"action":"send","delegate_id":%q,"message":"second","wait":false}`, started.DelegateID)), nil
		case 3:
			sent = parseQueued(t, prior)
			step++
			return toolCall("async-wait", fmt.Sprintf(`{"action":"wait","delegate_id":%q,"request_id":%q}`, sent.DelegateID, sent.RequestID)), nil
		case 4:
			waitResult = prior
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
	if !strings.Contains(statusResult, started.DelegateID) || !strings.Contains(waitResult, "child answer 2") {
		t.Fatalf("status=%q wait=%q", statusResult, waitResult)
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
				t.Fatalf("restored follow-up result = %q", got)
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

// TestManagedSubagentLimitsComposed uses the production SWE definitions with only the rig
// limits varied. Depth 2 admits primary->leaf; Depth 1 refuses before LoopStarted. Quota is
// likewise a typed session refusal and leaves no durable child phantom.
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
			client := &managedScript{}
			calls := 0
			var toolResult string
			client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
				calls++
				if calls == 1 {
					return toolCall("limited-start", `{"agent":"operator","message":"go"}`), nil
				}
				toolResult = lastToolText(req)
				return finalText("done"), nil
			}
			agent := newTestAgentWithLimits(t, client, tc.limits)
			_, observed := runManagedTurnObserved(t, agent, "go")
			if !strings.Contains(toolResult, (&session.SessionError{Kind: tc.want}).Error()) {
				t.Fatalf("tool result = %q, want typed %s refusal text", toolResult, tc.want)
			}
			if got := countLoopStarted(observed); got != 0 {
				t.Fatalf("child LoopStarted count = %d, want 0", got)
			}
		})
	}

	t.Run("quota one", func(t *testing.T) {
		t.Parallel()
		primaryStep := 0
		var refusal string
		client := &managedScript{}
		client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
			if !strings.Contains(req.System, operatorDelegation) {
				return finalText("child done"), nil
			}
			switch primaryStep {
			case 0:
				primaryStep++
				return toolCall("quota-first", `{"agent":"operator","message":"first"}`), nil
			case 1:
				primaryStep++
				return toolCall("quota-second", `{"agent":"reviewer","message":"second"}`), nil
			default:
				refusal = lastToolText(req)
				return finalText("done"), nil
			}
		}
		agent := newTestAgentWithLimits(t, client, rig.DelegationLimits{Depth: operatorSpawnDepth, Quota: 1})
		_, observed := runManagedTurnObserved(t, agent, "go")
		want := (&session.SessionError{Kind: session.SessionLoopQuotaExceeded}).Error()
		if !strings.Contains(refusal, want) {
			t.Fatalf("tool result = %q, want typed quota refusal %q", refusal, want)
		}
		if got := countLoopStarted(observed); got != 1 {
			t.Fatalf("child LoopStarted count = %d, want exactly the admitted first child", got)
		}
	})
}

func newTestAgentWithLimits(t *testing.T, client inference.Client, limits rig.DelegationLimits) *sessionAgent {
	t.Helper()
	definitions, err := swarmDefinitions(client, testModel(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	stores, err := openStores(memstore.New())
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := buildRigWithLimits(definitions, stores, t.TempDir(), Config{}, false, limits)
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
	return agent
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
