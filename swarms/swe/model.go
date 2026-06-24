package swe

import (
	"os"
	"strings"

	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/auto"
)

// model is the named model every agent in the SWE-Swarm runs on. P1 reuses Kimi
// K2 (a strong agentic-coding model already in the catalog, text-only), matching
// the coding agent's choice; swapping models is a one-line change here. Read-only
// after init: do not reassign or mutate it — the parallel fake-client tests read
// it concurrently.
var model = llm.ChutesKimiK2()

// envAPIKey is the only value read from the environment. The value is the NAME of
// an env var, not a secret; the #nosec annotation documents that gosec's G101
// "hardcoded credentials" heuristic (which matches on the identifier name) is a
// false positive here.
const envAPIKey = "LLM_API_KEY" // #nosec G101 -- env var name, not a credential

// newModelFactory builds the swarm's ModelFactory over the DEFAULT model: a closure that
// materializes a full llm.ModelSpec for any system prompt by injecting the shared model
// identity + the (already-read) API key. It is newModelFactoryFor bound to the package
// default model, kept for the tests that exercise the default seam directly.
func newModelFactory(apiKey string) ModelFactory {
	return newModelFactoryFor(model, apiKey)
}

// newModelFactoryFor builds a ModelFactory over an explicit base model identity: a closure
// that materializes a full llm.ModelSpec for any system prompt by injecting base's
// provider/model/sampling + the (already-read) API key. The swarm owns provider/model/
// sampling; agents pass only their finished system prompt and never see the key. The key is
// closed over verbatim — never normalized; credential material is passed as-is.
func newModelFactoryFor(base llm.Model, apiKey string) ModelFactory {
	return func(systemPrompt string) llm.ModelSpec {
		return base.Spec(apiKey, systemPrompt)
	}
}

// readAPIKey is the credential boundary for the DEFAULT model. See readAPIKeyFor.
func readAPIKey() (string, error) {
	return readAPIKeyFor(model)
}

// readAPIKeyFor resolves whether base's provider requires a key (failing secure on an
// unclassified provider), reads LLM_API_KEY, and fails loud with a typed *MissingEnvError if
// a required key is absent. env is a boundary, so a whitespace-only value is treated as
// missing — the failure is loud at startup, not deferred to provider-call time. The key is
// returned verbatim (the TrimSpace is a presence check, not a sanitizer) so the single
// read+pass of credential material lives in one spot.
func readAPIKeyFor(base llm.Model) (string, error) {
	needsKey, err := base.Provider.RequiresKey()
	if err != nil {
		return "", err // unclassified provider — fail secure
	}
	apiKey := os.Getenv(envAPIKey)
	if needsKey && strings.TrimSpace(apiKey) == "" {
		return "", &MissingEnvError{Var: envAPIKey}
	}
	return apiKey, nil
}

// buildClient is the env+provider construction boundary shared by swe.New and the session
// factory. It resolves the NORMAL-loop model from catalog (the Standard tier's first entry,
// or the swarm default when no Standard is configured — a validation failure in any tier
// fails loud here), reads the API key (fail-loud via readAPIKeyFor), builds + validates the
// single shared provider client via auto.New, and returns the ModelFactory bound to the
// resolved model + key. The client is built from a spec with an EMPTY system prompt — the
// provider client is system-agnostic (the per-agent system prompt is sent every turn via
// loop.Config.Model, materialized by the factory). On any failure it returns nil client +
// nil factory (fail secure).
func buildClient(catalog ModelCatalog) (llm.LLM, ModelFactory, error) {
	resolver, err := newModelResolver(catalog)
	if err != nil {
		return nil, nil, err
	}
	base := model
	if standard, ok := resolver.standardModel(); ok {
		base = standard
	}

	apiKey, err := readAPIKeyFor(base)
	if err != nil {
		return nil, nil, err
	}
	// Empty system: the provider client carries only provider/baseURL/key; the
	// system prompt is a per-turn concern the factory bakes into each agent's spec.
	client, err := auto.New(base.Spec(apiKey, ""))
	if err != nil {
		return nil, nil, err
	}
	return client, newModelFactoryFor(base, apiKey), nil
}

// economyTitleModel resolves the Economy tier into a title-spec builder for the session-title
// coordinator, or (nil, nil) when no Economy model is configured. It validates the catalog
// and reads the API key for the economy provider; a validation or key error is returned so
// the caller can warn and fall back to fallback-only titling. The returned closure keeps the
// key closed over — the key itself is never returned to the caller.
func economyTitleModel(catalog ModelCatalog) (func(system string) llm.ModelSpec, error) {
	resolver, err := newModelResolver(catalog)
	if err != nil {
		return nil, err
	}
	economy, ok := resolver.economyModel()
	if !ok {
		return nil, nil // no Economy tier → no generation
	}
	apiKey, err := readAPIKeyFor(economy)
	if err != nil {
		return nil, err
	}
	return func(system string) llm.ModelSpec { return economy.Spec(apiKey, system) }, nil
}
