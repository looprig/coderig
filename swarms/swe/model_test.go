package swe

import (
	"errors"
	"testing"

	"github.com/looprig/llm"
)

// TestModelFactoryYieldsSharedModel proves the ModelFactory yields the swarm's shared,
// secret-free inference.Model identity (the package default) verbatim: same provider, API format,
// endpoint, and model name — and, being secret-free, no API key field to carry. Post-split
// the factory takes no system prompt (each bound loop definition carries its prompt) and no
// key (the secret binds to the Client at auto.New).
func TestModelFactoryYieldsSharedModel(t *testing.T) {
	t.Parallel()

	got := newModelFactory()()

	if got.Provider != model.Provider {
		t.Errorf("factory().Provider = %q, want %q", got.Provider, model.Provider)
	}
	if got.APIFormat != model.APIFormat {
		t.Errorf("factory().APIFormat = %q, want %q", got.APIFormat, model.APIFormat)
	}
	if got.BaseURL != model.BaseURL {
		t.Errorf("factory().BaseURL = %q, want %q", got.BaseURL, model.BaseURL)
	}
	if got.Name != model.Name {
		t.Errorf("factory().Name = %q, want %q", got.Name, model.Name)
	}
}

// TestReadAPIKeyMissing proves readAPIKey fails loud with a typed *MissingEnvError
// when the model's provider requires a key and none is set, and that an explicit
// key (set via t.Setenv) is read verbatim. The whitespace-only case is treated as
// missing — a boundary check, so the failure is loud at startup, not deferred.
func TestReadAPIKey(t *testing.T) {
	// Not parallel: mutates the process environment via t.Setenv.
	tests := []struct {
		name    string
		set     bool
		value   string
		want    string
		wantErr bool
	}{
		{name: "key present", set: true, value: "secret-key", want: "secret-key"},
		{name: "key unset", set: false, wantErr: true},
		{name: "key empty", set: true, value: "", wantErr: true},
		{name: "key whitespace only", set: true, value: "   ", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv(envAPIKey, tt.value)
			} else {
				t.Setenv(envAPIKey, "")
			}

			got, err := readAPIKey()
			if (err != nil) != tt.wantErr {
				t.Fatalf("readAPIKey() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var me *MissingEnvError
				if !errors.As(err, &me) {
					t.Fatalf("readAPIKey() error = %v, want *MissingEnvError", err)
				}
				if me.Var != envAPIKey {
					t.Errorf("MissingEnvError.Var = %q, want %q", me.Var, envAPIKey)
				}
				return
			}
			if got != tt.want {
				t.Errorf("readAPIKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBuildClientFailsLoudOnMissingKey proves buildClient (the env+provider
// boundary) refuses to build a client when the required key is absent, returning
// the typed *MissingEnvError and a nil client + nil factory (fail secure).
func TestBuildClientFailsLoudOnMissingKey(t *testing.T) {
	t.Setenv(envAPIKey, "")

	client, factory, err := buildClient(ModelCatalog{})
	if client != nil {
		t.Errorf("buildClient(ModelCatalog{}) client = %v, want nil", client)
	}
	if factory != nil {
		t.Errorf("buildClient(ModelCatalog{}) factory = %v, want nil", factory)
	}
	var me *MissingEnvError
	if !errors.As(err, &me) {
		t.Fatalf("buildClient(ModelCatalog{}) error = %v, want *MissingEnvError", err)
	}
}

// TestBuildClientHappy proves buildClient builds a non-nil client + factory when
// the key is present, and that the factory it returns yields the shared model identity.
func TestBuildClientHappy(t *testing.T) {
	t.Setenv(envAPIKey, "secret-key")

	client, factory, err := buildClient(ModelCatalog{})
	if err != nil {
		t.Fatalf("buildClient(ModelCatalog{}) error = %v", err)
	}
	if client == nil {
		t.Fatal("buildClient(ModelCatalog{}) returned nil client")
	}
	if factory == nil {
		t.Fatal("buildClient(ModelCatalog{}) returned nil factory")
	}
	if got := factory(); got.Name != model.Name {
		t.Errorf("factory().Name = %q, want the shared model %q", got.Name, model.Name)
	}
}

// ensure llm is referenced even if a future refactor drops a direct use.
var _ = llm.ProviderChutes
