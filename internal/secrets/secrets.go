package secrets

import (
	"errors"
	"fmt"
	"os"

	"github.com/zalando/go-keyring"

	"github.com/trollLemon/MPHarness/internal/config"
)

var (
	ErrUnknownName = errors.New("unknown secret name")
	ErrUnsafeValue = errors.New("unsafe secret value")
)

// Provider resolves a secret by its placeholder name.
type Provider interface {
	Resolve(name string) (string, error)
}

// EnvResolver implements Provider.
// An internal cache stores resolved secrets to avoid fetching the ENV var each time.
type EnvResolver struct {
	cache map[string]string
}

func NewEnvResolver() *EnvResolver {
	return &EnvResolver{cache: make(map[string]string)}
}

func (e *EnvResolver) Resolve(name string) (string, error) {

	secret, ok := e.cache[name]

	if ok {
		return secret, nil
	}

	secret = os.Getenv(name)

	if secret == "" {
		return "", ErrUnknownName
	}

	e.cache[name] = secret

	return secret, nil
}

type KeyringResolver struct {
	service string
	cache   map[string]string
}

func NewKeyringResolver(service string) *KeyringResolver {
	if service == "" {
		service = DefaultService
	}
	return &KeyringResolver{
		service: service,
		cache:   make(map[string]string),
	}
}

func (k *KeyringResolver) Resolve(name string) (string, error) {
	secret, ok := k.cache[name]

	if ok {
		return secret, nil
	}

	secret, err := keyring.Get(k.service, name)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", fmt.Errorf("%w: %s", ErrUnknownName, name)
	}
	if err != nil {
		return "", fmt.Errorf("resolve secret %q: %w", name, err)
	}

	k.cache[name] = secret

	return secret, nil

}

type Resolver interface {
	Lookup(name string) (string, error)
}

// Source identifies the secret source type.
type Source string

const (
	SourceEnv     Source = "env"
	SourceKeyring Source = "keyring"
)

// DefaultService is the default freedesktop secret service name.
const DefaultService = "mph"

func NewSecretProvider(cfg config.SecretsConfig) (Provider, error) {
	switch cfg.Source {
	case "", string(SourceEnv):
		return NewEnvResolver(), nil
	case string(SourceKeyring):
		return NewKeyringResolver(cfg.Service), nil
	default:
		return nil, config.ErrInvalidSecretsSource
	}
}

func Names(input string) []string {
	return nil
}

func Validate(input string, provider Provider) error {
	return errors.New("not implemented")
}
