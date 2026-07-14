package swe

import (
	"crypto/sha256"
	"strconv"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	"github.com/looprig/inference/contextcount"
	"github.com/looprig/llm"
)

// modelInferencePolicy is the narrow, secret-free context-policy surface used
// when composing a loop. The counter and capability are fixed for the lifetime
// of that loop; live model changes are constrained by loop.Definition.
type modelInferencePolicy interface {
	ContextCounter() inference.ContextCounter
	InferenceCapability() inference.InferenceCapability
}

type fixedModelInferencePolicy struct {
	counter    inference.ContextCounter
	capability inference.InferenceCapability
}

func (p fixedModelInferencePolicy) ContextCounter() inference.ContextCounter {
	return p.counter
}

func (p fixedModelInferencePolicy) InferenceCapability() inference.InferenceCapability {
	return p.capability
}

// UnsupportedInferenceProviderError reports a provider for which SWE has no
// reviewed inference-transport declaration. It contains only a public provider
// label and never carries endpoint credentials.
type UnsupportedInferenceProviderError struct {
	Provider inference.ProviderName
}

func (e *UnsupportedInferenceProviderError) Error() string {
	return "swe: unsupported inference policy provider " + strconv.Quote(string(e.Provider))
}

// StandardModelContextError reports that a Standard-tier model does not declare
// enough context metadata to derive a positive input limit.
type StandardModelContextError struct {
	Model inference.ModelKey
	Cause error
}

func (e *StandardModelContextError) Error() string {
	message := "swe: standard model context input limit is unavailable for " +
		string(e.Model.Provider) + "/" + e.Model.Model
	if e.Cause != nil {
		return message + ": " + e.Cause.Error()
	}
	return message
}

func (e *StandardModelContextError) Unwrap() error { return e.Cause }

const (
	chutesInferenceIdentityRevision = "chutes-e2ee-tee-v1"
	phalaInferenceIdentityRevision  = "phala-aci-e2ee-v1"
)

// newModelInferencePolicy resolves the fixed, I/O-free counter and inference
// posture for one supported provider. The local estimator never sends request
// bytes to a separate counting endpoint. Remote retention remains Unknown
// because SWE has no reviewed provider retention guarantee; the in-process
// RetentionNone counter remains compatible with that conservative declaration.
func newModelInferencePolicy(model inference.Model) (modelInferencePolicy, error) {
	capability, err := inferenceCapabilityForModel(model)
	if err != nil {
		return nil, err
	}
	return fixedModelInferencePolicy{
		counter:    contextcount.NewEstimator(),
		capability: capability,
	}, nil
}

func inferenceCapabilityForModel(model inference.Model) (inference.InferenceCapability, error) {
	provider := llm.Provider(model.Provider)
	switch provider {
	case llm.ProviderChutes:
		return protectedInferenceCapability(model, chutesInferenceIdentityRevision), nil
	case llm.ProviderPhala:
		return protectedInferenceCapability(model, phalaInferenceIdentityRevision), nil
	case llm.ProviderLMStudio:
		return inference.InferenceCapability{
			Provider:  inference.ProviderID(model.Provider),
			Transport: inference.InferenceTransportLocal,
			Retention: inference.RetentionNone,
		}, nil
	default:
		return inference.InferenceCapability{}, &UnsupportedInferenceProviderError{Provider: model.Provider}
	}
}

func protectedInferenceCapability(model inference.Model, policyRevision string) inference.InferenceCapability {
	return inference.InferenceCapability{
		Provider:         inference.ProviderID(model.Provider),
		Transport:        inference.InferenceTransportEndToEndEncrypted,
		SecurityIdentity: transportSecurityIdentity(model, policyRevision),
		Retention:        inference.RetentionUnknown,
	}
}

// transportSecurityIdentity binds capability metadata to the exact transport
// fields harness keeps immutable plus a reviewed provider-policy revision. It
// intentionally excludes model name, limits, sampling, and capabilities, which
// may change live without replacing the connection security boundary.
func transportSecurityIdentity(model inference.Model, policyRevision string) inference.SecurityIdentity {
	material := string(model.Provider) + "\x00" + string(model.APIFormat) + "\x00" +
		model.BaseURL + "\x00" + policyRevision
	return sha256.Sum256([]byte(material))
}

func validateStandardModelContext(model inference.Model) error {
	_, err := loop.ResolveContextLimits(model.Key(), model.Limits, 0, 0)
	if err != nil {
		return &StandardModelContextError{Model: model.Key(), Cause: err}
	}
	return nil
}
