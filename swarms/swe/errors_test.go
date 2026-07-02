package swe

import (
	"errors"
	"testing"
)

// TestPrimaryToolSetError proves the typed error's message (with and without a Cause —
// exercising the nil-Cause guard) and that Unwrap returns the wrapped cause so errors.As
// recovers the underlying *tools.HomeUnresolvableError through it.
func TestPrimaryToolSetError(t *testing.T) {
	t.Parallel()

	cause := errors.New("home unresolvable")
	tests := []struct {
		name      string
		err       *PrimaryToolSetError
		wantMsg   string
		wantCause error
	}{
		{name: "with cause", err: &PrimaryToolSetError{Cause: cause}, wantMsg: "swe: cannot build primary operator tool set: home unresolvable", wantCause: cause},
		{name: "nil cause", err: &PrimaryToolSetError{}, wantMsg: "swe: cannot build primary operator tool set", wantCause: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.wantMsg {
				t.Errorf("Error() = %q, want %q", got, tt.wantMsg)
			}
			if got := tt.err.Unwrap(); got != tt.wantCause {
				t.Errorf("Unwrap() = %v, want %v", got, tt.wantCause)
			}
		})
	}
}
