// Package openai is the fw.Model adapter for servers that speak the OpenAI
// chat completions API: OpenAI itself, Azure OpenAI, and local servers such
// as vLLM, Ollama, llama.cpp, and tabbyAPI. It registers itself as the
// "openai" provider, so the selector "openai/gpt-5" opens it. The provider
// has no default model.
//
// The adapter uses net/http and no SDK. It streams every call and returns
// one complete fw.Response. BuildBody holds the mapping from fw types to the
// request body, so tests and other packages can use the mapping without the
// network.
//
// # Configuration
//
// The adapter reads the "openai" entry of the providers map in fw.yaml.
// Every field is optional.
//
//	providers:
//	  openai:
//	    baseUrl: https://api.openai.com/v1
//	    apiKeyEnv: OPENAI_API_KEY
//	    headers:
//	      X-Team: platform
//	    timeout: 10m
//	    tls:
//	      ca: /etc/ssl/private-ca.pem
//	    options:
//	      reasoning: auto
//
// baseUrl is the URL that the adapter appends /chat/completions and /models
// to. The first value that is set wins: baseUrl, then the secret or the
// variable OPENAI_BASE_URL, then https://api.openai.com/v1. A query in the
// URL stays on every request, which the older Azure endpoints need for
// api-version.
//
// apiKeyEnv names the secret that holds the API key. The default is
// OPENAI_API_KEY. A local server often needs no key, so a missing secret is
// not an error: the adapter then sends no credential. When the server
// answers 401 or 403, the error names this secret, never its value.
//
// headers go on every request. A value that looks like a secret name, such
// as PROXY_TOKEN, is replaced by the secret when the secret exists. Any
// other value goes as it is. A configured header replaces the header of the
// same name that the adapter sends, so a proxy credential can replace the
// Authorization header.
//
// timeout is the time limit of one call, retries included. The default is
// no limit beyond the idle timeout below.
//
// tls.ca is a PEM file that signs the server certificate, for a private
// server; it replaces the system roots. tls.cert and tls.key are a client
// certificate and key for mutual TLS. tls.insecure turns certificate checks
// off for development; the adapter logs a warning when it is set.
//
// # Options
//
// options holds the settings of this adapter. An unknown key is an error at
// start that lists the known keys.
//
//   - reasoning: auto, on, or off. Default auto. On sends the effort as
//     reasoning_effort and puts the token limit in max_completion_tokens,
//     which OpenAI reasoning models need. Off sends neither, which most
//     other servers need. Auto reads the variable FW_OPENAI_REASONING (1 or
//     0) and otherwise turns reasoning on for o-series and GPT-5 model
//     names only.
//   - maxTokensField: auto, max_tokens, or max_completion_tokens. Default
//     auto, which follows the reasoning setting.
//   - authHeader: the header that carries the API key. Default
//     Authorization, with the Bearer prefix. Any other name, such as api-key
//     for Azure, sends the key as that header with no prefix.
//   - organization and project: sent as OpenAI-Organization and
//     OpenAI-Project. The secrets or variables OPENAI_ORG_ID and
//     OPENAI_PROJECT_ID are the defaults. Nothing is sent when both are
//     empty.
//   - promptCacheKey: auto, on, or off. Default auto. On sends the run id as
//     prompt_cache_key, which keeps the turns of one run on the same cache
//     shard. Auto sends it only to api.openai.com, because other servers can
//     reject a field that they do not know.
//   - parallelToolCalls: empty, true, or false. Default empty, which sends
//     nothing. false sends parallel_tool_calls: false, so the model makes one
//     tool call per turn.
//   - idleTimeout: the longest wait for the next byte of a response, as a
//     duration such as 90s or 5m. Default 120s. It also bounds the wait for
//     the response headers. A stream that stalls for longer ends with an
//     error that names this option. 0 turns the check off.
//   - mediaTypes: the attachments that the model reads (section 7.10 of
//     docs/ARCHITECTURE.md): auto, none, or a comma-separated list such as
//     image/png,image/jpeg. Default auto, which is PNG, JPEG, GIF, and WebP
//     images, plus PDF for api.openai.com. Set none for a server that reads
//     text only. The agent then gets a note and reads the file with a tool.
//
// # Variables
//
// The secrets store, which by default is the environment, holds these
// names. The adapter reads each one through fw.Secrets and then through the
// environment.
//
//   - OPENAI_API_KEY, or the secret that apiKeyEnv names: the API key.
//   - OPENAI_BASE_URL: the base URL, when baseUrl is not set.
//   - OPENAI_ORG_ID and OPENAI_PROJECT_ID: the organization and the project,
//     when the options are not set.
//   - FW_OPENAI_REASONING: 1 or 0, the reasoning setting when the option is
//     auto. This one is read from the environment only.
//
// # Examples
//
// OpenAI, with the key in OPENAI_API_KEY and nothing else to set:
//
//	model: openai/gpt-5
//
// A local vLLM server on port 8000, or Ollama on port 11434, with no key and
// a long idle timeout for a slow GPU:
//
//	model: openai/qwen3-32b
//	providers:
//	  openai:
//	    baseUrl: http://localhost:8000/v1
//	    options:
//	      idleTimeout: 10m
//
// Azure OpenAI, with the key in AZURE_OPENAI_KEY and the deployment name as
// the model name:
//
//	model: openai/my-gpt-5-deployment
//	providers:
//	  openai:
//	    baseUrl: https://my-resource.openai.azure.com/openai/v1
//	    apiKeyEnv: AZURE_OPENAI_KEY
//	    options:
//	      authHeader: api-key
//
// An older Azure endpoint takes the deployment in the path and the API
// version in the query, which the adapter keeps on every request:
//
//	baseUrl: https://my-resource.openai.azure.com/openai/deployments/my-gpt-5-deployment?api-version=2024-10-21
//
// # Behavior
//
// A 429 or a 5xx answer is retried three times, after the wait that
// Retry-After names or an exponential backoff. A connection error before any
// byte of the answer arrived, such as a refused connection, a reset, or an
// early EOF, is retried the same way, because the request had no effect
// yet. A call is never retried once the stream started. A response larger
// than 16 MB is an error.
//
// A 404 that names an unknown model lists the models that the server serves
// in the error. The Models method returns the same list for a doctor
// command; the Lister interface names it.
package openai

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Names and defaults of this provider.
const (
	// Provider is the name of this provider in selectors, responses, opaque
	// blocks, and the providers map of the configuration.
	Provider = "openai"

	// APIKeySecret is the default secret name that holds the API key. It is
	// optional, because a local server often needs none.
	APIKeySecret = "OPENAI_API_KEY"

	// BaseURLSecret is the secret name, or the environment variable, that
	// holds the base URL of the API when the configuration sets none.
	BaseURLSecret = "OPENAI_BASE_URL"

	// OrganizationSecret and ProjectSecret hold the organization and the
	// project when the options set none.
	OrganizationSecret = "OPENAI_ORG_ID"
	ProjectSecret      = "OPENAI_PROJECT_ID"

	// DefaultBaseURL is the base URL when nothing sets one.
	DefaultBaseURL = "https://api.openai.com/v1"

	// ReasoningEnv set to 1 sends reasoning_effort to any model, and set to
	// 0 sends it to none, when the reasoning option is auto.
	ReasoningEnv = "FW_OPENAI_REASONING"

	// DefaultIdleTimeout is the longest wait for the next byte of a
	// response when the idleTimeout option is not set.
	DefaultIdleTimeout = 120 * time.Second

	// MaxResponseBytes is the most that one response can hold.
	MaxResponseBytes = 16 << 20

	maxRetries = 3
	// maxNetRetries bounds the retries of a connection error that happened
	// before any byte of the answer arrived. It is larger than maxRetries,
	// about fifteen seconds with the default backoff, because under ax a
	// Task boots before its egress policy takes effect, and the first
	// requests of a Task fail on a connection that is refused or reset
	// until the policy is in place.
	maxNetRetries = 5
	maxWait       = 60 * time.Second
	maxBackoff    = 20 * time.Second
	listTimeout   = 15 * time.Second // the wait for the model list on a 404
)

// knownOptions lists the keys of providers.openai.options, sorted.
var knownOptions = []string{
	"authHeader", "idleTimeout", "maxTokensField", "mediaTypes", "organization",
	"parallelToolCalls", "project", "promptCacheKey", "reasoning",
}

// imageTypes are the image types that auto sends to every server.
var imageTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// secretName matches an environment-style secret name such as PROXY_TOKEN.
var secretName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func init() {
	fw.RegisterModel(Provider, New)
}

// Lister lists the models of a server. *Model implements it. A doctor
// command asserts a fw.Model to Lister before it calls Models.
type Lister interface {
	Models(ctx context.Context) ([]string, error)
}

// Compile-time checks: *Model is a fw.Model and a Lister.
var (
	_ fw.Model = (*Model)(nil)
	_ Lister   = (*Model)(nil)
)

// Model implements fw.Model on a chat completions server.
type Model struct {
	model      string
	effort     string
	endpoint   *url.URL // the chat completions URL, query included
	list       *url.URL // the models URL, query included
	key        string
	keyName    string      // the secret that holds the key, for error messages
	auth       string      // the header that carries the key
	headers    http.Header // the organization, the project, and the configured headers
	reasoning  bool
	tokenField string   // max_tokens or max_completion_tokens
	cacheKey   bool     // send the run id as prompt_cache_key
	parallel   *bool    // parallel_tool_calls, when set
	types      []string // the media types of attachments that the model reads
	idle       time.Duration
	timeout    time.Duration
	maxBytes   int64
	client     *http.Client
	backoff    time.Duration // the wait before the first retry without Retry-After
	log        *slog.Logger
}

// settings is what New resolves from the configuration and the secrets
// before it builds a Model. Tests build it by hand.
type settings struct {
	model      string
	effort     string
	base       string
	key        string
	keyName    string
	authHeader string
	headers    map[string]string
	reasoning  bool
	tokenField string // auto, max_tokens, or max_completion_tokens
	cacheKey   string // auto, on, or off
	parallel   string // empty, true, or false
	mediaTypes string // auto, none, or a list
	idle       time.Duration
	timeout    time.Duration
	tls        *tls.Config
}

// options holds the parsed values of providers.openai.options.
type options struct {
	reasoning    string
	tokenField   string
	authHeader   string
	organization string
	project      string
	cacheKey     string
	parallel     string
	mediaTypes   string
	idle         time.Duration
}

// New opens the adapter. The model name is required. New reads the "openai"
// entry of cfg.Providers; the package comment lists every field, option,
// and variable with its default. A nil cfg uses the defaults, and a nil
// secrets reads the environment.
func New(ctx context.Context, model string, cfg *fw.Config, secrets fw.Secrets) (fw.Model, error) {
	if model == "" {
		return nil, errors.New("openai: no model name; use a selector such as openai/gpt-5 or openai/qwen3")
	}
	if secrets == nil {
		secrets = fw.EnvSecrets{}
	}
	var (
		pc     fw.ProviderConfig
		effort string
	)
	if cfg != nil {
		pc = cfg.ProviderNamed(Provider)
		effort = cfg.Effort
	}
	if _, err := effortOf(effort); err != nil {
		return nil, err
	}
	opts, err := parseOptions(pc.Options)
	if err != nil {
		return nil, err
	}
	keyName := strings.TrimSpace(pc.APIKeyEnv)
	if keyName == "" {
		keyName = APIKeySecret
	}
	key, err := optional(ctx, secrets, keyName)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSpace(pc.BaseURL)
	if base == "" {
		if base, err = lookup(ctx, secrets, BaseURLSecret); err != nil {
			return nil, err
		}
	}
	if base == "" {
		base = DefaultBaseURL
	}
	org := opts.organization
	if org == "" {
		if org, err = lookup(ctx, secrets, OrganizationSecret); err != nil {
			return nil, err
		}
	}
	project := opts.project
	if project == "" {
		if project, err = lookup(ctx, secrets, ProjectSecret); err != nil {
			return nil, err
		}
	}
	headers := map[string]string{}
	if org != "" {
		headers["OpenAI-Organization"] = org
	}
	if project != "" {
		headers["OpenAI-Project"] = project
	}
	for name, value := range pc.Headers {
		if headers[name], err = resolveHeader(ctx, secrets, name, value); err != nil {
			return nil, err
		}
	}
	reasoning, err := resolveReasoning(opts.reasoning, model)
	if err != nil {
		return nil, err
	}
	tc, err := loadTLS(pc.TLS)
	if err != nil {
		return nil, err
	}
	m, err := newModel(settings{
		model:      model,
		effort:     effort,
		base:       base,
		key:        key,
		keyName:    keyName,
		authHeader: opts.authHeader,
		headers:    headers,
		reasoning:  reasoning,
		tokenField: opts.tokenField,
		cacheKey:   opts.cacheKey,
		parallel:   opts.parallel,
		mediaTypes: opts.mediaTypes,
		idle:       opts.idle,
		timeout:    pc.Timeout,
		tls:        tc,
	})
	if err != nil {
		return nil, err
	}
	if pc.TLS.Insecure {
		m.log.Warn("openai: tls.insecure is set; the server certificate is not checked", "host", m.endpoint.Host)
	}
	return m, nil
}

// newModel builds the adapter. Tests use it to point the adapter at a local
// server.
func newModel(s settings) (*Model, error) {
	u, err := url.Parse(strings.TrimSpace(s.base))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("openai: the base URL is not an http or https URL; set providers.openai.baseUrl or %s", BaseURLSecret)
	}
	path := strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	u.Fragment = ""
	endpoint := *u
	endpoint.Path = path + "/chat/completions"
	list := *u
	list.Path = path + "/models"

	tokenField := s.tokenField
	if tokenField == "" || tokenField == "auto" {
		tokenField = "max_tokens"
		if s.reasoning {
			tokenField = "max_completion_tokens"
		}
	}
	var cacheKey bool
	switch s.cacheKey {
	case "on":
		cacheKey = true
	case "off":
		cacheKey = false
	default:
		cacheKey = strings.EqualFold(u.Hostname(), "api.openai.com")
	}
	var parallel *bool
	switch s.parallel {
	case "true":
		parallel = new(bool)
		*parallel = true
	case "false":
		parallel = new(bool)
	}
	auth := strings.TrimSpace(s.authHeader)
	if auth == "" {
		auth = "Authorization"
	}
	keyName := s.keyName
	if keyName == "" {
		keyName = APIKeySecret
	}
	types, err := parseMediaTypes(s.mediaTypes, u.Hostname())
	if err != nil {
		return nil, fmt.Errorf("openai: option mediaTypes: %w", err)
	}
	headers := http.Header{}
	for k, v := range s.headers {
		headers.Set(k, v)
	}
	return &Model{
		model:      s.model,
		effort:     s.effort,
		endpoint:   &endpoint,
		list:       &list,
		key:        s.key,
		keyName:    keyName,
		auth:       auth,
		headers:    headers,
		reasoning:  s.reasoning,
		tokenField: tokenField,
		cacheKey:   cacheKey,
		parallel:   parallel,
		types:      types,
		idle:       s.idle,
		timeout:    s.timeout,
		maxBytes:   MaxResponseBytes,
		client:     &http.Client{Transport: newTransport(s.tls, s.idle)},
		backoff:    500 * time.Millisecond,
		log:        slog.Default().With("component", "openai"),
	}, nil
}

// parseOptions reads providers.openai.options. It fills the defaults and
// rejects an unknown key or a value outside the documented set.
func parseOptions(in map[string]string) (options, error) {
	o := options{
		reasoning:  "auto",
		tokenField: "auto",
		authHeader: "Authorization",
		cacheKey:   "auto",
		mediaTypes: "auto",
		idle:       DefaultIdleTimeout,
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v := strings.TrimSpace(in[k])
		switch k {
		case "reasoning":
			if o.reasoning = strings.ToLower(v); !oneOf(o.reasoning, "auto", "on", "off") {
				return o, optionError(k, v, "auto, on, or off")
			}
		case "maxTokensField":
			if o.tokenField = strings.ToLower(v); !oneOf(o.tokenField, "auto", "max_tokens", "max_completion_tokens") {
				return o, optionError(k, v, "auto, max_tokens, or max_completion_tokens")
			}
		case "authHeader":
			if v == "" || strings.ContainsAny(v, " :\t\r\n") {
				return o, optionError(k, v, "a header name such as Authorization or api-key")
			}
			o.authHeader = v
		case "organization":
			o.organization = v
		case "project":
			o.project = v
		case "promptCacheKey":
			if o.cacheKey = strings.ToLower(v); !oneOf(o.cacheKey, "auto", "on", "off") {
				return o, optionError(k, v, "auto, on, or off")
			}
		case "parallelToolCalls":
			if o.parallel = strings.ToLower(v); !oneOf(o.parallel, "", "true", "false") {
				return o, optionError(k, v, "true, false, or empty")
			}
		case "mediaTypes":
			o.mediaTypes = strings.ToLower(v)
			if _, err := parseMediaTypes(o.mediaTypes, ""); err != nil {
				return o, optionError(k, v, "auto, none, or a comma-separated list such as image/png,image/jpeg")
			}
		case "idleTimeout":
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				return o, optionError(k, v, "a duration such as 90s or 5m, or 0 for no check")
			}
			o.idle = d
		default:
			return o, fmt.Errorf("openai: unknown option %q in providers.openai.options (known: %s)", k, strings.Join(knownOptions, ", "))
		}
	}
	return o, nil
}

func optionError(key, value, want string) error {
	return fmt.Errorf("openai: option %s is %q, want %s", key, value, want)
}

func oneOf(v string, set ...string) bool {
	return slices.Contains(set, v)
}

// resolveReasoning maps the reasoning option to a decision for one model.
// Auto reads ReasoningEnv, then the model name.
func resolveReasoning(opt, model string) (bool, error) {
	switch opt {
	case "on":
		return true, nil
	case "off":
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ReasoningEnv))) {
	case "1", "true", "on":
		return true, nil
	case "0", "false", "off":
		return false, nil
	case "":
		return reasoningModel(model), nil
	}
	return false, fmt.Errorf("openai: %s is %q, want 1 or 0", ReasoningEnv, os.Getenv(ReasoningEnv))
}

// resolveHeader returns the value of a configured header: the secret of that
// name when the value looks like a secret name and the secret exists, else
// the value as it is.
func resolveHeader(ctx context.Context, secrets fw.Secrets, name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if !secretName.MatchString(value) {
		return value, nil
	}
	v, err := secrets.Get(ctx, value)
	if errors.Is(err, fw.ErrSecretNotFound) {
		return value, nil
	}
	if err != nil {
		return "", fmt.Errorf("openai: read header %s from secret %s: %w", name, value, err)
	}
	return strings.TrimSpace(v), nil
}

// optional reads a secret that can be absent. A missing secret is empty.
func optional(ctx context.Context, secrets fw.Secrets, name string) (string, error) {
	v, err := secrets.Get(ctx, name)
	if errors.Is(err, fw.ErrSecretNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("openai: read %s: %w", name, err)
	}
	return strings.TrimSpace(v), nil
}

// lookup reads a setting that can be absent: the secret of that name, then
// the environment variable.
func lookup(ctx context.Context, secrets fw.Secrets, name string) (string, error) {
	v, err := optional(ctx, secrets, name)
	if err != nil || v != "" {
		return v, err
	}
	return strings.TrimSpace(os.Getenv(name)), nil
}

// reasoningModel reports whether a model name is an OpenAI reasoning model:
// o1, o3, o4-mini, and the like, or a GPT-5 model. A provider prefix such as
// "openai/" is ignored.
func reasoningModel(model string) bool {
	base := strings.ToLower(model[strings.LastIndexByte(model, '/')+1:])
	if len(base) > 1 && base[0] == 'o' && base[1] >= '0' && base[1] <= '9' {
		return true
	}
	return strings.Contains(base, "gpt-5")
}

// Name returns the model name that the adapter sends.
func (m *Model) Name() string { return m.model }

// Generate implements fw.Model. It is Stream with the deltas ignored.
func (m *Model) Generate(ctx context.Context, req *fw.Request) (*fw.Response, error) {
	return m.Stream(ctx, req, nil)
}

// Stream implements fw.Model. It calls on with a "text" delta for each text
// increment and a "block" delta for each block when the stream ends.
//
// A 429 or 5xx answer, and a connection error before any byte of the answer
// arrived, is retried three times after the wait that Retry-After names or
// an exponential backoff. Another error status fails at once with an
// *APIError. A stream that sends no byte for the idle timeout, or that
// grows past MaxResponseBytes, fails with an error that says so.
func (m *Model) Stream(ctx context.Context, req *fw.Request, on func(fw.Delta)) (*fw.Response, error) {
	if req == nil {
		return nil, errors.New("openai: nil request")
	}
	r := *req
	r.Effort = r.EffortOr(m.effort)
	body, names, err := BuildBody(&r, m.model, m.bodyOptions(&r))
	if err != nil {
		return nil, err
	}
	ctx, done := m.deadline(ctx)
	defer done()
	call, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	resp, err := m.send(call, http.MethodPost, m.endpoint, body)
	if err != nil {
		return nil, m.withModels(ctx, err)
	}
	defer resp.Body.Close()

	rd := m.guard(resp.Body, func() { cancel(errStalled) })
	defer rd.stop()
	acc := newAccumulator(names, on)
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt == "application/json" {
		// A server that ignores stream: true answers with one completion.
		err = acc.readJSON(rd)
	} else {
		err = acc.readSSE(rd)
	}
	if err != nil {
		return nil, fmt.Errorf("openai: stream %s: %w", m.model, m.explain(call, err))
	}
	return acc.response(m.model)
}

// bodyOptions returns the body settings of one request.
func (m *Model) bodyOptions(req *fw.Request) BodyOptions {
	o := BodyOptions{
		Reasoning:         m.reasoning,
		MaxTokensField:    m.tokenField,
		ParallelToolCalls: m.parallel,
	}
	if m.cacheKey {
		o.PromptCacheKey = req.Metadata["runId"]
	}
	return o
}

// Models lists the ids of the models that the server serves, from GET
// {base}/models, sorted and without duplicates. A doctor command uses it to
// check a connection and to show what the model name can be.
func (m *Model) Models(ctx context.Context) ([]string, error) {
	ctx, done := m.deadline(ctx)
	defer done()
	call, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	resp, err := m.send(call, http.MethodGet, m.list, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rd := m.guard(resp.Body, func() { cancel(errStalled) })
	defer rd.stop()
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rd).Decode(&doc); err != nil {
		return nil, fmt.Errorf("openai: GET %s: read model list: %w", clean(m.list), m.explain(call, err))
	}
	ids := make([]string, 0, len(doc.Data))
	for _, d := range doc.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// withModels adds the model list of the server to a 404 that names an
// unknown model, so the user sees what to pick. Any other error comes back
// as it is.
func (m *Model) withModels(ctx context.Context, err error) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || !apiErr.aboutModel() || ctx.Err() != nil {
		return err
	}
	lctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	ids, lerr := m.Models(lctx)
	if lerr != nil || len(ids) == 0 {
		m.logger(ctx).Debug("openai: no model list for the 404 hint", "err", lerr)
		return err
	}
	const show = 40
	more := ""
	if len(ids) > show {
		more = fmt.Sprintf(", and %d more", len(ids)-show)
		ids = ids[:show]
	}
	return fmt.Errorf("%w; the server serves these models: %s%s", err, strings.Join(ids, ", "), more)
}

// deadline bounds a call by the configured timeout, when there is one.
func (m *Model) deadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeoutCause(ctx, m.timeout, &timeoutError{limit: m.timeout})
}

// logger returns the logger of the agent in the context, or the logger of
// the adapter.
func (m *Model) logger(ctx context.Context) *slog.Logger {
	if a := fw.AgentFrom(ctx); a != nil {
		return a.Logger().With("component", "openai")
	}
	return m.log
}

// MediaTypes returns the media types of attachments that the model reads.
func (m *Model) MediaTypes() []string { return m.types }

// parseMediaTypes reads the option mediaTypes for the server at host.
func parseMediaTypes(v, host string) ([]string, error) {
	switch v {
	case "", "auto":
		if strings.EqualFold(host, "api.openai.com") {
			return append(slices.Clone(imageTypes), "application/pdf"), nil
		}
		return slices.Clone(imageTypes), nil
	case "none":
		return nil, nil
	}
	var out []string
	for _, t := range strings.Split(v, ",") {
		t = strings.TrimSpace(t)
		main, sub, ok := strings.Cut(t, "/")
		if !ok || main == "" || sub == "" || strings.ContainsAny(t, " ;") {
			return nil, fmt.Errorf("%q is not a media type such as image/png", t)
		}
		out = append(out, t)
	}
	return out, nil
}
