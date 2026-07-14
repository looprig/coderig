package swe

import (
	"errors"
	"testing"

	"github.com/looprig/inference"
	"github.com/looprig/llm"
)

// TestCatalogModelsAreValid proves every swarm-owned catalogue entry is a well-formed,
// secret-free inference.Model that passes the fail-closed llm.ValidateModel provider preset,
// and pins the exact wire identity (provider / API format / endpoint / model id / caps) each
// row was confirmed with. If a hardcoded row drifts, this fails loud here rather than at the
// first provider call.
func TestCatalogModelsAreValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		got        inference.Model
		provider   llm.Provider
		apiFormat  inference.APIFormat
		baseURL    string
		modelID    string
		wantCaps   inference.Capabilities
		wantLimits inference.ContextLimits
	}{
		{
			name:       "chutes kimi k2.6 (default)",
			got:        chutesKimiK26(),
			provider:   llm.ProviderChutes,
			apiFormat:  inference.APIFormatOpenAI,
			baseURL:    "https://api.chutes.ai",
			modelID:    "moonshotai/Kimi-K2.6-TEE",
			wantCaps:   inference.Capabilities{Tools: true, Thinking: true},
			wantLimits: inference.ContextLimits{WindowTokens: 128_000},
		},
		{
			name:       "phala glm 5.2",
			got:        phalaGLM52(),
			provider:   llm.ProviderPhala,
			apiFormat:  inference.APIFormatOpenAI,
			baseURL:    "https://inference.phala.com/v1",
			modelID:    "z-ai/glm-5.2",
			wantCaps:   inference.Capabilities{Tools: true},
			wantLimits: inference.ContextLimits{WindowTokens: 128_000},
		},
		{
			name:      "lmstudio local",
			got:       lmStudioLocal("some-local-model"),
			provider:  llm.ProviderLMStudio,
			apiFormat: inference.APIFormatOpenAI,
			baseURL:   "http://localhost:1234/v1",
			modelID:   "some-local-model",
			wantCaps:  inference.Capabilities{Tools: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := llm.ValidateModel(tt.got); err != nil {
				t.Fatalf("llm.ValidateModel(%s) = %v, want nil", tt.name, err)
			}
			if inference.ProviderName(tt.provider) != tt.got.Provider {
				t.Errorf("Provider = %q, want %q", tt.got.Provider, tt.provider)
			}
			if tt.got.APIFormat != tt.apiFormat {
				t.Errorf("APIFormat = %q, want %q", tt.got.APIFormat, tt.apiFormat)
			}
			if tt.got.BaseURL != tt.baseURL {
				t.Errorf("BaseURL = %q, want %q", tt.got.BaseURL, tt.baseURL)
			}
			if tt.got.Name != tt.modelID {
				t.Errorf("Name = %q, want %q", tt.got.Name, tt.modelID)
			}
			if tt.got.Caps != tt.wantCaps {
				t.Errorf("Caps = %+v, want %+v", tt.got.Caps, tt.wantCaps)
			}
			if tt.got.Limits != tt.wantLimits {
				t.Errorf("Limits = %+v, want %+v", tt.got.Limits, tt.wantLimits)
			}
		})
	}
}

// TestDefaultModelIsKimiK26 proves the package default the whole swarm runs on is Chutes
// Kimi K2.6 — the newest Kimi Chutes serves.
func TestDefaultModelIsKimiK26(t *testing.T) {
	t.Parallel()

	if model.Name != "moonshotai/Kimi-K2.6-TEE" {
		t.Errorf("default model = %q, want the Chutes Kimi K2.6 id %q", model.Name, "moonshotai/Kimi-K2.6-TEE")
	}
	if model.Provider != inference.ProviderName(llm.ProviderChutes) {
		t.Errorf("default model provider = %q, want %q", model.Provider, llm.ProviderChutes)
	}
}

// TestMustValidModelRejectsMalformed proves mustValidModel fails loud (panics with a typed
// *InvalidCatalogModelError) on a hardcoded row that cannot pass the provider preset — here an
// unclassified provider. This is the fail-closed guard behind every catalogue constructor.
func TestMustValidModelRejectsMalformed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   inference.Model
	}{
		{
			name: "unknown provider",
			in: inference.CustomModel(
				inference.ProviderName("bogus"), inference.APIFormatOpenAI,
				"https://example.com", "some-model",
			),
		},
		{
			name: "provider/format mismatch",
			in: inference.CustomModel(
				inference.ProviderName(llm.ProviderGoogle), inference.APIFormatOpenAI,
				"https://example.com", "some-model",
			),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("mustValidModel did not panic on a malformed row")
				}
				err, ok := r.(error)
				if !ok {
					t.Fatalf("panic value = %T, want error", r)
				}
				var invalid *InvalidCatalogModelError
				if !errors.As(err, &invalid) {
					t.Fatalf("panic error = %v, want *InvalidCatalogModelError", err)
				}
			}()
			_ = mustValidModel(tt.in)
		})
	}
}
