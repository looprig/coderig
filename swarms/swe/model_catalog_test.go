package swe

import (
	"errors"
	"strings"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
)

// catalogSpec builds a valid Chutes ModelSpec named name carrying apiKey, for catalog tests.
func catalogSpec(name, apiKey string) llm.ModelSpec {
	spec := llm.ChutesKimiK2().Spec(apiKey, "")
	spec.Model = name
	return spec
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
		Standard: []llm.ModelSpec{catalogSpec("standard-first", "k1"), catalogSpec("standard-second", "k2")},
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
		Economy: []llm.ModelSpec{catalogSpec("economy-first", "k")},
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
		Premium: []llm.ModelSpec{catalogSpec("premium-first", "k")},
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

// TestModelCatalogInvalidSpecIsTyped proves an invalid or unclassified spec fails loud at
// construction with a typed *ModelCatalogError naming the tier and index.
func TestModelCatalogInvalidSpecIsTyped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		catalog   ModelCatalog
		wantTier  ModelTier
		wantIndex int
	}{
		{
			name:      "empty model name in standard",
			catalog:   ModelCatalog{Standard: []llm.ModelSpec{catalogSpec("ok", "k"), catalogSpec("", "k")}},
			wantTier:  TierStandard,
			wantIndex: 1,
		},
		{
			name: "unclassified provider in economy",
			catalog: ModelCatalog{Economy: []llm.ModelSpec{func() llm.ModelSpec {
				s := catalogSpec("eco", "k")
				s.Provider = llm.Provider("bogus")
				return s
			}()}},
			wantTier:  TierEconomy,
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

// TestModelCatalogErrorHidesAPIKey proves a construction error from an invalid spec never
// leaks the spec's API key into its message.
func TestModelCatalogErrorHidesAPIKey(t *testing.T) {
	t.Parallel()

	const secret = "SUPER-SECRET-KEY-1234"
	_, err := newModelResolver(ModelCatalog{
		Standard: []llm.ModelSpec{catalogSpec("", secret)}, // empty name → invalid
	})
	if err == nil {
		t.Fatal("expected an error for an invalid spec")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error message leaked the API key: %q", err.Error())
	}
}
