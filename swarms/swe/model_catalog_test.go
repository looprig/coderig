package swe

import (
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/llm"
)

// catalogModel builds a valid, secret-free Chutes llm.Model named name, for catalog tests.
// The connection secret is bound to the Client at auto.New, never on a catalog entry.
func catalogModel(name string) llm.Model {
	m := llm.ChutesKimiK2()
	m.Name = name
	return m
}

// TestModelCatalogEmptyPreservesDefault proves an empty catalog yields a resolver with no
// overrides: standard/economy report absent and there is no premium.
func TestModelCatalogEmptyPreservesDefault(t *testing.T) {
	t.Parallel()

	r, err := newModelResolver(ModelCatalog{})
	if err != nil {
		t.Fatalf("newModelResolver(empty): %v", err)
	}
	if _, ok := r.standardModel(); ok {
		t.Error("empty catalog reported a standard model; want none (caller uses the default)")
	}
	if _, ok := r.economyModel(); ok {
		t.Error("empty catalog reported an economy model; want none")
	}
	if r.hasPremium() {
		t.Error("empty catalog reported a premium tier; want none")
	}
}

// TestModelCatalogStandardChoosesFirst proves Standard selects its FIRST entry's identity,
// and that the resolved value is a secret-free llm.Model (no API key field exists on it).
func TestModelCatalogStandardChoosesFirst(t *testing.T) {
	t.Parallel()

	r, err := newModelResolver(ModelCatalog{
		Standard: []llm.Model{catalogModel("standard-first"), catalogModel("standard-second")},
	})
	if err != nil {
		t.Fatalf("newModelResolver: %v", err)
	}
	got, ok := r.standardModel()
	if !ok {
		t.Fatal("standard model not reported despite a configured Standard tier")
	}
	if got.Name != "standard-first" {
		t.Errorf("standard model = %q, want the first entry %q", got.Name, "standard-first")
	}
	// Economy is independent and stays absent.
	if _, ok := r.economyModel(); ok {
		t.Error("economy reported present from a Standard-only catalog")
	}
}

// TestModelCatalogEconomyResolvesLazily proves Economy is reported independently (the title
// coordinator resolves it only when title generation starts) and does not select Standard.
func TestModelCatalogEconomyResolvesLazily(t *testing.T) {
	t.Parallel()

	r, err := newModelResolver(ModelCatalog{
		Economy: []llm.Model{catalogModel("economy-first")},
	})
	if err != nil {
		t.Fatalf("newModelResolver: %v", err)
	}
	got, ok := r.economyModel()
	if !ok {
		t.Fatal("economy model not reported despite a configured Economy tier")
	}
	if got.Name != "economy-first" {
		t.Errorf("economy model = %q, want %q", got.Name, "economy-first")
	}
	if _, ok := r.standardModel(); ok {
		t.Error("standard reported present from an Economy-only catalog")
	}
}

// TestModelCatalogPremiumHasNoImplicitSelection proves Premium is stored but never selected
// as standard or economy.
func TestModelCatalogPremiumHasNoImplicitSelection(t *testing.T) {
	t.Parallel()

	r, err := newModelResolver(ModelCatalog{
		Premium: []llm.Model{catalogModel("premium-first")},
	})
	if err != nil {
		t.Fatalf("newModelResolver: %v", err)
	}
	if !r.hasPremium() {
		t.Error("premium tier not reported despite being configured")
	}
	if _, ok := r.standardModel(); ok {
		t.Error("premium implicitly selected as standard")
	}
	if _, ok := r.economyModel(); ok {
		t.Error("premium implicitly selected as economy")
	}
}

// TestModelCatalogInvalidModelIsTyped proves an invalid or unclassified catalog model fails
// loud at construction with a typed *ModelCatalogError naming the tier and index.
func TestModelCatalogInvalidModelIsTyped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		catalog   ModelCatalog
		wantTier  ModelTier
		wantIndex int
	}{
		{
			name:      "empty model name in standard",
			catalog:   ModelCatalog{Standard: []llm.Model{catalogModel("ok"), catalogModel("")}},
			wantTier:  TierStandard,
			wantIndex: 1,
		},
		{
			name: "unclassified provider in economy",
			catalog: ModelCatalog{Economy: []llm.Model{func() llm.Model {
				m := catalogModel("eco")
				m.Provider = llm.Provider("bogus")
				return m
			}()}},
			wantTier:  TierEconomy,
			wantIndex: 0,
		},
		{
			// A classified provider + non-empty name that still fails llm.Model.Validate for a
			// NEW reason (a non-loopback http:// BaseURL is rejected — plaintext remote endpoint),
			// proving validateCatalogModel wraps Validate failures, not just empty-name/unknown-provider.
			name: "validate failure (non-loopback http BaseURL) in premium",
			catalog: ModelCatalog{Premium: []llm.Model{func() llm.Model {
				m := catalogModel("insecure")
				m.BaseURL = "http://api.example.com"
				return m
			}()}},
			wantTier:  TierPremium,
			wantIndex: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := newModelResolver(tt.catalog)
			var catErr *ModelCatalogError
			if !errors.As(err, &catErr) {
				t.Fatalf("newModelResolver() error = %T %v, want *ModelCatalogError", err, err)
			}
			if catErr.Tier != tt.wantTier {
				t.Errorf("Tier = %q, want %q", catErr.Tier, tt.wantTier)
			}
			if catErr.Index != tt.wantIndex {
				t.Errorf("Index = %d, want %d", catErr.Index, tt.wantIndex)
			}
		})
	}
}
