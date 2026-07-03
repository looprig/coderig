package swe

import (
	"errors"
	"fmt"
	"strings"

	"github.com/looprig/harness/pkg/llm"
)

// ModelTier names an optional model-catalog tier in typed errors and logs.
type ModelTier string

const (
	// TierEconomy is the cheap tier used for best-effort session-title generation.
	TierEconomy ModelTier = "economy"
	// TierStandard is the tier used for normal operator (primary) and subagent turns.
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
// (stored, never implicitly selected). Post-split each tier is a list of secret-free
// llm.Model identities: the connection secret is bound to the Client at auto.New, not carried
// on any catalog entry, so a resolved or logged model never holds an API key.
type ModelCatalog struct {
	Economy  []llm.Model
	Standard []llm.Model
	Premium  []llm.Model
}

// ModelCatalogError reports an invalid configured model found at construction. It carries the
// tier, the index within that tier, and a sanitized, field-based reason. A secret-free
// llm.Model carries no API key, so the error cannot expose one.
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
// llm.Model identities (the connection secret is bound to the Client at auto.New; each
// agent's system prompt rides loop.Config.System), so a resolved or logged value never
// carries an API key.
type modelResolver interface {
	// standardModel returns the identity for normal operator/subagent turns. ok is false
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

// newModelResolver validates EVERY supplied model in every tier (a non-empty model name, a
// classified provider, and a valid APIFormat/BaseURL per llm.Model.Validate) and returns a
// resolver that selects the first entry of each tier. An empty catalog yields a resolver with
// no overrides. The first invalid model returns a typed *ModelCatalogError naming the tier +
// index.
func newModelResolver(catalog ModelCatalog) (modelResolver, error) {
	standard, err := firstValidModel(TierStandard, catalog.Standard)
	if err != nil {
		return nil, err
	}
	economy, err := firstValidModel(TierEconomy, catalog.Economy)
	if err != nil {
		return nil, err
	}
	// Premium is validated too — it is catalog-only, but a misconfigured Premium model must
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

// firstValidModel validates every model in tier (so a misconfiguration anywhere fails loud)
// and returns a copy of the FIRST entry, or nil for an empty tier. The entries are already
// secret-free llm.Model values (the connection secret binds to the Client at auto.New).
func firstValidModel(tier ModelTier, models []llm.Model) (*llm.Model, error) {
	var first *llm.Model
	for i, m := range models {
		if err := validateCatalogModel(m); err != nil {
			return nil, &ModelCatalogError{Tier: tier, Index: i, Reason: err.Error(), Cause: err}
		}
		if first == nil {
			m := m
			first = &m
		}
	}
	return first, nil
}

// validateCatalogModel checks a configured model has a non-empty name, a classified provider
// (RequiresKey resolves — fail secure on an unknown provider), and passes llm.Model.Validate
// (provider supports the APIFormat, valid https / loopback-http BaseURL). The errors it
// surfaces are field-based; a secret-free llm.Model carries no API key to leak.
func validateCatalogModel(m llm.Model) error {
	if strings.TrimSpace(m.Name) == "" {
		return errEmptyModelName
	}
	if _, err := m.Provider.RequiresKey(); err != nil {
		return err // unclassified provider — fail secure
	}
	return m.Validate()
}
