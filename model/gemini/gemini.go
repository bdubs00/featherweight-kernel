// Package gemini is the fw.Model adapter for the Gemini API. It registers
// itself as the "gemini" provider, so the selector "gemini/gemini-3.8-flash"
// opens it.
//
// The adapter uses google.golang.org/genai, streams every call, and returns
// one complete fw.Response. BuildRequest and MapResponse hold the mapping
// between fw types and SDK types, so tests and other packages can use the
// mapping without the network.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"google.golang.org/genai"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Defaults and names of this provider.
const (
	DefaultModel = "gemini-3.8-flash"

	// Provider is the name of this provider in selectors, responses, and
	// opaque blocks.
	Provider = "gemini"

	// APIKeySecret is the default secret name that holds the API key. The
	// apiKeyEnv field of the provider configuration replaces it.
	APIKeySecret = "GEMINI_API_KEY"

	// AltAPIKeySecret is read when APIKeySecret is not set.
	AltAPIKeySecret = "GOOGLE_API_KEY"

	// BaseURLEnv names an environment variable that replaces the API base
	// URL, for example a proxy, when the provider configuration sets none.
	BaseURLEnv = "GEMINI_BASE_URL"

	// retryAttempts is the number of attempts for a call that fails with
	// 408, 429, or 5xx, including the first attempt.
	retryAttempts = 4
)

// secretName matches an environment-style secret name such as TEAM_TOKEN.
var secretName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func init() {
	fw.RegisterModel(Provider, New)
}

// Model implements fw.Model on the Gemini API.
type Model struct {
	client *genai.Client
	model  string
	effort string
}

// New opens the adapter. An empty model name selects DefaultModel. New reads
// the API key from GEMINI_API_KEY, then GOOGLE_API_KEY, through secrets, and
// fails when neither is set. A nil cfg leaves the effort at the default of
// the model.
//
// New reads the "gemini" entry of cfg.Providers. baseUrl replaces the API
// endpoint, before the variable GEMINI_BASE_URL. apiKeyEnv names the secret
// that holds the key, in place of GEMINI_API_KEY. headers go on every
// request; a value that is a secret name is replaced by the secret when it
// exists, and any other value goes as it is. timeout is the time limit of
// one request.
func New(ctx context.Context, model string, cfg *fw.Config, secrets fw.Secrets) (fw.Model, error) {
	var pc fw.ProviderConfig
	if cfg != nil {
		pc = cfg.ProviderNamed(Provider)
	}
	keyName := strings.TrimSpace(pc.APIKeyEnv)
	if keyName == "" {
		keyName = APIKeySecret
	}
	if secrets == nil {
		return nil, errors.New("gemini: no secrets source; cannot read " + keyName)
	}
	key, err := apiKey(ctx, secrets, keyName)
	if err != nil {
		return nil, err
	}
	var effort string
	if cfg != nil {
		effort = cfg.Effort
	}
	if err := checkEffort(effort); err != nil {
		return nil, err
	}
	base := strings.TrimSpace(pc.BaseURL)
	if base == "" {
		base = os.Getenv(BaseURLEnv)
	}
	headers := http.Header{}
	for name, value := range pc.Headers {
		v, err := headerValue(ctx, secrets, name, value)
		if err != nil {
			return nil, err
		}
		headers.Set(name, v)
	}
	cc := &genai.ClientConfig{
		APIKey:  key,
		Backend: genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{
			BaseURL:      base,
			Headers:      headers,
			RetryOptions: &genai.HTTPRetryOptions{Attempts: genai.Ptr(int32(retryAttempts))},
		},
	}
	if pc.Timeout > 0 {
		cc.HTTPOptions.Timeout = genai.Ptr(pc.Timeout)
	}
	return newModel(ctx, model, effort, cc)
}

// headerValue returns the value of a configured header: the secret of that
// name when the value looks like a secret name and the secret exists, else
// the value as it is.
func headerValue(ctx context.Context, secrets fw.Secrets, name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if !secretName.MatchString(value) {
		return value, nil
	}
	v, err := secrets.Get(ctx, value)
	if errors.Is(err, fw.ErrSecretNotFound) {
		return value, nil
	}
	if err != nil {
		return "", fmt.Errorf("gemini: read header %s from secret %s: %w", name, value, err)
	}
	return strings.TrimSpace(v), nil
}

// newModel builds the adapter from a client configuration. Tests use it to
// point the client at a local server.
func newModel(ctx context.Context, model, effort string, cc *genai.ClientConfig) (*Model, error) {
	if model == "" {
		model = DefaultModel
	}
	key := cc.APIKey
	client, err := genai.NewClient(ctx, cc)
	if err != nil {
		// Some SDK errors print the whole client configuration, key included.
		return nil, fmt.Errorf("gemini: new client: %w", redacted{err: err, secret: key})
	}
	return &Model{client: client, model: model, effort: effort}, nil
}

// apiKey reads the named secret, then AltAPIKeySecret.
func apiKey(ctx context.Context, secrets fw.Secrets, name string) (string, error) {
	key, err := secrets.Get(ctx, name)
	if err == nil && key != "" {
		return key, nil
	}
	alt, altErr := secrets.Get(ctx, AltAPIKeySecret)
	if altErr == nil && alt != "" {
		return alt, nil
	}
	if err == nil {
		err = fmt.Errorf("%s is empty", name)
	}
	return "", fmt.Errorf("gemini: neither %s nor %s is set; set one in the environment or the secrets store: %w",
		name, AltAPIKeySecret, err)
}

// Name returns the model name that the adapter sends.
func (m *Model) Name() string { return m.model }

// Generate implements fw.Model. It is Stream with the deltas ignored.
func (m *Model) Generate(ctx context.Context, req *fw.Request) (*fw.Response, error) {
	return m.Stream(ctx, req, nil)
}

// Stream implements fw.Model. It calls on with a "text" delta for each text
// increment and a "block" delta when a block completes. A block completes
// when the next part starts or the stream ends.
//
// The SDK retries 408, 429, and 5xx responses. Errors wrap the SDK error, so
// errors.As finds genai.APIError.
func (m *Model) Stream(ctx context.Context, req *fw.Request, on func(fw.Delta)) (*fw.Response, error) {
	if req == nil {
		return nil, errors.New("gemini: nil request")
	}
	r := *req
	r.Effort = r.EffortOr(m.effort)
	contents, config, names, err := BuildRequest(&r, m.model)
	if err != nil {
		return nil, err
	}
	acc := newAccumulator(names, on)
	for chunk, err := range m.client.Models.GenerateContentStream(ctx, m.model, contents, config) {
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gemini: stream %s: %w", m.model, err)
		}
		if err := acc.add(chunk); err != nil {
			return nil, err
		}
	}
	return acc.response(m.model)
}

// redacted wraps an error and hides a secret in its message.
type redacted struct {
	err    error
	secret string
}

func (r redacted) Error() string {
	msg := r.err.Error()
	if r.secret != "" {
		msg = strings.ReplaceAll(msg, r.secret, "[redacted]")
	}
	return msg
}

func (r redacted) Unwrap() error { return r.err }

// MediaTypes returns the media types of attachments that the model reads:
// PNG, JPEG, GIF, and WebP images, and PDF documents.
func (m *Model) MediaTypes() []string {
	return []string{"image/png", "image/jpeg", "image/gif", "image/webp", "application/pdf"}
}
