package app

import (
	model "github.com/looprig/inference/model"
)

// Config contains the user-selected CodeRig application modes and the resolved,
// session-fixed access configuration. It is the app-level composition input the
// CLI fills before the Rig is constructed; the access profile cannot change for
// the lifetime of the session.
type Config struct {
	// RuntimeSkills enables the untrusted, human-gated workspace skill source.
	RuntimeSkills bool
	// AccessProfile is the selected product access profile (readonly by
	// default). It is validated at the CLI boundary before Rig construction.
	AccessProfile AccessProfile
	// AccessConfigRev is the secret-free durable digest of the effective access
	// configuration (access ABI version, selected profile, normalized operator
	// and reviewer profiles, and the non-secret egress route identity and
	// guarantees). Assembly computes it with accessConfigDigest; the composition
	// root folds it into the configuration fingerprint so a product-profile,
	// reviewer-restriction, or egress-boundary change invalidates a restore. It
	// never carries a secret.
	AccessConfigRev string
}

// ModelFactory returns the secret-free model descriptor shared by CodeRig Loops.
type ModelFactory func() model.Model
