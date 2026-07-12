package swe

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/looprig/cli/tui"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/swe/agents/operator"
	"github.com/looprig/swe/agents/reviewer"
)

// swarm_test.go proves the three-loop managed-delegation topology: swarmDefinitions yields
// exactly [operator-primary, operator, reviewer]; only the primer declares delegates + managed
// delegation and displays as "operator"; the primer's tool policy and prompt identity match the
// operator leaf's (minus the primer-only delegation guidance) so they cannot drift; and the
// headless New path brings the primer up as the durable root loop.

// swarmDefs builds the three definitions with the fake client + test model under cfg.
func swarmDefs(t *testing.T, cfg Config) []loop.Definition {
	t.Helper()
	defs, err := swarmDefinitions(&fakeLLM{}, testModel(), cfg)
	if err != nil {
		t.Fatalf("swarmDefinitions() error = %v", err)
	}
	if len(defs) != 3 {
		t.Fatalf("swarmDefinitions() len = %d, want 3", len(defs))
	}
	return defs
}

// TestSwarmDefinitionsTopology proves the three definitions, their order and names, and that
// ONLY the operator-primary primer declares delegates + managed delegation; both leaves are
// delegate-free with the zero (sync-only) delegation.
func TestSwarmDefinitionsTopology(t *testing.T) {
	t.Parallel()
	defs := swarmDefs(t, Config{})
	primer, operatorLeaf, reviewerLeaf := defs[0], defs[1], defs[2]

	tests := []struct {
		name          string
		def           loop.Definition
		wantName      identity.AgentName
		wantDelegates int
		wantManaged   bool
	}{
		{name: "primer", def: primer, wantName: operatorPrimaryName, wantDelegates: 2, wantManaged: true},
		{name: "operator leaf", def: operatorLeaf, wantName: operator.Name, wantDelegates: 0, wantManaged: false},
		{name: "reviewer leaf", def: reviewerLeaf, wantName: reviewer.Name, wantDelegates: 0, wantManaged: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.def.Name(); got != tt.wantName {
				t.Errorf("Name() = %q, want %q", got, tt.wantName)
			}
			if got := len(tt.def.Delegates()); got != tt.wantDelegates {
				t.Errorf("len(Delegates()) = %d, want %d", got, tt.wantDelegates)
			}
			managed := tt.def.Delegation().Style == loop.DelegationManaged
			if managed != tt.wantManaged {
				t.Errorf("Delegation managed = %v, want %v", managed, tt.wantManaged)
			}
		})
	}

	// The primer's delegates are exactly the two leaves.
	delegates := map[identity.AgentName]bool{}
	for _, d := range primer.Delegates() {
		delegates[d] = true
	}
	if !delegates[operator.Name] || !delegates[reviewer.Name] {
		t.Errorf("primer delegates = %v, want operator + reviewer", primer.Delegates())
	}
}

// TestSwarmDefinitionsAntiDrift proves the primer and operator leaf share ONE tool policy
// (byte-identical PolicyRevision) and one prompt identity: the operator leaf's effective
// system equals the primer's with the primer-only operatorDelegation guidance removed. This
// is the guard that the two operator faces cannot silently diverge.
func TestSwarmDefinitionsAntiDrift(t *testing.T) {
	t.Parallel()
	defs := swarmDefs(t, Config{})
	primer, operatorLeaf := defs[0], defs[1]

	// The primer and operator leaf are built from the SAME operator.BuildTools result, so
	// their declared tool sets are byte-identical (the managed Subagent is added
	// structurally by the rig at bind, not part of either definition's WithTools). Whole-
	// definition PolicyRevision necessarily differs (name, system, delegation), so the
	// no-drift signal is the identical declared tool-name set.
	primerTools := append([]string(nil), primer.FingerprintInitial().ToolNames...)
	leafTools := append([]string(nil), operatorLeaf.FingerprintInitial().ToolNames...)
	sort.Strings(primerTools)
	sort.Strings(leafTools)
	if !slices.Equal(primerTools, leafTools) {
		t.Errorf("primer tool set %v != operator leaf %v — tool policy drifted", primerTools, leafTools)
	}

	primerSys := primer.FingerprintInitial().EffectiveSystem
	leafSys := operatorLeaf.FingerprintInitial().EffectiveSystem

	if !strings.Contains(primerSys, operatorDelegation) {
		t.Error("primer system does not carry the operatorDelegation guidance")
	}
	if strings.Contains(leafSys, operatorDelegation) {
		t.Error("operator leaf system carries the primer-only operatorDelegation guidance, want absent")
	}
	if got := strings.Replace(primerSys, operatorDelegation, "", 1); got != leafSys {
		t.Errorf("primer-minus-delegation system != operator leaf system:\nprimer(-deleg)=%q\nleaf=%q", got, leafSys)
	}
	// Both carry the shared identity, operator role, and trusted code-style catalog.
	for _, want := range []string{`<identity product="SWE">`, `<role name="operator">`, "<available_skills>", "code-style"} {
		if !strings.Contains(leafSys, want) {
			t.Errorf("operator leaf system missing %q", want)
		}
	}
}

// TestReviewerLeafIsReadOnly proves the reviewer leaf carries no write/edit/Subagent tools:
// its tool policy differs from the operator's, and it declares no delegates.
func TestReviewerLeafIsReadOnly(t *testing.T) {
	t.Parallel()
	defs := swarmDefs(t, Config{})
	operatorLeaf, reviewerLeaf := defs[1], defs[2]
	if reviewerLeaf.PolicyRevision() == operatorLeaf.PolicyRevision() {
		t.Error("reviewer PolicyRevision == operator's, want a distinct read-only policy")
	}
	names := map[string]bool{}
	for _, n := range reviewerLeaf.FingerprintInitial().ToolNames {
		names[n] = true
	}
	for _, forbidden := range []string{"WriteFile", "EditFile", "Subagent"} {
		if names[forbidden] {
			t.Errorf("reviewer leaf carries %q, want read-only critique tools only", forbidden)
		}
	}
}

// TestOperatorDelegationIsWellFormedXML proves the primer-only operatorDelegation fragment is a
// single well-formed <delegation> element (it is baked into the primer system prompt).
func TestOperatorDelegationIsWellFormedXML(t *testing.T) {
	t.Parallel()
	var probe struct {
		XMLName xml.Name `xml:"delegation"`
	}
	if err := xml.Unmarshal([]byte(operatorDelegation), &probe); err != nil {
		t.Fatalf("operatorDelegation is not well-formed XML: %v", err)
	}
	if probe.XMLName.Local != "delegation" {
		t.Errorf("operatorDelegation root element = %q, want %q", probe.XMLName.Local, "delegation")
	}
}

// TestOperatorSpawnCaps pins the delegation safety caps the rig enforces (Depth 2 admits the
// one structural level primary→leaf; Quota 64 bounds total spawns).
func TestOperatorSpawnCaps(t *testing.T) {
	t.Parallel()
	if operatorSpawnDepth != 2 {
		t.Errorf("operatorSpawnDepth = %d, want 2 (structural depth-1: primary → non-spawning leaf)", operatorSpawnDepth)
	}
	if operatorSpawnQuota != 64 {
		t.Errorf("operatorSpawnQuota = %d, want 64", operatorSpawnQuota)
	}
}

// TestNewWithClientBuildsRootPrimer proves the headless New path (via the fake-client seam)
// builds a usable tui.Agent whose durable root loop is the operator-primary, DISPLAYED as
// "operator", started first (zero-parent). It is serial (not t.Parallel) because the exclusive
// current-checkout placement means only one headless session can hold the root lease at a time;
// it Closes the agent so the next serial session test can acquire.
func TestNewWithClientBuildsRootPrimer(t *testing.T) {
	ctx := context.Background()
	agent, err := newWithClient(ctx, &fakeLLM{}, newModelFactory(), Config{})
	if err != nil {
		t.Fatalf("newWithClient() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(ctx) })

	var _ tui.Agent = agent

	root := agent.RootLoopID()
	if root.IsZero() {
		t.Fatal("RootLoopID() is zero")
	}
	if agent.ActiveLoopID() != root {
		t.Errorf("ActiveLoopID() = %v, want RootLoopID %v (the active primer is the root)", agent.ActiveLoopID(), root)
	}

	started := swarmFirstRootLoop(t, agent.SessionID())
	if got := string(started.AgentName); got != string(operatorPrimaryName) {
		t.Errorf("root loop AgentName = %q, want %q", got, operatorPrimaryName)
	}
	if started.DisplayName != string(operator.Name) {
		t.Errorf("root loop DisplayName = %q, want %q", started.DisplayName, operator.Name)
	}
	if started.Description != operator.Description {
		t.Errorf("root loop Description = %q, want operator.Description", started.Description)
	}
	if started.LoopID != root {
		t.Errorf("root LoopStarted LoopID = %v, want RootLoopID %v", started.LoopID, root)
	}
}

// swarmFirstRootLoop drains the durable log of the headless session and returns the first
// zero-parent LoopStarted (the active primer, started first).
func swarmFirstRootLoop(t *testing.T, sessionID uuid.UUID) event.LoopStarted {
	t.Helper()
	stores, err := headlessStores()
	if err != nil {
		t.Fatalf("headlessStores() error = %v", err)
	}
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
		ev, _, err := cursor.Next(context.Background())
		if errors.Is(err, io.EOF) {
			t.Fatal("no zero-parent LoopStarted in the durable log")
		}
		if err != nil {
			t.Fatalf("cursor.Next() error = %v", err)
		}
		if started, ok := ev.(event.LoopStarted); ok && started.Cause.Coordinates.LoopID.IsZero() {
			return started
		}
	}
}
