package swe

import (
	"os"
	"strings"

	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/auth"
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

// newModelFactory builds the swarm's ModelFactory over the DEFAULT model: it yields the
// package default's secret-free llm.Model identity. It is newModelFactoryFor bound to the
// package default model, kept for the tests that exercise the default seam directly.
func newModelFactory() ModelFactory {
	return newModelFactoryFor(model)
}

// newModelFactoryFor builds a ModelFactory over an explicit base model identity: it yields
// base's secret-free llm.Model (provider/endpoint/model/sampling) unchanged. Post-split it
// carries NO key (the secret is bound to the Client at auto.New) and NO system prompt (each
// agent's finished prompt is set on loop.Config.System), so the factory's only job is to
// hand out the shared model identity every agent's loop is stamped with.
func newModelFactoryFor(base llm.Model) ModelFactory {
	return func() llm.Model { return base }
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
// resolved model. The connection secret (the API key) is bound to the CLIENT here, once, at
// auto.New — never onto the model or the factory. The provider client is system-agnostic:
// each agent's system prompt rides loop.Config.System / llm.Request.System every turn. On any
// failure it returns nil client + nil factory (fail secure).
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
	// The secret binds to the connection here, once: auto.New couples base's provider/
	// endpoint with the key. The returned model + factory stay secret-free.
	client, err := auto.New(base, auth.APIKey(apiKey))
	if err != nil {
		return nil, nil, err
	}
	return client, newModelFactoryFor(base), nil
}

// economyTitleModel resolves the Economy tier into the secret-free title MODEL for the
// session-title coordinator, or (nil, nil) when no Economy model is configured. It validates
// the catalog and confirms the economy provider's key requirement is satisfiable (via the
// shared LLM_API_KEY); a validation or key error is returned so the caller can warn and fall
// back to fallback-only titling. The returned *llm.Model carries no secret: the coordinator
// sends it on the SHARED provider client (bound to the standard provider/endpoint/key) via
// llm.Request.Model — the client's connection-binding guard fail-secures a mismatched-provider
// economy model to fallback-only titling rather than misrouting it.
func economyTitleModel(catalog ModelCatalog) (*llm.Model, error) {
	resolver, err := newModelResolver(catalog)
	if err != nil {
		return nil, err
	}
	economy, ok := resolver.economyModel()
	if !ok {
		return nil, nil // no Economy tier → no generation
	}
	if _, err := readAPIKeyFor(economy); err != nil {
		return nil, err
	}
	return &economy, nil
}
