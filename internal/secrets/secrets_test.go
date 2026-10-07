package secrets_test

import (
	"errors"
	"testing"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/secrets"
	"github.com/zalando/go-keyring"
)

func TestNew(t *testing.T) {
	cases := []struct {
		name    string
		cfg     config.SecretsConfig
		wantErr bool
	}{
		{name: "defaults to environment", cfg: config.SecretsConfig{}, wantErr: false},
		{name: "uses keyring source", cfg: config.SecretsConfig{Source: "keyring", Service: "mph-test-service"}, wantErr: false},
		{name: "rejects unsupported source", cfg: config.SecretsConfig{Source: "vault"}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := secrets.New(tc.cfg)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("New: %v", err)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		setupEnv func(t *testing.T)
		wantErr  bool
		errType  error
	}{
		{
			name:  "with real environment provider",
			input: "attach %{MPH_TEST_VALIDATE_TOKEN} --region %{MPH_TEST_VALIDATE_REGION}",
			setupEnv: func(t *testing.T) {
				t.Setenv("MPH_TEST_VALIDATE_TOKEN", "token123")
				t.Setenv("MPH_TEST_VALIDATE_REGION", "eu")
			},
			wantErr: false,
		},
		{
			name:  "reports missing environment secret",
			input: "attach %{MPH_TEST_VALIDATE_MISSING}",
			setupEnv: func(t *testing.T) {
				t.Setenv("MPH_TEST_VALIDATE_MISSING", "")
			},
			wantErr: true,
			errType: secrets.ErrUnknownName,
		},
		{
			name:  "rejects unsafe secret without leaking it",
			input: "attach %{MPH_TEST_VALIDATE_UNSAFE}",
			setupEnv: func(t *testing.T) {
				t.Setenv("MPH_TEST_VALIDATE_UNSAFE", "not safe")
			},
			wantErr: true,
			errType: secrets.ErrUnsafeValue,
		},
		{
			name:  "preserves keyring errors",
			input: "attach %{MPH_TEST_VALIDATE_KEYRING}",
			setupEnv: func(t *testing.T) {
				backendErr := errors.New("mock keyring backend failure")
				keyring.MockInitWithError(backendErr)
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setupEnv(t)
			provider, err := secrets.New(config.SecretsConfig{Source: "env"})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			err = secrets.Validate(tt.input, provider)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errType != nil && !errors.Is(err, tt.errType) {
					t.Fatalf("Validate error = %v, want %v", err, tt.errType)
				}
			} else {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
			}
		})
	}
}
