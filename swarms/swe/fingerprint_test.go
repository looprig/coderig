package swe

import (
	"testing"

	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/swe/agents/operator"
)

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
