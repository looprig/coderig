package app

import (
	"crypto/sha256"
	"strconv"

	"github.com/looprig/inference/contextcount"

	model "github.com/looprig/inference/model"
	"github.com/looprig/llm"
)

// modelInferencePolicy is the narrow, secret-free context-policy surface used
// when composing a loop. The counter and capability are fixed for the lifetime
// of that loop; live model changes are constrained by loop.Definition.
type modelInferencePolicy interface {
	ContextCounter() contextcount.ContextCounter
	InferenceCapability() contextcount.InferenceCapability
}

type fixedModelInferencePolicy struct {
	counter    contextcount.ContextCounter
	capability contextcount.InferenceCapability
}

func (p fixedModelInferencePolicy) ContextCounter() contextcount.ContextCounter {
	return p.counter
}

func (p fixedModelInferencePolicy) InferenceCapability() contextcount.InferenceCapability {
	return p.capability
}

// UnsupportedInferenceProviderError reports a provider for which CodeRig has no
// reviewed inference-transport declaration. It contains only a public provider
// label and never carries endpoint credentials.
type UnsupportedInferenceProviderError struct {
	Provider model.ProviderName
}

func (e *UnsupportedInferenceProviderError) Error() string {
	return "coderig: unsupported inference policy provider " + strconv.Quote(string(e.Provider))
}

const (
	chutesInferenceIdentityRevision = "chutes-e2ee-tee-v1"
	phalaInferenceIdentityRevision  = "phala-aci-e2ee-v1"
)

// newModelInferencePolicy resolves the fixed, I/O-free counter and inference
// posture for one supported provider. The local estimator never sends request
// bytes to a separate counting endpoint. Remote retention remains Unknown
// because CodeRig has no reviewed provider retention guarantee; the in-process
// RetentionNone counter remains compatible with that conservative declaration.
func newModelInferencePolicy(model model.Model) (modelInferencePolicy, error) {
	capability, err := inferenceCapabilityForModel(model)
	if err != nil {
		return nil, err
	}
	return fixedModelInferencePolicy{
		counter:    contextcount.NewEstimator(),
		capability: capability,
	}, nil
}

func inferenceCapabilityForModel(model model.Model) (contextcount.InferenceCapability, error) {
	provider := llm.Provider(model.Provider)
	switch provider {
	case llm.ProviderChutes:
		return protectedInferenceCapability(model, chutesInferenceIdentityRevision), nil
	case llm.ProviderPhala:
		return protectedInferenceCapability(model, phalaInferenceIdentityRevision), nil
	case llm.ProviderLMStudio:
		return contextcount.InferenceCapability{
			Provider:  contextcount.ProviderID(model.Provider),
			Transport: contextcount.InferenceTransportLocal,
			Retention: contextcount.RetentionNone,
		}, nil
	default:
		return contextcount.InferenceCapability{}, &UnsupportedInferenceProviderError{Provider: model.Provider}
	}
}

func protectedInferenceCapability(model model.Model, policyRevision string) contextcount.InferenceCapability {
	return contextcount.InferenceCapability{
		Provider:         contextcount.ProviderID(model.Provider),
		Transport:        contextcount.InferenceTransportEndToEndEncrypted,
		SecurityIdentity: transportSecurityIdentity(model, policyRevision),
		Retention:        contextcount.RetentionUnknown,
	}
}

// transportSecurityIdentity binds capability metadata to the exact transport
// fields harness keeps immutable plus a reviewed provider-policy revision. It
// intentionally excludes model name, limits, sampling, and capabilities, which
// may change live without replacing the connection security boundary.
func transportSecurityIdentity(model model.Model, policyRevision string) contextcount.SecurityIdentity {
	material := string(model.Provider) + "\x00" + string(model.APIFormat) + "\x00" +
		model.BaseURL + "\x00" + policyRevision
	return sha256.Sum256([]byte(material))
}
