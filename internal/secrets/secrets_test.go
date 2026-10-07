package secrets_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/trollLemon/MPHarness/internal/config"
	"github.com/trollLemon/MPHarness/internal/secrets"
	"github.com/zalando/go-keyring"
)

func TestNames(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty input", in: ""},
		{name: "no placeholders", in: "run the healthcheck"},
		{name: "one placeholder", in: "attach %{PRO_TOKEN}", want: []string{"PRO_TOKEN"}},
		{name: "multiple placeholders", in: "use %{PRO_TOKEN} in %{REGION}", want: []string{"PRO_TOKEN", "REGION"}},
		{name: "repeated placeholder", in: "%{TOKEN} then %{TOKEN}", want: []string{"TOKEN"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := secrets.Names(tt.in); !slices.Equal(got, tt.want) {
				t.Fatalf("Names(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestNewSecretProviderDefaultsToEnvironment(t *testing.T) {
	t.Setenv("MPH_TEST_DEFAULT_SOURCE", "from-env")
	var provider secrets.Provider
	provider, err := secrets.NewSecretProvider(config.SecretsConfig{})
	if err != nil {
		t.Fatalf("NewSecretProvider: %v", err)
	}

	got, err := provider.Resolve("MPH_TEST_DEFAULT_SOURCE")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "from-env" {
		t.Fatalf("Resolve returned %q, want %q", got, "from-env")
	}
}

func TestNewSecretProviderUsesKeyringService(t *testing.T) {
	const service = "mph-test-service"
	keyring.MockInit()
	if err := keyring.Set(service, "MPH_TEST_INTERFACE_TOKEN", "from-keyring"); err != nil {
		t.Fatalf("keyring.Set: %v", err)
	}

	var provider secrets.Provider
	provider, err := secrets.NewSecretProvider(config.SecretsConfig{Source: "keyring", Service: service})
	if err != nil {
		t.Fatalf("NewSecretProvider: %v", err)
	}

	got, err := provider.Resolve("MPH_TEST_INTERFACE_TOKEN")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "from-keyring" {
		t.Fatalf("Resolve returned %q, want %q", got, "from-keyring")
	}
}

func TestNewSecretProviderRejectsUnsupportedSource(t *testing.T) {
	_, err := secrets.NewSecretProvider(config.SecretsConfig{Source: "vault"})
	if !errors.Is(err, config.ErrInvalidSecretsSource) {
		t.Fatalf("NewSecretProvider error = %v, want %v", err, config.ErrInvalidSecretsSource)
	}
}

func TestValidateWithRealEnvironmentProvider(t *testing.T) {
	t.Setenv("MPH_TEST_VALIDATE_TOKEN", "token123")
	t.Setenv("MPH_TEST_VALIDATE_REGION", "eu")
	provider, err := secrets.NewSecretProvider(config.SecretsConfig{Source: "env"})
	if err != nil {
		t.Fatalf("NewSecretProvider: %v", err)
	}

	if err := secrets.Validate("attach %{MPH_TEST_VALIDATE_TOKEN} --region %{MPH_TEST_VALIDATE_REGION}", provider); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateReportsMissingEnvironmentSecret(t *testing.T) {
	t.Setenv("MPH_TEST_VALIDATE_MISSING", "")
	provider, err := secrets.NewSecretProvider(config.SecretsConfig{Source: "env"})
	if err != nil {
		t.Fatalf("NewSecretProvider: %v", err)
	}

	err = secrets.Validate("attach %{MPH_TEST_VALIDATE_MISSING}", provider)
	if !errors.Is(err, secrets.ErrUnknownName) {
		t.Fatalf("Validate error = %v, want %v", err, secrets.ErrUnknownName)
	}
}

func TestValidateRejectsUnsafeSecretWithoutLeakingIt(t *testing.T) {
	const secret = "not safe"
	t.Setenv("MPH_TEST_VALIDATE_UNSAFE", secret)
	provider, err := secrets.NewSecretProvider(config.SecretsConfig{Source: "env"})
	if err != nil {
		t.Fatalf("NewSecretProvider: %v", err)
	}

	err = secrets.Validate("attach %{MPH_TEST_VALIDATE_UNSAFE}", provider)
	if !errors.Is(err, secrets.ErrUnsafeValue) {
		t.Fatalf("Validate error = %v, want %v", err, secrets.ErrUnsafeValue)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Validate error unexpectedly contains secret value: %q", err)
	}
}

func TestValidatePreservesKeyringErrors(t *testing.T) {
	backendErr := errors.New("mock keyring backend failure")
	keyring.MockInitWithError(backendErr)
	provider, err := secrets.NewSecretProvider(config.SecretsConfig{Source: "keyring"})
	if err != nil {
		t.Fatalf("NewSecretProvider: %v", err)
	}

	err = secrets.Validate("attach %{MPH_TEST_VALIDATE_KEYRING}", provider)
	if !errors.Is(err, backendErr) {
		t.Fatalf("Validate error = %v, want to preserve backend error %v", err, backendErr)
	}
	if errors.Is(err, secrets.ErrUnknownName) {
		t.Fatalf("Validate error = %v, should not classify backend failure as missing secret", err)
	}
}
