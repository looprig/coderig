package swe

import (
	"strconv"

	"github.com/looprig/inference"
	"github.com/looprig/llm"
)

// The SWE-Swarm owns its model catalogue. Each entry is a secret-free
// inference.Model identity built with inference.CustomModel and validated at
// construction with the fail-closed llm.ValidateModel provider truth table (a
// known backend that speaks the entry's APIFormat over a syntactically safe
// endpoint). The connection secret is never carried on a catalogue entry — it
// binds to the Client once at auto.New (see model.go). These constructors replace
// the model rows that used to live in the now-deleted harness/pkg/llm catalogue:
// consumers own model policy, so the swarm defines its own rows here.

// InvalidCatalogModelError reports a hardcoded catalogue entry that fails the
// fail-closed provider validation. A malformed constant model row is a programming
// error, so mustValidModel panics with this typed value at package initialisation —
// the row must be fixed before the binary can run. This is the regexp.MustCompile /
// uuid.MustParse idiom: a Must-constructor for a developer-authored constant, not a
// runtime input boundary (untrusted model input is validated and returns a typed
// error, never panics — see model_catalog.go).
type InvalidCatalogModelError struct {
	Name  string
	Cause error
}

func (e *InvalidCatalogModelError) Error() string {
	return "swe: invalid catalogue model " + strconv.Quote(e.Name) + ": " + e.Cause.Error()
}

func (e *InvalidCatalogModelError) Unwrap() error { return e.Cause }

// mustValidModel runs the fail-closed llm.ValidateModel provider check on a
// hardcoded catalogue entry and returns it unchanged, panicking with an
// *InvalidCatalogModelError if the entry is malformed. It is called at the
// construction of every catalogue row (including the package-default model), so a
// bad hardcoded row fails loud at startup rather than at first provider call.
func mustValidModel(m inference.Model) inference.Model {
	if err := llm.ValidateModel(m); err != nil {
		panic(&InvalidCatalogModelError{Name: m.Name, Cause: err})
	}
	return m
}

// chutesKimiK26 is the Moonshot Kimi K2.6 model served through Chutes' TEE-attested,
// OpenAI-compatible tunnel — the swarm's default model (see model.go). It is the newest
// Kimi Chutes serves (confirmed against llm.chutes.ai /v1/models); the id carries the
// -TEE suffix every Chutes confidential chute uses. Chutes resolves the model name to a
// chute at request time, so Name is the value sent on every request; the base is the
// explicit e2e apiBase (chutes.New does not default it). Tool- and thinking-capable,
// text-only agentic-coding model (AcceptsImages stays false).
func chutesKimiK26() inference.Model {
	return mustValidModel(inference.CustomModel(
		inference.ProviderName(llm.ProviderChutes), inference.APIFormatOpenAI,
		"https://api.chutes.ai", "moonshotai/Kimi-K2.6-TEE",
		inference.WithContextLimits(inference.ContextLimits{WindowTokens: 128_000}),
		inference.WithTools(), inference.WithThinking(),
	))
}

// phalaGLM52 is the zai-org GLM 5.2 model served through Phala's aci confidential
// OpenAI-compatible gateway. The id and base are the client-facing pair Phala's
// gateway accepts (z-ai/glm-5.2 over https://inference.phala.com/v1); the gateway
// resolves it to its Chutes upstream (zai-org/GLM-5.2-TEE) transparently. Tool-capable.
func phalaGLM52() inference.Model {
	return mustValidModel(inference.CustomModel(
		inference.ProviderName(llm.ProviderPhala), inference.APIFormatOpenAI,
		"https://inference.phala.com/v1", "z-ai/glm-5.2",
		inference.WithContextLimits(inference.ContextLimits{WindowTokens: 128_000}),
		inference.WithTools(),
	))
}

// lmStudioLocal is a Model for a local LM Studio server at its default loopback
// endpoint. LM Studio speaks the OpenAI-compatible dialect and needs no credentials
// (Provider.RequiredAuth → AuthNone); the http:// loopback host is permitted by the
// structural loopback exception. Capabilities are conservative: tool-calling is
// commonly supported by local OpenAI-compatible servers; image input and hidden
// thinking are model-specific and left off. Ported from the former llm.LMStudioLocal.
func lmStudioLocal(name string) inference.Model {
	return mustValidModel(inference.CustomModel(
		inference.ProviderName(llm.ProviderLMStudio), inference.APIFormatOpenAI,
		"http://localhost:1234/v1", name,
		inference.WithTools(),
	))
}
