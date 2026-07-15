//go:build integration

package swe

import (
	"context"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/sessionstore"
)

func TestCompactionRestoreUsesSummaryAndRetainsPrivilegedAudit(t *testing.T) {
	const summary = `<conversation_summary><goal>restore work</goal><constraints/><decisions/><state>first turn compacted</state><open_items>continue</open_items></conversation_summary>`
	tests := []struct {
		name string
	}{
		{name: "restored runtime starts at latest summary while journal retains superseded and internal records"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			t.Chdir(workspace)
			factory := newIntegrationFactory(t)
			originalClient := &fakeLLM{
				streamSteps: []fakeStreamStep{
					{chunks: []content.Chunk{&content.TextChunk{Text: "first reply"}}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "second reply"}}},
				},
				invokeSteps: []fakeInvokeStep{{respond: acceptanceCompactionResponse(summary, content.Usage{OutputTokens: 1})}},
			}
			agent, err := factory.openWithClient(context.Background(), originalClient, newModelFactoryFor(testModel()), SessionSelector{}, Config{})
			if err != nil {
				t.Fatalf("openWithClient(new) error = %v", err)
			}
			sessionID := agent.SessionID()
			stream, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
			if err != nil {
				t.Fatalf("Subscribe() error = %v", err)
			}
			if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "first user"}}); err != nil {
				t.Fatalf("first Submit() error = %v", err)
			}
			acceptanceEventsUntil(t, stream, func(ev event.Event) bool { _, ok := ev.(event.TurnDone); return ok })
			if _, err := agent.CompactToLoop(context.Background(), agent.ActiveLoopID()); err != nil {
				t.Fatalf("CompactToLoop() error = %v", err)
			}
			acceptanceEventsUntil(t, stream, func(ev event.Event) bool { _, ok := ev.(event.CompactionCommitted); return ok })
			if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "second user"}}); err != nil {
				t.Fatalf("second Submit() error = %v", err)
			}
			acceptanceEventsUntil(t, stream, func(ev event.Event) bool { _, ok := ev.(event.TurnDone); return ok })
			if err := stream.Close(); err != nil {
				t.Fatalf("subscription Close() error = %v", err)
			}
			if err := agent.Close(context.Background()); err != nil {
				t.Fatalf("original Close() error = %v", err)
			}

			rawReplayer, err := factory.stores.session.OpenInternalEventReplayer(sessionID, sessionstore.ReplayRequest{})
			if err != nil {
				t.Fatalf("OpenInternalEventReplayer() error = %v", err)
			}
			rawEvents := drainEventReplay(t, rawReplayer)
			assertCompactionRawAudit(t, rawEvents)

			restoredClient := &fakeLLM{streamSteps: []fakeStreamStep{{chunks: []content.Chunk{&content.TextChunk{Text: "third reply"}}}}}
			restored, err := factory.openWithClient(
				context.Background(), restoredClient, newModelFactoryFor(testModel()),
				SessionSelector{Resume: sessionID}, Config{},
			)
			if err != nil {
				t.Fatalf("openWithClient(restore) error = %v", err)
			}
			defer func() { _ = restored.Close(context.Background()) }()
			backlog, err := restored.ReplayBacklog(context.Background())
			if err != nil {
				t.Fatalf("ReplayBacklog() error = %v", err)
			}
			for _, ev := range backlog {
				switch ev.(type) {
				case event.HustleStarted, event.HustleCompleted, event.HustleFailed:
					t.Errorf("public restore backlog exposed internal audit event %T", ev)
				}
			}
			if !hasType(backlog, event.CompactionCommitted{}) {
				t.Errorf("public restore backlog missing CompactionCommitted: %v", typeNames(backlog))
			}
			firstTerminalPreserved := false
			for _, ev := range backlog {
				done, ok := ev.(event.TurnDone)
				if ok && acceptanceMessageText(t, done.Message) == "first reply" {
					firstTerminalPreserved = true
				}
			}
			if !firstTerminalPreserved {
				t.Errorf("public restore backlog lost superseded terminal response: %v", typeNames(backlog))
			}

			restoredStream, err := restored.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
			if err != nil {
				t.Fatalf("restored Subscribe() error = %v", err)
			}
			defer func() { _ = restoredStream.Close() }()
			if _, err := restored.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "third user"}}); err != nil {
				t.Fatalf("restored Submit() error = %v", err)
			}
			acceptanceEventsUntil(t, restoredStream, func(ev event.Event) bool { _, ok := ev.(event.TurnDone); return ok })
			streamRequests, invokes := restoredClient.capturedRequests()
			if len(invokes) != 0 || len(streamRequests) != 1 {
				t.Fatalf("restored Stream/Invoke requests = %d/%d, want 1/0", len(streamRequests), len(invokes))
			}
			texts := make([]string, 0, len(streamRequests[0].Messages))
			for _, message := range streamRequests[0].Messages {
				texts = append(texts, acceptanceMessageText(t, message))
			}
			joined := strings.Join(texts, "\n")
			if len(texts) < 5 || texts[0] != summary || !strings.Contains(joined, "second user") ||
				!strings.Contains(joined, "second reply") || !strings.Contains(joined, "third user") {
				t.Errorf("restored request context = %q, want summary plus post-compaction turn and new input", texts)
			}
			if strings.Contains(joined, "first user") || strings.Contains(joined, "first reply") {
				t.Errorf("restored runtime context retained superseded pre-compaction text: %q", texts)
			}
		})
	}
}

func assertCompactionRawAudit(t *testing.T, events []event.Event) {
	t.Helper()
	wants := []struct {
		name  string
		found bool
	}{
		{name: "superseded first TurnStarted"},
		{name: "superseded first StepDone"},
		{name: "internal HustleStarted"},
		{name: "internal HustleCompleted"},
		{name: "public CompactionCommitted"},
	}
	turns := 0
	steps := 0
	for _, ev := range events {
		switch ev.(type) {
		case event.TurnStarted:
			turns++
		case event.StepDone:
			steps++
		case event.HustleStarted:
			wants[2].found = true
		case event.HustleCompleted:
			wants[3].found = true
		case event.CompactionCommitted:
			wants[4].found = true
		}
	}
	wants[0].found = turns >= 2
	wants[1].found = steps >= 2
	for _, want := range wants {
		if !want.found {
			t.Errorf("privileged raw journal missing %s: %v", want.name, typeNames(events))
		}
	}
}
