package swe

import (
	"os"
	"strings"

	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	"github.com/looprig/llm"
	"github.com/looprig/llm/auto"
)

// model is the named model every agent in the SWE-Swarm runs on: the swarm's local
// catalogue default, Chutes Kimi K2.7 (a strong agentic-coding model served over
// Chutes' TEE tunnel, text-only). Swapping models is a one-line change here.
// chutesKimiK27 validates the row via llm.ValidateModel at construction, so a
// malformed default fails loud at package init. Read-only after init: do not reassign
// or mutate it — the parallel fake-client tests read it concurrently.
var model = chutesKimiK27()

// envAPIKey is the only value read from the environment. The value is the NAME of
// an env var, not a secret; the #nosec annotation documents that gosec's G101
// "hardcoded credentials" heuristic (which matches on the identifier name) is a
// false positive here.
const envAPIKey = "LLM_API_KEY" // #nosec G101 -- env var name, not a credential

// newModelFactory builds the swarm's ModelFactory over the DEFAULT model: it yields the
// package default's secret-free inference.Model identity. It is newModelFactoryFor bound to the
// package default model, kept for the tests that exercise the default seam directly.
func newModelFactory() ModelFactory {
	return newModelFactoryFor(model)
}

// newModelFactoryFor builds a ModelFactory over an explicit base model identity: it yields
// base's secret-free inference.Model (provider/endpoint/model/sampling) unchanged. Post-split it
// carries NO key (the secret is bound to the Client at auto.New) and NO system prompt (each
// agent's finished prompt is set on loop.Config.System), so the factory's only job is to
// hand out the shared model identity every agent's loop is stamped with.
func newModelFactoryFor(base inference.Model) ModelFactory {
	return func() inference.Model { return base }
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
func readAPIKeyFor(base inference.Model) (string, error) {
	needsKey, err := llm.Provider(base.Provider).RequiresKey()
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
// each agent's system prompt rides loop.Config.System / inference.Request.System every turn. On any
// failure it returns nil client + nil factory (fail secure).
func buildClient(catalog ModelCatalog) (inference.Client, ModelFactory, error) {
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
