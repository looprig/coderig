package coderig

import (
	"errors"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	"github.com/looprig/inference/contextcount"
	"github.com/looprig/llm"
)

func TestNewModelInferencePolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		model           inference.Model
		wantTransport   inference.InferenceTransport
		wantRetention   inference.RetentionPosture
		wantIdentity    bool
		wantUnsupported bool
	}{
		{name: "chutes is end-to-end encrypted", model: chutesKimiK26(), wantTransport: inference.InferenceTransportEndToEndEncrypted, wantRetention: inference.RetentionUnknown, wantIdentity: true},
		{name: "phala is end-to-end encrypted", model: phalaGLM52(), wantTransport: inference.InferenceTransportEndToEndEncrypted, wantRetention: inference.RetentionUnknown, wantIdentity: true},
		{name: "lm studio stays local", model: lmStudioLocal("local-model"), wantTransport: inference.InferenceTransportLocal, wantRetention: inference.RetentionNone},
		{name: "unknown provider fails closed", model: func() inference.Model {
			value := chutesKimiK26()
			value.Provider = inference.ProviderName("unknown")
			return value
		}(), wantUnsupported: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			policy, err := newModelInferencePolicy(tt.model)
			if tt.wantUnsupported {
				var target *UnsupportedInferenceProviderError
				if !errors.As(err, &target) {
					t.Fatalf("newModelInferencePolicy() error = %T %v, want *UnsupportedInferenceProviderError", err, err)
				}
				if target.Provider != tt.model.Provider {
					t.Errorf("Provider = %q, want %q", target.Provider, tt.model.Provider)
				}
				return
			}
			if err != nil {
				t.Fatalf("newModelInferencePolicy() error = %v", err)
			}

			capability := policy.InferenceCapability()
			if err := capability.Validate(); err != nil {
				t.Fatalf("InferenceCapability().Validate() error = %v", err)
			}
			if capability.Provider != inference.ProviderID(tt.model.Provider) {
				t.Errorf("Provider = %q, want %q", capability.Provider, tt.model.Provider)
			}
			if capability.Transport != tt.wantTransport {
				t.Errorf("Transport = %v, want %v", capability.Transport, tt.wantTransport)
			}
			if capability.Retention != tt.wantRetention {
				t.Errorf("Retention = %v, want %v", capability.Retention, tt.wantRetention)
			}
			if got := capability.SecurityIdentity != (inference.SecurityIdentity{}); got != tt.wantIdentity {
				t.Errorf("SecurityIdentity non-zero = %v, want %v", got, tt.wantIdentity)
			}

			counter := policy.ContextCounter()
			if counter == nil {
				t.Fatal("ContextCounter() = nil")
			}
			metadata := counter.CounterCapability()
			wantMetadata := inference.CounterCapability{
				Transport:    inference.CounterTransportLocal,
				Retention:    inference.RetentionNone,
				TokenizerRev: contextcount.EstimatorRevision,
				Quality:      inference.CountQualityHeuristicEstimate,
			}
			if metadata != wantMetadata {
				t.Errorf("CounterCapability() = %+v, want %+v", metadata, wantMetadata)
			}
		})
	}
}

func TestModelInferencePolicyIsFixedAcrossAllowedModelChanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		base inference.Model
	}{
		{name: "chutes", base: chutesKimiK26()},
		{name: "phala", base: phalaGLM52()},
		{name: "lm studio", base: func() inference.Model {
			value := lmStudioLocal("local-model")
			value.Limits = inference.ContextLimits{MaxInputTokens: 32_000}
			return value
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			changed := tt.base.Clone()
			changed.Name = "another-model"
			changed.Limits = inference.ContextLimits{WindowTokens: 64_000}
			changed.Caps.Tools = !changed.Caps.Tools
			effort := inference.EffortHigh
			changed.Sampling.Effort = effort

			basePolicy, err := newModelInferencePolicy(tt.base)
			if err != nil {
				t.Fatalf("newModelInferencePolicy(base) error = %v", err)
			}
			changedPolicy, err := newModelInferencePolicy(changed)
			if err != nil {
				t.Fatalf("newModelInferencePolicy(changed) error = %v", err)
			}
			if got, want := changedPolicy.InferenceCapability(), basePolicy.InferenceCapability(); got != want {
				t.Errorf("capability changed with model-local fields: got %+v, want %+v", got, want)
			}
			if got, want := changedPolicy.ContextCounter().CounterCapability(), basePolicy.ContextCounter().CounterCapability(); got != want {
				t.Errorf("counter metadata changed: got %+v, want %+v", got, want)
			}
		})
	}
}

func TestInferencePolicyTransportBinding(t *testing.T) {
	t.Parallel()

	base := chutesKimiK26()
	policy, err := newModelInferencePolicy(base)
	if err != nil {
		t.Fatalf("newModelInferencePolicy() error = %v", err)
	}
	definition, err := loop.Define(
		loop.WithName(identity.AgentName("policy-test")),
		loop.WithInference(&fakeLLM{}, base),
		loop.WithContextCounter(policy.ContextCounter()),
		loop.WithInferenceCapability(policy.InferenceCapability()),
		loop.WithContextObservation(loop.ContextObservationPolicy{
			ReservedOutput: 1,
			SafetyMargin:   1,
			CountTimeout:   time.Second,
		}),
	)
	if err != nil {
		t.Fatalf("loop.Define() error = %v", err)
	}

	allowed := base.Clone()
	allowed.Name = "another-model"
	allowed.Limits = inference.ContextLimits{WindowTokens: 64_000}
	tests := []struct {
		name      string
		candidate inference.Model
		wantField string
	}{
		{name: "model-local change is allowed", candidate: allowed},
		{name: "provider change is rejected", candidate: func() inference.Model {
			value := allowed
			value.Provider = inference.ProviderName(llm.ProviderPhala)
			return value
		}(), wantField: "Provider"},
		{name: "api format change is rejected", candidate: func() inference.Model { value := allowed; value.APIFormat = inference.APIFormatAnthropic; return value }(), wantField: "APIFormat"},
		{name: "base url change is rejected", candidate: func() inference.Model { value := allowed; value.BaseURL = "https://other.example.test"; return value }(), wantField: "BaseURL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := definition.ValidateContextModel(tt.candidate)
			if tt.wantField == "" {
				if err != nil {
					t.Fatalf("ValidateContextModel() error = %v", err)
				}
				return
			}
			var target *loop.ContextTransportBindingError
			if !errors.As(err, &target) {
				t.Fatalf("ValidateContextModel() error = %T %v, want *loop.ContextTransportBindingError", err, err)
			}
			if target.Field != tt.wantField {
				t.Errorf("Field = %q, want %q", target.Field, tt.wantField)
			}
		})
	}
}
