package swe

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/swe/agents/operator"
)

func compactionFingerprintFor(t *testing.T, root string, client *fakeLLM, policy conversationContextPolicy, registration conversationHustleRegistration) event.ConfigFingerprint {
	t.Helper()
	definitions, err := swarmDefinitionsWithContextPolicy(client, testModel(), Config{}, policy)
	if err != nil {
		t.Fatalf("swarmDefinitionsWithContextPolicy() error = %v", err)
	}
	stores := mustHeadlessTestStores(t)
	assembly, err := buildRigWithRegistration(
		definitions, stores, root, Config{}, false,
		rig.DelegationLimits{Depth: operatorSpawnDepth, Quota: operatorSpawnQuota}, registration,
	)
	if err != nil {
		t.Fatalf("buildRigWithRegistration() error = %v", err)
	}
	controller, err := assembly.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = controller.Shutdown(context.Background()) })
	return durableSessionFingerprint(t, stores, controller.SessionID())
}

func durableSessionFingerprint(t *testing.T, stores *swarmStores, sessionID uuid.UUID) event.ConfigFingerprint {
	t.Helper()
	replayer, err := stores.session.OpenEventReplayer(sessionID, sessionstore.ReplayRequest{})
	if err != nil {
		t.Fatalf("OpenEventReplayer() error = %v", err)
	}
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{From: journal.Beginning()})
	if err != nil {
		t.Fatalf("replayer.Open() error = %v", err)
	}
	defer func() { _ = cursor.Close() }()
	for {
		ev, _, nextErr := cursor.Next(context.Background())
		if errors.Is(nextErr, io.EOF) {
			t.Fatal("durable log has no SessionStarted")
		}
		if nextErr != nil {
			t.Fatalf("cursor.Next() error = %v", nextErr)
		}
		if started, ok := ev.(event.SessionStarted); ok {
			return started.Config
		}
	}
}

func compactionDefinitionForFingerprint(t *testing.T, promptRevision, parserRevision string) hustle.Definition {
	t.Helper()
	definition, err := hustle.Define(
		hustle.WithName(conversationCompactionName),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithCurrentLoopModel(),
		hustle.WithTimeout(conversationCompactionTimeout),
		hustle.WithLimits(hustle.Limits{InputBytes: conversationCompactionInputBytes, OutputBytes: conversationCompactionOutputBytes}),
		hustle.WithSystemPrompt(conversationCompactionPrompt, promptRevision),
		hustle.WithPolicyRevision(parserRevision),
	)
	if err != nil {
		t.Fatalf("hustle.Define() error = %v", err)
	}
	return definition
}

func TestCompactionCompositionFingerprintSensitivityAndSecretExclusion(t *testing.T) {
	t.Parallel()

	basePolicy, err := newConversationContextPolicy(testModel())
	if err != nil {
		t.Fatalf("newConversationContextPolicy() error = %v", err)
	}
	baseRegistration, err := newConversationHustleRegistration()
	if err != nil {
		t.Fatalf("newConversationHustleRegistration() error = %v", err)
	}
	root := t.TempDir()
	base := compactionFingerprintFor(t, root, &fakeLLM{credential: "secret-a"}, basePolicy, baseRegistration)

	tests := []struct {
		name         string
		client       *fakeLLM
		policy       conversationContextPolicy
		registration conversationHustleRegistration
		wantEqual    bool
	}{
		{name: "client credential excluded", client: &fakeLLM{credential: "secret-b"}, policy: basePolicy, registration: baseRegistration, wantEqual: true},
		{name: "compaction policy", client: &fakeLLM{}, policy: func() conversationContextPolicy { value := basePolicy; value.compaction.CompactAt--; return value }(), registration: baseRegistration},
		{name: "summary revision", client: &fakeLLM{}, policy: func() conversationContextPolicy {
			value := basePolicy
			value.summaryRevision = "swe-summary-consumption-v2"
			return value
		}(), registration: baseRegistration},
		{name: "hustle prompt revision", client: &fakeLLM{}, policy: basePolicy, registration: conversationHustleRegistration{definition: compactionDefinitionForFingerprint(t, "swe-compaction-prompt-v2", conversationCompactionParserRevision), limits: baseRegistration.limits}},
		{name: "hustle parser revision", client: &fakeLLM{}, policy: basePolicy, registration: conversationHustleRegistration{definition: compactionDefinitionForFingerprint(t, conversationCompactionPromptRevision, "harness-compaction-parser-v2"), limits: baseRegistration.limits}},
		{name: "hustle lane limits", client: &fakeLLM{}, policy: basePolicy, registration: func() conversationHustleRegistration {
			value := baseRegistration
			value.limits.AuditTimeout += time.Second
			return value
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := compactionFingerprintFor(t, root, tt.client, tt.policy, tt.registration)
			if equal := got.Equal(base); equal != tt.wantEqual {
				t.Errorf("ConfigFingerprint.Equal(base) = %v, want %v\ngot=%+v\nbase=%+v", equal, tt.wantEqual, got, base)
			}
		})
	}
}

// TestOperatorFingerprintFields asserts the rig-level config-fingerprint fields the
// composition root injects via rig.WithFingerprintFields: AgentKind is the swarm+primary
// identity ("swe:operator") and RuntimeSkills passes the human-set mode through verbatim. The
// workspace-root field is NOT set here — the rig's exclusive-workspace placement folds the
// canonical root into the fingerprint — so a restore still compares agent identity, skill
// mode, AND (via the placement) the repo root.
func TestOperatorFingerprintFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
		want rig.ConfigFingerprintFields
	}{
		{
			name: "runtime skills off",
			cfg:  Config{RuntimeSkills: false},
			want: rig.ConfigFingerprintFields{AgentKind: "swe:operator", RuntimeSkills: false},
		},
		{
			name: "runtime skills on",
			cfg:  Config{RuntimeSkills: true},
			want: rig.ConfigFingerprintFields{AgentKind: "swe:operator", RuntimeSkills: true},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := operatorFingerprintFields(tt.cfg)
			if got != tt.want {
				t.Errorf("operatorFingerprintFields = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestOperatorAgentKindFormat pins the AgentKind to "<swarm>:<primary agent>" so a rename of
// the operator's attribution name is reflected in the fingerprint (and a prior/other session,
// with a different or empty AgentKind, cannot resume as SWE).
func TestOperatorAgentKindFormat(t *testing.T) {
	t.Parallel()
	want := "swe:" + string(operator.Name)
	if operatorAgentKind != want {
		t.Errorf("operatorAgentKind = %q, want %q", operatorAgentKind, want)
	}
	if operatorAgentKind != "swe:operator" {
		t.Errorf("operatorAgentKind = %q, want %q", operatorAgentKind, "swe:operator")
	}
}
