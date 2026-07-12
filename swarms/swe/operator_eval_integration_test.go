//go:build integration

package swe

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/eval"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	"github.com/looprig/llm/auto"
)

// errTurnInterrupted is the eval-harness sentinel for a turn whose context was
// cancelled (event.TurnInterrupted carries no typed cause to forward).
var errTurnInterrupted = errors.New("turn interrupted")

// operatorRunner adapts the live operator root-primer sessionAgent to eval.Runner:
// it runs one turn for the input prompt over the session subscription transport
// and projects the terminal TurnDone.Message to text (reusing the aiMessageText
// projection from text_test.go — this test is in package swe, so the unexported
// helper is in scope). Salvaged from the prior coding agent's togoRunner; only the
// agent type changed (the operator session adapter, not the coding wrapper).
type operatorRunner struct{ agent *sessionAgent }

// Run subscribes to the session fan-in, submits a single turn fire-and-forget, and
// drains the subscription to that turn's terminal, returning the terminal assistant
// text. It subscribes BEFORE submitting so no event is missed, correlates by the
// submit command id (TurnStarted.Cause.CommandID == id) to capture the turn id, then
// returns the latest TurnDone.Message for that turn; TurnFailed/TurnInterrupted map
// to an error. Enduring/all-loop scope is enough — every terminal is Enduring — and
// avoids importing the tui package for its DefaultEventFilter.
func (r operatorRunner) Run(ctx context.Context, input string) (string, error) {
	sub, err := r.agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		return "", err
	}
	defer func() { _ = sub.Close() }()

	id, err := r.agent.Submit(ctx, []content.Block{&content.TextBlock{Text: input}})
	if err != nil {
		return "", err
	}

	var turnID uuid.UUID // captured from this submit's TurnStarted; zero until then
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case d, ok := <-sub.Events():
			if !ok {
				return "", sub.Err() // hub-forced loss (or nil on intentional close)
			}
			switch e := d.Event.(type) {
			case event.TurnStarted:
				if e.Cause.CommandID == id {
					turnID = e.TurnID
				}
			case event.TurnDone:
				if !turnID.IsZero() && e.TurnID == turnID {
					return aiMessageText(e.Message), nil
				}
			case event.TurnFailed:
				if !turnID.IsZero() && e.TurnID == turnID {
					return "", e.Err
				}
			case event.TurnInterrupted:
				if !turnID.IsZero() && e.TurnID == turnID {
					return "", errTurnInterrupted
				}
			}
		}
	}
}

// modelCompleter adapts an inference.Client to eval.Completer for the Judge metric. It holds the
// provider client, the secret-free model built from the judge factory, and the judge's system
// prompt (post-split the system prompt rides inference.Request.System, not the model).
type modelCompleter struct {
	client inference.Client
	model  inference.Model
	system string
}

// Complete builds a single user-message request and projects the response to
// text. The AgenticMessages construction mirrors the production turn builder in
// internal/agent/loop/turn.go (a *content.UserMessage wrapping a content.Message
// with Role: content.RoleUser and the prompt as a TextBlock), and
// inference.Response.Message is a *content.AIMessage, so aiMessageText projects it.
func (m modelCompleter) Complete(ctx context.Context, prompt string) (string, error) {
	msgs := content.AgenticMessages{
		&content.UserMessage{Message: content.Message{
			Role:   content.RoleUser,
			Blocks: []content.Block{&content.TextBlock{Text: prompt}},
		}},
	}
	resp, err := m.client.Invoke(ctx, inference.Request{Model: m.model, System: m.system, Messages: msgs})
	if err != nil {
		return "", err
	}
	return aiMessageText(resp.Message), nil
}

// newOperatorRig constructs the production three-loop rig for the operator eval: the internal
// operator-primary root primer plus operator/reviewer leaves, with the public operator identity
// preserved by display metadata.
func newOperatorRig(ctx context.Context, client inference.Client, factory ModelFactory) (*sessionAgent, error) {
	// The eval uses the same headless rig composition as production over the process-shared
	// in-memory store, with the current checkout as the exclusive workspace.
	return newWithClient(ctx, client, factory, Config{})
}

// TestOperatorEvalIntegration runs the live operator agent — built as a session
// root primer — through the golden-set with the deterministic Contains metric and a
// model-backed Judge. It is the Phase 7A migration of the prior coding agent's eval: the eval
// engine (internal/eval) is reused unchanged; only the agent under test changed from
// the coding agent to the operator rig. It skips cleanly when LLM_API_KEY is
// unset, so the default (untagged) suite and a tagged build without a key never
// attempt a network call.
func TestOperatorEvalIntegration(t *testing.T) {
	if os.Getenv("LLM_API_KEY") == "" {
		t.Skip("LLM_API_KEY not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Build the shared client + key-bound ModelFactory the same way swe.New does
	// (buildClient), then resolve the workspace root for the operator's file tools.
	client, factory, err := buildClient(ModelCatalog{})
	if err != nil {
		t.Fatalf("buildClient: %v", err)
	}
	agent, err := newOperatorRig(ctx, client, factory)
	if err != nil {
		t.Fatalf("newOperatorRig: %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })

	cases, err := eval.LoadCases("golden-set/cases")
	if err != nil {
		t.Fatalf("LoadCases: %v", err)
	}

	run, err := eval.RunCases(ctx, operatorRunner{agent: agent}, cases)
	if err != nil {
		t.Fatalf("RunCases: %v", err)
	}

	// The judge reuses the same production model + key (package-level model var via the
	// factory), with a strict-evaluator system prompt. Post-split the secret binds to the
	// client at auto.New (2-arg), the model stays secret-free, and the system prompt rides
	// the request. readAPIKey resolves the same LLM_API_KEY buildClient used above.
	apiKey, err := readAPIKey()
	if err != nil {
		t.Fatalf("readAPIKey: %v", err)
	}
	const judgeSystem = "You are a strict, impartial evaluator."
	judgeModel := factory()
	judgeClient, err := auto.New(judgeModel, auth.APIKey(apiKey))
	if err != nil {
		t.Fatalf("auto.New: %v", err)
	}

	results, err := eval.Evaluate(ctx, run, []eval.Metric{
		eval.Contains{},
		eval.Judge{
			Criteria:  "The response directly and correctly answers the input.",
			Threshold: 0.6,
			Model:     modelCompleter{client: judgeClient, model: judgeModel, system: judgeSystem},
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	for _, r := range results {
		if r.Passed {
			continue
		}
		var b strings.Builder
		for _, s := range r.Scores {
			b.WriteString(s.Metric + "=" + s.Reason + "; ")
		}
		t.Errorf("case %q failed: %s", r.Case.Name, b.String())
	}
}
