package secrets_test

import (
	"errors"
	"testing"

	"github.com/trollLemon/MPHarness/internal/secrets"
	"github.com/zalando/go-keyring"
)

func TestKeyringProviderResolveUsesDefaultService(t *testing.T) {
	keyring.MockInit()
	if err := keyring.Set(secrets.DefaultService, "MPH_TEST_KEYRING_TOKEN", "from-keyring"); err != nil {
		t.Fatalf("keyring.Set: %v", err)
	}

	resolver := secrets.NewKeyringResolver("")
	got, err := resolver.Resolve("MPH_TEST_KEYRING_TOKEN")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "from-keyring" {
		t.Fatalf("Resolve returned %q, want %q", got, "from-keyring")
	}
}

func TestKeyringProviderResolveUsesConfiguredService(t *testing.T) {
	const service = "mph-test-service"
	keyring.MockInit()
	if err := keyring.Set(service, "MPH_TEST_KEYRING_TOKEN", "from-custom-service"); err != nil {
		t.Fatalf("keyring.Set: %v", err)
	}

	resolver := secrets.NewKeyringResolver(service)
	got, err := resolver.Resolve("MPH_TEST_KEYRING_TOKEN")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "from-custom-service" {
		t.Fatalf("Resolve returned %q, want %q", got, "from-custom-service")
	}
}

func TestKeyringProviderResolveReportsMissingSecret(t *testing.T) {
	keyring.MockInit()
	resolver := secrets.NewKeyringResolver("")
	_, err := resolver.Resolve("MPH_TEST_NOT_IN_KEYRING")
	if !errors.Is(err, secrets.ErrUnknownName) {
		t.Fatalf("Resolve error = %v, want %v", err, secrets.ErrUnknownName)
	}
}

func TestKeyringProviderResolveWrapsBackendErrors(t *testing.T) {
	backendErr := errors.New("mock keyring backend failure")
	keyring.MockInitWithError(backendErr)
	resolver := secrets.NewKeyringResolver("")
	_, err := resolver.Resolve("MPH_TEST_TOKEN")
	if !errors.Is(err, backendErr) {
		t.Fatalf("Resolve error = %v, want to preserve backend error %v", err, backendErr)
	}
	if errors.Is(err, secrets.ErrUnknownName) {
		t.Fatalf("Resolve error = %v, should not classify backend failure as missing secret", err)
	}
}
