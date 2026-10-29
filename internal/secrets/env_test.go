package secrets_test

import (
	"errors"
	"testing"

	"github.com/trollLemon/MPHarness/v2/internal/secrets"
)

func TestEnvironmentProviderLookup(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		set     bool
		want    string
		wantErr error
	}{
		{name: "present variable", key: "MPH_TEST_TOKEN", value: "token123", set: true, want: "token123"},
		{name: "unset variable", key: "MPH_TEST_MISSING", wantErr: secrets.ErrUnknownName},
		{name: "empty variable", key: "MPH_TEST_EMPTY", value: "", set: true, wantErr: secrets.ErrUnknownName},
		{name: "whitespace value is present", key: "MPH_TEST_WHITESPACE", value: "  ", set: true, want: "  "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv(tt.key, tt.value)
			}
			resolver := secrets.NewEnvResolver()
			got, err := resolver.Lookup(tt.key)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Lookup error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Lookup(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}
