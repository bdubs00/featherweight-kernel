package fw

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrSecretNotFound is returned when no source holds the secret.
var ErrSecretNotFound = errors.New("fw: secret not found")

// Secrets resolves a name to a value. Names are environment-style, for
// example ANTHROPIC_API_KEY.
type Secrets interface {
	Get(ctx context.Context, name string) (string, error)
}

// SecretsFunc adapts a function to Secrets.
type SecretsFunc func(ctx context.Context, name string) (string, error)

// Get implements Secrets.
func (f SecretsFunc) Get(ctx context.Context, name string) (string, error) { return f(ctx, name) }

// EnvSecrets reads NAME from the environment, then the file named by NAME_FILE.
type EnvSecrets struct{}

// Get implements Secrets.
func (EnvSecrets) Get(_ context.Context, name string) (string, error) {
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	if p := os.Getenv(name + "_FILE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("fw: read %s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", fmt.Errorf("%w: %s", ErrSecretNotFound, name)
}
