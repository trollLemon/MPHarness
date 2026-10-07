package secrets

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/zalando/go-keyring"

	"github.com/trollLemon/MPHarness/internal/config"
)

var (
	ErrUnknownName = errors.New("unknown secret name")
	ErrUnsafeValue = errors.New("unsafe secret value")
)

var markerRegex = regexp.MustCompile(`%\{([^}]+)\}`)

// Resolver maps a secret name to its value.
type Resolver interface {
	Lookup(name string) (string, error)
}

// EnvResolver looks secrets up in the host process environment. An internal
// cache stores resolved secrets to avoid fetching the ENV var each time.
type EnvResolver struct {
	cache map[string]string
}

func NewEnvResolver() *EnvResolver {
	return &EnvResolver{cache: make(map[string]string)}
}

func (e *EnvResolver) Resolve(name string) (string, error) {
	if secret, ok := e.cache[name]; ok {
		return secret, nil
	}
	secret := os.Getenv(name)
	if secret == "" {
		return "", ErrUnknownName
	}
	e.cache[name] = secret
	return secret, nil
}

func (e *EnvResolver) Lookup(name string) (string, error) {
	return e.Resolve(name)
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
	if secret, ok := k.cache[name]; ok {
		return secret, nil
	}
	secret, err := keyring.Get(k.service, name)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", fmt.Errorf("%w: %s", ErrUnknownName, name)
	}
	if err != nil {
		return "", fmt.Errorf("resolve secret %q: %w", name, err)
	}
	if secret == "" {
		return "", fmt.Errorf("%w: %s", ErrUnknownName, name)
	}
	k.cache[name] = secret
	return secret, nil
}

func (k *KeyringResolver) Lookup(name string) (string, error) {
	return k.Resolve(name)
}

// Source identifies the secret source type.
type Source string

const (
	SourceEnv     Source = "env"
	SourceKeyring Source = "keyring"
)

// DefaultService is the default freedesktop secret service name.
const DefaultService = "mph"

// New returns the Resolver that cfg selects. Parse has already rejected an
// unknown source, so the error arm is defensive.
func New(cfg config.SecretsConfig) (Resolver, error) {
	switch cfg.Source {
	case "", string(SourceEnv):
		return NewEnvResolver(), nil
	case string(SourceKeyring):
		return NewKeyringResolver(cfg.Service), nil
	default:
		return nil, config.ErrInvalidSecretsSource
	}
}

type marker struct {
	name       string
	start, end int
}

func isWordByte(c byte) bool {
	return c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	head := s[0]
	if head != '_' && ('a' > head || head > 'z') && ('A' > head || head > 'Z') {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isWordByte(s[i]) {
			return false
		}
	}
	return true
}

func scanMarkers(s string) []marker {
	var out []marker
	for _, loc := range markerRegex.FindAllStringSubmatchIndex(s, -1) {
		start, end := loc[0], loc[1]
		name := s[loc[2]:loc[3]]
		if !validName(name) {
			continue
		}
		if start > 0 && isWordByte(s[start-1]) {
			continue
		}
		if end < len(s) && isWordByte(s[end]) {
			continue
		}
		out = append(out, marker{name: name, start: start, end: end})
	}
	return out
}

// Names returns every marker name in s, in first-appearance order.
func Names(s string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, m := range scanMarkers(s) {
		if seen[m.name] {
			continue
		}
		seen[m.name] = true
		out = append(out, m.name)
	}
	return out
}

// Validate reports an error for every marker name in s that r cannot look up.
// It returns values never, so it is safe to call on the prompt at startup.
func Validate(input string, r Resolver) error {
	names := Names(input)
	if len(names) == 0 {
		return nil
	}
	if r == nil {
		return fmt.Errorf("%w: %s", ErrUnknownName, names[0])
	}
	for _, name := range names {
		secret, err := r.Lookup(name)
		if err != nil {
			return err
		}
		if secret == "" {
			return fmt.Errorf("%w: %s", ErrUnknownName, name)
		}
		if !config.IsSafeValue(secret) {
			return fmt.Errorf("%w: %s", ErrUnsafeValue, name)
		}
	}
	return nil
}

// Redact substitutes every marker in input with the secret r looks up for it.
// On an unknown name or an unsafe value it returns an error and no partial
// result. It also returns the values it substituted.
func Redact(input string, r Resolver) (string, map[string]string, error) {
	markers := scanMarkers(input)
	if len(markers) == 0 {
		return input, map[string]string{}, nil
	}
	if r == nil {
		return "", nil, fmt.Errorf("%w: %s", ErrUnknownName, markers[0].name)
	}
	values := make(map[string]string, len(markers))
	for _, m := range markers {
		if _, ok := values[m.name]; ok {
			continue
		}
		secret, err := r.Lookup(m.name)
		if err != nil {
			return "", nil, err
		}
		if secret == "" {
			return "", nil, fmt.Errorf("%w: %s", ErrUnknownName, m.name)
		}
		if !config.IsSafeValue(secret) {
			return "", nil, fmt.Errorf("%w: %s", ErrUnsafeValue, m.name)
		}
		values[m.name] = secret
	}
	var b strings.Builder
	b.Grow(len(input))
	pos := 0
	for _, m := range markers {
		b.WriteString(input[pos:m.start])
		b.WriteString(values[m.name])
		pos = m.end
	}
	b.WriteString(input[pos:])
	return b.String(), values, nil
}
