package swe

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ciram-co/looprig/pkg/llm"
)

// ModelTier names an optional model-catalog tier in typed errors and logs.
type ModelTier string

const (
	// TierEconomy is the cheap tier used for best-effort session-title generation.
	TierEconomy ModelTier = "economy"
	// TierStandard is the tier used for normal orchestrator and subagent turns.
	TierStandard ModelTier = "standard"
	// TierPremium is stored but never implicitly selected in this change.
	TierPremium ModelTier = "premium"
)

// errEmptyModelName is the leaf cause for a catalog spec missing a model name.
var errEmptyModelName = errors.New("model name is empty")

// ModelCatalog is the OPTIONAL, in-memory model-tier catalog carried on swe.Config. Each
// tier is an ordered list whose FIRST entry is the one selected for that tier. Empty tiers
// mean "no override": an empty Standard preserves the swarm's existing default model, an
// empty Economy disables title-model generation, and Premium is catalog-only in this change
// (stored, never implicitly selected). Configured specs may carry secrets and live in memory
// only — the resolver returns secret-free model identities and never logs or serializes a
// spec.
type ModelCatalog struct {
	Economy  []llm.ModelSpec
	Standard []llm.ModelSpec
	Premium  []llm.ModelSpec
}

// ModelCatalogError reports an invalid configured spec found at construction. It carries the
// tier, the index within that tier, and a sanitized reason — never the spec's API key or
// other secret material.
type ModelCatalogError struct {
	Tier   ModelTier
	Index  int
	Reason string
	Cause  error
}

func (e *ModelCatalogError) Error() string {
	msg := fmt.Sprintf("swe: invalid %s model #%d", e.Tier, e.Index)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	return msg
}

func (e *ModelCatalogError) Unwrap() error { return e.Cause }

// modelResolver is the narrow seam the swarm depends on to choose a model per use, instead
// of threading the whole catalog through session logic. Its accessors return secret-free
// llm.Model identities (the per-use API key + system prompt are injected later by the
// ModelFactory), so a resolved or logged value never carries an API key.
type modelResolver interface {
	// standardModel returns the identity for normal orchestrator/subagent turns. ok is false
	// when no Standard tier is configured (the caller uses the swarm's default model).
	standardModel() (llm.Model, bool)
	// economyModel returns the identity for best-effort title generation, resolved lazily by
	// the title coordinator. ok is false when no Economy tier is configured.
	economyModel() (llm.Model, bool)
	// hasPremium reports whether a Premium tier is configured. Premium is catalog-only in this
	// change: there is no implicit Premium selection.
	hasPremium() bool
}

// catalogResolver is the default modelResolver over a validated catalog.
type catalogResolver struct {
	standard *llm.Model
	economy  *llm.Model
	premium  bool
}

// newModelResolver validates EVERY supplied spec in every tier (a non-empty model name, a
// classified provider, and a self-consistent sampling config) and returns a resolver that
// selects the first entry of each tier. An empty catalog yields a resolver with no
// overrides. The first invalid spec returns a typed *ModelCatalogError that never includes
// the spec's secrets.
func newModelResolver(catalog ModelCatalog) (modelResolver, error) {
	standard, err := firstValidModel(TierStandard, catalog.Standard)
	if err != nil {
		return nil, err
	}
	economy, err := firstValidModel(TierEconomy, catalog.Economy)
	if err != nil {
		return nil, err
	}
	// Premium is validated too — it is catalog-only, but a misconfigured Premium spec must
	// still fail loud at construction rather than lurk until a future tier-selection feature.
	premium, err := firstValidModel(TierPremium, catalog.Premium)
	if err != nil {
		return nil, err
	}

	return &catalogResolver{standard: standard, economy: economy, premium: premium != nil}, nil
}

func (r *catalogResolver) standardModel() (llm.Model, bool) { return derefModel(r.standard) }
func (r *catalogResolver) economyModel() (llm.Model, bool)  { return derefModel(r.economy) }
func (r *catalogResolver) hasPremium() bool                 { return r.premium }

func derefModel(m *llm.Model) (llm.Model, bool) {
	if m == nil {
		return llm.Model{}, false
	}
	return *m, true
}

// firstValidModel validates every spec in tier (so a misconfiguration anywhere fails loud)
// and returns the secret-free identity of the FIRST entry, or nil for an empty tier.
func firstValidModel(tier ModelTier, specs []llm.ModelSpec) (*llm.Model, error) {
	var first *llm.Model
	for i, spec := range specs {
		if err := validateCatalogSpec(spec); err != nil {
			return nil, &ModelCatalogError{Tier: tier, Index: i, Reason: err.Error(), Cause: err}
		}
		if first == nil {
			identity := modelFromSpec(spec)
			first = &identity
		}
	}
	return first, nil
}

// validateCatalogSpec checks a spec is self-consistent and its provider is classified
// (RequiresKey resolves). The errors it surfaces (empty-name, unknown provider, validation)
// are field-based and never contain the spec's API key.
func validateCatalogSpec(spec llm.ModelSpec) error {
	if strings.TrimSpace(spec.Model) == "" {
		return errEmptyModelName
	}
	if _, err := spec.Provider.RequiresKey(); err != nil {
		return err // unclassified provider — fail secure
	}
	return spec.Validate()
}

// modelFromSpec extracts the secret-free llm.Model identity from a spec (dropping APIKey +
// System). The per-use key + system prompt are injected later by the ModelFactory, so the
// resolved identity can never carry a secret.
func modelFromSpec(spec llm.ModelSpec) llm.Model {
	return llm.Model{
		Provider:      spec.Provider,
		BaseURL:       spec.BaseURL,
		Name:          spec.Model,
		Temperature:   spec.Temperature,
		MaxTokens:     spec.MaxTokens,
		AcceptsImages: spec.AcceptsImages,
	}
}
