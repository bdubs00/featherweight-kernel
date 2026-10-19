// Package anthropic is the fw.Model adapter for the Claude API. It registers
// itself as the "anthropic" provider, so the selector
// "anthropic/claude-opus-5-5" opens it.
//
// The adapter uses the beta messages namespace, streams every call, and
// returns one complete fw.Response. BuildParams and MapResponse hold the
// mapping between fw types and SDK types, so tests and other packages can use
// the mapping without the network.
package anthropic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Defaults that apply when the request and the configuration leave a value
// empty.
const (
	DefaultModel           = "claude-opus-5-5"
	DefaultEffort          = "high"
	DefaultMaxTokens       = 64000
	DefaultSearchThreshold = 24

	// Provider is the name of this provider in selectors and responses.
	Provider = "anthropic"

	// APIKeySecret is the default secret name that holds the API key. The
	// apiKeyEnv field of the provider configuration replaces it.
	APIKeySecret = "ANTHROPIC_API_KEY"

	// BaseURLEnv names an environment variable that replaces the API base
	// URL when the provider configuration sets none.
	BaseURLEnv = "ANTHROPIC_BASE_URL"

	// FallbacksEnv turns the server-side refusal fallback off when it is "off".
	FallbacksEnv = "FW_ANTHROPIC_FALLBACKS"
)

// secretName matches an environment-style secret name such as TEAM_TOKEN.
var secretName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func init() {
	fw.RegisterModel(Provider, New)
}

// Model implements fw.Model on the Claude API.
type Model struct {
	client    anthropic.Client
	model     string
	effort    string
	threshold int
}

// New opens the adapter. An empty model name selects DefaultModel. New reads
// the API key from secrets and fails when the key is missing. A nil cfg uses
// the defaults of this package.
//
// New reads the "anthropic" entry of cfg.Providers. baseUrl replaces the
// API endpoint, before the variable ANTHROPIC_BASE_URL. apiKeyEnv names the
// secret that holds the key, default ANTHROPIC_API_KEY. headers go on every
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
		return nil, errors.New("anthropic: no secrets source; cannot read " + keyName)
	}
	key, err := secrets.Get(ctx, keyName)
	if err != nil {
		return nil, fmt.Errorf("anthropic: %s is not set; set it in the environment or the secrets store: %w", keyName, err)
	}
	if key == "" {
		return nil, fmt.Errorf("anthropic: %s is empty; set it in the environment or the secrets store", keyName)
	}
	var effort string
	threshold := DefaultSearchThreshold
	if cfg != nil {
		effort = cfg.Effort
		if cfg.Tools.SearchThreshold != 0 {
			threshold = cfg.Tools.SearchThreshold
		}
	}
	if _, err := effortOf(effort); err != nil {
		return nil, err
	}
	// The key comes only from secrets. The SDK reads no credential from the
	// environment, so a stray ANTHROPIC_AUTH_TOKEN or profile file cannot add
	// a second credential to the request.
	opts := []option.RequestOption{option.WithoutEnvironmentDefaults(), option.WithAPIKey(key)}
	base := strings.TrimSpace(pc.BaseURL)
	if base == "" {
		base = os.Getenv(BaseURLEnv)
	}
	if base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}
	names := make([]string, 0, len(pc.Headers))
	for name := range pc.Headers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		value, err := headerValue(ctx, secrets, name, pc.Headers[name])
		if err != nil {
			return nil, err
		}
		opts = append(opts, option.WithHeader(name, value))
	}
	if pc.Timeout > 0 {
		opts = append(opts, option.WithRequestTimeout(pc.Timeout))
	}
	return newModel(model, effort, threshold, opts...), nil
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
		return "", fmt.Errorf("anthropic: read header %s from secret %s: %w", name, value, err)
	}
	return strings.TrimSpace(v), nil
}

// newModel builds the adapter from client options. Tests use it to point the
// client at a local server.
func newModel(model, effort string, threshold int, opts ...option.RequestOption) *Model {
	if model == "" {
		model = DefaultModel
	}
	return &Model{
		client:    anthropic.NewClient(opts...),
		model:     model,
		effort:    effort,
		threshold: threshold,
	}
}

// Name returns the model name that the adapter sends.
func (m *Model) Name() string { return m.model }

// Generate implements fw.Model. It is Stream with the deltas ignored.
func (m *Model) Generate(ctx context.Context, req *fw.Request) (*fw.Response, error) {
	return m.Stream(ctx, req, nil)
}

// Stream implements fw.Model. It calls on with a "text" delta for each text
// increment and a "block" delta when a block completes. The returned response
// is authoritative: after a mid-stream refusal fallback, MapResponse drops
// blocks of the declined attempt that a "block" delta already reported.
//
// The SDK retries 429 and 5xx responses itself. Errors wrap the SDK error, so
// errors.As finds *anthropic.Error.
func (m *Model) Stream(ctx context.Context, req *fw.Request, on func(fw.Delta)) (*fw.Response, error) {
	if req == nil {
		return nil, errors.New("anthropic: nil request")
	}
	r := *req
	r.Effort = r.EffortOr(m.effort)
	params, names, err := BuildParams(&r, m.model, m.threshold)
	if err != nil {
		return nil, err
	}

	stream := m.client.Beta.Messages.NewStreaming(ctx, params)
	defer stream.Close()

	var msg anthropic.BetaMessage
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			return nil, fmt.Errorf("anthropic: accumulate stream: %w", err)
		}
		if on == nil {
			continue
		}
		switch ev.Type {
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				on(fw.Delta{Type: "text", Text: ev.Delta.Text})
			}
		case "content_block_stop":
			i := int(ev.Index)
			if i < 0 || i >= len(msg.Content) {
				continue
			}
			b, err := mapBlock(msg.Content[i], names)
			if err != nil {
				return nil, err
			}
			on(fw.Delta{Type: "block", Block: &b})
		}
	}
	if err := stream.Err(); err != nil {
		return nil, fmt.Errorf("anthropic: stream %s: %w", m.model, err)
	}
	if msg.ID == "" {
		return nil, fmt.Errorf("anthropic: stream %s ended before message_start", m.model)
	}
	return MapResponse(&msg, names)
}

// MediaTypes returns the media types of attachments that the model reads:
// PNG, JPEG, GIF, and WebP images, and PDF documents.
func (m *Model) MediaTypes() []string {
	return []string{"image/png", "image/jpeg", "image/gif", "image/webp", "application/pdf"}
}
