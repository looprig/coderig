package app

import (
	"github.com/looprig/harness/pkg/identity"
	model "github.com/looprig/inference/model"
	"github.com/looprig/sandbox"
)

// DefaultSecurityMode permits workspace writes while confinement keeps effects bounded.
const DefaultSecurityMode = uint8(sandbox.Write)

var securityModeNames = map[string]uint8{
	"zerotrust": uint8(sandbox.ZeroTrust),
	"readonly":  uint8(sandbox.ReadOnly),
	"write":     uint8(sandbox.Write),
	"trusted":   uint8(sandbox.Trusted),
}

// ParseSecurityMode converts a CLI security name to its security-limit ordinal.
func ParseSecurityMode(name string) (uint8, bool) {
	mode, ok := securityModeNames[name]
	return mode, ok
}

// Config contains the user-selected CodeRig application modes.
type Config struct {
	RuntimeSkills bool
	Greeting      bool
	SecurityLimit uint8
}

// ModelFactory returns the secret-free model descriptor shared by CodeRig Loops.
type ModelFactory func() model.Model

// LoopDisplay is the ordered public metadata used by the greeting.
type LoopDisplay struct {
	Name        identity.AgentName
	Description string
}
