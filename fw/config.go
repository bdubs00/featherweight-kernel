package fw

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the resolved configuration of one fw process. Keys match
// examples/fw.yaml. Every field has a default from DefaultConfig.
type Config struct {
	Listen     string                    `yaml:"listen" json:"listen"`
	Home       string                    `yaml:"home" json:"home"`
	TLS        ListenTLSConfig           `yaml:"tls" json:"tls"`
	Auth       AuthConfig                `yaml:"auth" json:"auth"`
	Access     AccessConfig              `yaml:"access" json:"access"`
	Model      string                    `yaml:"model" json:"model"`
	Models     map[string]string         `yaml:"models" json:"models,omitempty"`
	Providers  map[string]ProviderConfig `yaml:"providers" json:"providers,omitempty"`
	Effort     string                    `yaml:"effort" json:"effort"`
	Secrets    SecretsConfig             `yaml:"secrets" json:"secrets"`
	Ledger     LedgerConfig              `yaml:"ledger" json:"ledger"`
	Bus        BusConfig                 `yaml:"bus" json:"bus"`
	Tools      ToolsConfig               `yaml:"tools" json:"tools"`
	Policy     PolicyConfig              `yaml:"policy" json:"policy"`
	Budget     Budget                    `yaml:"budget" json:"budget"`
	Compaction CompactionConfig          `yaml:"compaction" json:"compaction"`
	Time       TimeConfig                `yaml:"time" json:"time"`
	Spawn      SpawnConfig               `yaml:"spawn" json:"spawn"`
	Prices     map[string]Price          `yaml:"prices" json:"prices,omitempty"`
	Agents     map[string]AgentProfile   `yaml:"agents" json:"agents,omitempty"`
	Surfaces   []string                  `yaml:"surfaces" json:"surfaces"`
	A2A        A2AConfig                 `yaml:"a2a" json:"a2a"`
	MCP        MCPSurfaceConfig          `yaml:"mcp" json:"mcp"`
	AX         AXConfig                  `yaml:"ax" json:"ax"`
	Telemetry  TelemetryConfig           `yaml:"telemetry" json:"telemetry"`
	MaxRuns    int                       `yaml:"maxRuns" json:"maxRuns"`
	Version    string                    `yaml:"-" json:"version,omitempty"`
	// ConfigFile is the configuration file that the loader read, or "".
	ConfigFile string `yaml:"-" json:"-"`
	// FileProfiles names the profiles that the loader read from the files
	// under <home>/agents. Every other profile comes from fw.yaml, from a
	// FW_AGENTS_* variable, or from a Go option, and the configuration
	// wins over a file at the next start (see Agent.ProfileLocked).
	FileProfiles map[string]bool `yaml:"-" json:"-"`
	// Extensions holds the top-level blocks of fw.yaml that a linked package
	// claimed with RegisterExtension, by key, as the loader read them. The
	// kernel reads none of them, and no variable sets them. Extension
	// decodes one.
	Extensions map[string]yaml.Node `yaml:",inline" json:"-"`
}

// Extension decodes the block of a claimed top-level key into out, which is
// a pointer to a struct. It reports whether the file had the block. A
// block that does not fit out is an error that names the key.
func (c *Config) Extension(name string, out any) (bool, error) {
	n, ok := c.Extensions[name]
	if !ok || n.Kind == 0 {
		return false, nil
	}
	if err := n.Decode(out); err != nil {
		return true, fmt.Errorf("fw: configuration block %s: %w", name, err)
	}
	return true, nil
}

// AuthConfig selects the Authn plug.
//
// Mode is one mode, or several separated by commas that are tried in
// order, for example "oidc,spiffe,token". The modes are token (a static
// bearer token from the secret TokenEnv), none, oidc (a bearer token from
// an OpenID Connect issuer), and spiffe (a JWT-SVID bearer token or an
// X.509-SVID client certificate). A driver registers each of the last two.
type AuthConfig struct {
	Mode     string        `yaml:"mode" json:"mode"`
	TokenEnv string        `yaml:"tokenEnv" json:"tokenEnv"`
	OIDC     *OIDCConfig   `yaml:"oidc,omitempty" json:"oidc,omitempty"`
	SPIFFE   *SPIFFEConfig `yaml:"spiffe,omitempty" json:"spiffe,omitempty"`
}

// Modes returns the authentication modes of the configuration, in order.
func (c AuthConfig) Modes() []string {
	var out []string
	for _, m := range strings.Split(c.Mode, ",") {
		if m = strings.ToLower(strings.TrimSpace(m)); m != "" {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		out = []string{"token"}
	}
	return out
}

// OIDCConfig configures the oidc authentication mode. Issuer is the issuer
// URL, and the adapter reads the discovery document under it. Audience
// lists the accepted audiences: a client id, or the public URL of this
// server. GroupsClaim and NameClaim name the claims that fill
// Principal.Groups and Principal.Name. Scopes are the scopes that the
// protected-resource metadata advertises to clients. CA is a PEM bundle
// for the issuer, and Insecure allows an http issuer for tests.
//
// ClientID turns on the browser sign-in of the workbench: its page sends
// the person to the issuer with the authorization code flow and PKCE, and
// the workbench exchanges the code at GET /ui/callback. ClientSecret is the Secrets
// name of the client secret for a confidential client, and empty for a
// public client. RedirectURL is the callback address that the issuer knows,
// and defaults to /ui/callback on the public address of the page.
type OIDCConfig struct {
	Issuer       string   `yaml:"issuer" json:"issuer"`
	Audience     []string `yaml:"audience,omitempty" json:"audience,omitempty"`
	JWKSURL      string   `yaml:"jwksUrl,omitempty" json:"jwksUrl,omitempty"`
	GroupsClaim  string   `yaml:"groupsClaim,omitempty" json:"groupsClaim,omitempty"`
	NameClaim    string   `yaml:"nameClaim,omitempty" json:"nameClaim,omitempty"`
	Scopes       []string `yaml:"scopes,omitempty" json:"scopes,omitempty"`
	CA           string   `yaml:"ca,omitempty" json:"ca,omitempty"`
	Insecure     bool     `yaml:"insecure,omitempty" json:"insecure,omitempty"`
	ClientID     string   `yaml:"clientId,omitempty" json:"clientId,omitempty"`
	ClientSecret string   `yaml:"clientSecret,omitempty" json:"clientSecret,omitempty"` // a Secrets name
	RedirectURL  string   `yaml:"redirectUrl,omitempty" json:"redirectUrl,omitempty"`
}

// SPIFFEConfig configures the spiffe authentication mode. TrustDomain is
// the trust domain of the accepted identities. Bundle is a path or a URL
// of a SPIFFE bundle in JWKS form; without it the adapter fetches bundles
// from the Workload API at Socket, which defaults to SPIFFE_ENDPOINT_SOCKET.
// Audience lists the accepted JWT-SVID audiences. X509 accepts an
// X.509-SVID presented as a client certificate on a TLS listener.
type SPIFFEConfig struct {
	TrustDomain string   `yaml:"trustDomain" json:"trustDomain"`
	Bundle      string   `yaml:"bundle,omitempty" json:"bundle,omitempty"`
	Socket      string   `yaml:"socket,omitempty" json:"socket,omitempty"`
	Audience    []string `yaml:"audience,omitempty" json:"audience,omitempty"`
	X509        bool     `yaml:"x509,omitempty" json:"x509,omitempty"`
}

// ListenTLSConfig configures TLS on the one listener. Provider selects who
// builds the TLS configuration: "" serves plain HTTP with unencrypted
// HTTP/2, "file" uses Cert and Key, and a driver such as "spiffe" fetches a
// certificate from the Workload API. ClientCA is a PEM bundle that signs
// client certificates, and ClientAuth is "none", "request", or "require".
type ListenTLSConfig struct {
	Provider   string `yaml:"provider,omitempty" json:"provider,omitempty"`
	Cert       string `yaml:"cert,omitempty" json:"cert,omitempty"`
	Key        string `yaml:"key,omitempty" json:"key,omitempty"`
	ClientCA   string `yaml:"clientCA,omitempty" json:"clientCA,omitempty"`
	ClientAuth string `yaml:"clientAuth,omitempty" json:"clientAuth,omitempty"`
}

// ProviderConfig is the connection of one model provider, keyed by the
// provider name in Config.Providers, for example "openai" or "anthropic".
// Every field is optional, and the adapter of the provider documents the
// defaults and the options that it reads. BaseURL replaces the endpoint of
// the provider, so a local server or a proxy needs no code change. APIKeyEnv
// names the secret that holds the API key, when the default name of the
// adapter is not the one in use.
type ProviderConfig struct {
	BaseURL   string            `yaml:"baseUrl,omitempty" json:"baseUrl,omitempty"`
	APIKeyEnv string            `yaml:"apiKeyEnv,omitempty" json:"apiKeyEnv,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"` // values resolve through Secrets
	Timeout   time.Duration     `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	TLS       TLSConfig         `yaml:"tls" json:"tls"`
	Options   map[string]string `yaml:"options,omitempty" json:"options,omitempty"`
}

// ProviderNamed returns the configuration of a provider, or a zero value.
func (c *Config) ProviderNamed(name string) ProviderConfig {
	if c == nil || c.Providers == nil {
		return ProviderConfig{}
	}
	return c.Providers[name]
}

// SecretsConfig selects the Secrets driver.
type SecretsConfig struct {
	Driver  string            `yaml:"driver" json:"driver"`
	Options map[string]string `yaml:"options,omitempty" json:"options,omitempty"`
}

// LedgerConfig selects the Ledger driver.
type LedgerConfig struct {
	Driver  string            `yaml:"driver" json:"driver"`
	Path    string            `yaml:"path,omitempty" json:"path,omitempty"`
	URL     string            `yaml:"url,omitempty" json:"url,omitempty"`
	Options map[string]string `yaml:"options,omitempty" json:"options,omitempty"`
}

// BusConfig selects the Bus driver and its uses.
type BusConfig struct {
	Driver      string            `yaml:"driver" json:"driver"`
	URL         string            `yaml:"url,omitempty" json:"url,omitempty"`
	Intake      string            `yaml:"intake,omitempty" json:"intake,omitempty"`
	Egress      string            `yaml:"egress,omitempty" json:"egress,omitempty"`
	EgressTypes []string          `yaml:"egressTypes,omitempty" json:"egressTypes,omitempty"`
	Options     map[string]string `yaml:"options,omitempty" json:"options,omitempty"`
}

// ToolsConfig configures built-in tools and tool sources.
//
// Limits apply to every tool call of every tool, from the built-in tools,
// from MCP servers, and from Go options alike. Skills lists directories that
// hold skills (see LoadSkills); the default is the skills directory under
// the home directory.
type ToolsConfig struct {
	Builtin         []string          `yaml:"builtin" json:"builtin"`
	Files           FilesToolConfig   `yaml:"files" json:"files"`
	HTTP            HTTPToolConfig    `yaml:"http" json:"http"`
	Bash            BashToolConfig    `yaml:"bash" json:"bash"`
	Limits          ToolLimits        `yaml:"limits" json:"limits"`
	Skills          []string          `yaml:"skills,omitempty" json:"skills,omitempty"`
	MCP             []MCPServerConfig `yaml:"mcp,omitempty" json:"mcp,omitempty"`
	Peers           []PeerConfig      `yaml:"peers,omitempty" json:"peers,omitempty"`
	Memory          MemoryToolConfig  `yaml:"memory" json:"memory"`
	SearchThreshold int               `yaml:"searchThreshold" json:"searchThreshold"`
}

// FilesToolConfig configures the workspace: the directory of the files
// tool and bash, and the rules of the kernel for it (section 7.9). Deny
// lists the paths that are never read or written, ReadOnly the paths that
// are never written, with the patterns of .gitignore. MaxFileBytes bounds
// a file that is read or written.
type FilesToolConfig struct {
	Root         string   `yaml:"root" json:"root"`
	Deny         []string `yaml:"deny" json:"deny"`
	ReadOnly     []string `yaml:"readOnly" json:"readOnly"`
	MaxFileBytes int64    `yaml:"maxFileBytes" json:"maxFileBytes"`
}

// defaultDeny is the default of tools.files.deny: files that usually hold
// a secret.
var defaultDeny = []string{".env", ".env.*", "*.pem", "*.key", ".netrc", "**/.git/config"}

// WorkspaceRoot returns the root of the workspace: tools.files.root, or
// the working directory.
func (c *Config) WorkspaceRoot() string {
	if c.Tools.Files.Root != "" {
		return c.Tools.Files.Root
	}
	return "."
}

// HTTPToolConfig configures the http tool.
type HTTPToolConfig struct {
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"`
}

// BashToolConfig configures the bash tool. Env holds variables that every
// shell starts with, so a script inside the sandbox reads an endpoint from
// its environment instead of from a literal in a prompt. Values resolve
// through Secrets. Timeout is the limit of one command.
type BashToolConfig struct {
	Env     map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Timeout time.Duration     `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// Limit bounds one tool call. A zero field means no limit for that field.
//
// A call whose input is larger than MaxInputBytes does not run; the model
// reads an error result that names the limit. A result larger than
// MaxOutputBytes is cut: the result keeps its head and its tail and a note
// says how many bytes were cut. Timeout ends a call that runs too long.
type Limit struct {
	MaxInputBytes  int           `yaml:"maxInputBytes,omitempty" json:"maxInputBytes,omitempty"`
	MaxOutputBytes int           `yaml:"maxOutputBytes,omitempty" json:"maxOutputBytes,omitempty"`
	Timeout        time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// Merge returns l with every zero field taken from o.
func (l Limit) Merge(o Limit) Limit {
	if l.MaxInputBytes == 0 {
		l.MaxInputBytes = o.MaxInputBytes
	}
	if l.MaxOutputBytes == 0 {
		l.MaxOutputBytes = o.MaxOutputBytes
	}
	if l.Timeout == 0 {
		l.Timeout = o.Timeout
	}
	return l
}

// ToolLimits is the default limit of every tool call plus overrides per
// tool. A key of PerTool is a tool name or a glob over tool names, such as
// "git.*". The first matching key in sorted order wins, and an exact name
// wins over a glob.
type ToolLimits struct {
	Limit   `yaml:",inline" json:",inline"`
	PerTool map[string]Limit `yaml:"perTool,omitempty" json:"perTool,omitempty"`
}

// For returns the limit of one tool: the override of the tool merged with
// the default limit.
func (t ToolLimits) For(name string) Limit {
	if l, ok := t.PerTool[name]; ok {
		return l.Merge(t.Limit)
	}
	var best string
	for pattern := range t.PerTool {
		if ok, err := path.Match(pattern, name); err == nil && ok {
			if best == "" || pattern < best {
				best = pattern
			}
		}
	}
	if best != "" {
		return t.PerTool[best].Merge(t.Limit)
	}
	return t.Limit
}

// CompactionConfig sets when the kernel compacts the history of a run
// (docs/ARCHITECTURE.md section 5.4). At is the size of the context, in
// estimated tokens, that starts a compaction before the next model call.
// Zero turns compaction off, both before a call and after a call that fails
// because the context is too long. Model is a selector or a name from
// Config.Models for the summary call. Empty means the model of the run,
// which reads the prompt cache of the run.
type CompactionConfig struct {
	At    int    `yaml:"at" json:"at"`
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
}

// Scopes of the memory tool.
const (
	MemoryScopeAgent  = "agent"  // each agent profile has a memory directory of its own
	MemoryScopeShared = "shared" // every agent profile uses one memory directory
)

// MemoryToolConfig configures the memory tool. Scope is MemoryScopeAgent or
// MemoryScopeShared. With MemoryScopeAgent, the notes of the profile
// "reviewer" live under $FW_HOME/memory/agents/reviewer. With
// MemoryScopeShared, the notes of every profile live under
// $FW_HOME/memory/shared.
type MemoryToolConfig struct {
	Scope string `yaml:"scope" json:"scope"`
}

// SpawnConfig bounds the child runs that a run starts through spawn_task.
// MaxDepth is the deepest child allowed: 1 lets a run start children, and 2
// lets those children start children. MaxChildren is the number of child
// runs that one run can start in its life.
type SpawnConfig struct {
	MaxDepth    int `yaml:"maxDepth" json:"maxDepth"`
	MaxChildren int `yaml:"maxChildren" json:"maxChildren"`
}

// MCPServerConfig is one MCP server that fw connects to. Title is the name
// of the service for a person, such as "Tickets", and defaults to Name.
type MCPServerConfig struct {
	Name     string            `yaml:"name" json:"name"`
	Title    string            `yaml:"title,omitempty" json:"title,omitempty"`
	Endpoint string            `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Command  string            `yaml:"command,omitempty" json:"command,omitempty"`
	Args     []string          `yaml:"args,omitempty" json:"args,omitempty"`
	TLS      TLSConfig         `yaml:"tls" json:"tls"`
	Auth     MCPAuthConfig     `yaml:"auth" json:"auth"`
	Headers  map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Timeout  time.Duration     `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Trusted  bool              `yaml:"trusted" json:"trusted"`
}

// TLSConfig configures a client connection.
type TLSConfig struct {
	CA       string `yaml:"ca,omitempty" json:"ca,omitempty"`
	Cert     string `yaml:"cert,omitempty" json:"cert,omitempty"`
	Key      string `yaml:"key,omitempty" json:"key,omitempty"`
	Insecure bool   `yaml:"insecure,omitempty" json:"insecure,omitempty"`
}

// MCPAuthConfig configures client authentication to an MCP server. SPIFFE
// sends a JWT-SVID of this workload as the bearer token.
type MCPAuthConfig struct {
	Bearer string             `yaml:"bearer,omitempty" json:"bearer,omitempty"` // a Secrets name
	OAuth  *OAuthClientConfig `yaml:"oauth,omitempty" json:"oauth,omitempty"`
	SPIFFE *SPIFFEAuthConfig  `yaml:"spiffe,omitempty" json:"spiffe,omitempty"`
}

// SPIFFEAuthConfig asks for a JWT-SVID with that audience on every request.
// An empty audience uses the endpoint URL.
type SPIFFEAuthConfig struct {
	Audience string `yaml:"audience,omitempty" json:"audience,omitempty"`
}

// OAuthClientConfig configures OAuth for one MCP server.
//
// Grant selects whose authority a call carries. GrantClient, the default,
// is the client_credentials grant: the agent acts as itself, and ClientID
// and ClientSecret are required. GrantUser acts for the person who started
// the run, from a grant that the person connects in the workbench
// (docs/ARCHITECTURE.md section 7.11). With GrantUser and no ClientID, the entry uses the client
// of the browser sign-in (auth.oidc), so the person connects once for every
// such entry. With a ClientID, the entry has a client of its own at the
// authorization server that Issuer names, or that the protected resource
// metadata of the MCP server names (RFC 9728) when Issuer is empty.
//
// AuthorizeURL and TokenURL skip discovery. Offline asks for the scope
// offline_access, so the grant outlives the browser session of the person,
// and is true when unset. AuthParams adds parameters to the authorization
// request, such as access_type=offline and prompt=consent for a server
// that has no offline_access scope. Exchange, when set, turns the token of
// the person into a token for the MCP server with RFC 8693 token exchange.
// Without it, every token request carries the resource parameter (RFC
// 8707), which the MCP authorization spec requires.
type OAuthClientConfig struct {
	ClientID     string               `yaml:"clientId,omitempty" json:"clientId,omitempty"`
	ClientSecret string               `yaml:"clientSecret,omitempty" json:"clientSecret,omitempty"` // a Secrets name
	Scopes       []string             `yaml:"scopes,omitempty" json:"scopes,omitempty"`
	TokenURL     string               `yaml:"tokenUrl,omitempty" json:"tokenUrl,omitempty"` // skips discovery when set
	Grant        string               `yaml:"grant,omitempty" json:"grant,omitempty"`       // client (default) or user
	Issuer       string               `yaml:"issuer,omitempty" json:"issuer,omitempty"`
	AuthorizeURL string               `yaml:"authorizeUrl,omitempty" json:"authorizeUrl,omitempty"`
	Offline      *bool                `yaml:"offline,omitempty" json:"offline,omitempty"`
	AuthParams   map[string]string    `yaml:"authParams,omitempty" json:"authParams,omitempty"`
	Exchange     *TokenExchangeConfig `yaml:"exchange,omitempty" json:"exchange,omitempty"`
}

// Grants of an MCP server entry.
const (
	GrantClient = "client" // the agent acts as itself (client_credentials)
	GrantUser   = "user"   // the agent acts for the person who started the run
)

// GrantOf returns the grant of an OAuth entry: GrantClient or GrantUser.
func (c *OAuthClientConfig) GrantOf() string {
	if c != nil && strings.EqualFold(strings.TrimSpace(c.Grant), GrantUser) {
		return GrantUser
	}
	return GrantClient
}

// OfflineOf reports whether the entry asks for offline_access.
func (c *OAuthClientConfig) OfflineOf() bool { return c == nil || c.Offline == nil || *c.Offline }

// TokenExchangeConfig asks the authorization server for a token for the
// MCP server with RFC 8693 token exchange, after each refresh of the grant.
// Audience is the audience parameter, such as the client id of the MCP
// server at the authorization server. Scopes narrow the new token.
type TokenExchangeConfig struct {
	Audience string   `yaml:"audience,omitempty" json:"audience,omitempty"`
	Scopes   []string `yaml:"scopes,omitempty" json:"scopes,omitempty"`
}

// PeerConfig is one A2A peer.
type PeerConfig struct {
	Name string `yaml:"name" json:"name"`
	AX   string `yaml:"ax,omitempty" json:"ax,omitempty"`     // "<atespace>/<task>", reached through the atenet router
	Card string `yaml:"card,omitempty" json:"card,omitempty"` // agent card URL
}

// PolicyConfig is the rules plus an optional external policy. External
// replaces the rules: the decision point decides every tool call, and the
// policy of a profile and of the ancestors of a run can only make that
// decision stricter. A decision point that does not answer denies the call.
type PolicyConfig struct {
	Rules    `yaml:",inline" json:",inline"`
	External *ExternalPolicyConfig `yaml:"external,omitempty" json:"external,omitempty"`
}

// ExternalPolicyConfig routes decisions to a decision point out of process
// (see DecisionPoint). It sets exactly one of MCP and Bus. MCP names an
// entry of tools.mcp, and Tool names its tool: "authorize" for
// access.external and "decide" for policy.external by default. The model
// never sees the tools of that server. Bus is the subject that receives a
// fw.decision.requested event. Timeout bounds one request, 5s by default.
type ExternalPolicyConfig struct {
	MCP     string        `yaml:"mcp,omitempty" json:"mcp,omitempty"` // MCP server name
	Tool    string        `yaml:"tool,omitempty" json:"tool,omitempty"`
	Bus     string        `yaml:"bus,omitempty" json:"bus,omitempty"` // request subject
	Timeout time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// A2AConfig names the agent on its card.
type A2AConfig struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
	URL         string `yaml:"url,omitempty" json:"url,omitempty"` // public base URL for the card
}

// MCPSurfaceConfig configures the MCP server surface.
type MCPSurfaceConfig struct {
	Path    string               `yaml:"path" json:"path"`
	Origins []string             `yaml:"origins,omitempty" json:"origins,omitempty"`
	Expose  []string             `yaml:"expose,omitempty" json:"expose,omitempty"`
	Tasks   bool                 `yaml:"tasks" json:"tasks"`
	TaskTTL time.Duration        `yaml:"taskTTL" json:"taskTTL"`
	Auth    MCPSurfaceAuthConfig `yaml:"auth" json:"auth"`
}

// MCPSurfaceAuthConfig configures the OAuth resource server mode.
type MCPSurfaceAuthConfig struct {
	AuthorizationServers []string `yaml:"authorizationServers,omitempty" json:"authorizationServers,omitempty"`
	Audience             string   `yaml:"audience,omitempty" json:"audience,omitempty"`
}

// AXConfig configures the ax integration.
type AXConfig struct {
	Server string `yaml:"server" json:"server"`
	Router string `yaml:"router" json:"router"`
}

// TelemetryConfig configures metrics and logs.
type TelemetryConfig struct {
	Metrics string `yaml:"metrics" json:"metrics"`
	Log     string `yaml:"log" json:"log"`
}

// Default limits of a tool call and of a run.
const (
	DefaultMaxInputBytes   = 256 << 10
	DefaultMaxOutputBytes  = 100 << 10
	DefaultToolTimeout     = 10 * time.Minute
	DefaultMaxToolCalls    = 1000
	DefaultMaxCallsPerTurn = 32
	DefaultMaxRepeats      = 5
	DefaultSpawnDepth      = 3
	DefaultCompactAt       = 150_000
	DefaultSpawnChildren   = 16
)

// DefaultConfig returns the configuration that works with no file and no
// variables, given a model credential in the environment. The changes of
// RegisterDefaults apply last.
func DefaultConfig() *Config {
	c := &Config{
		Listen:  ":8080",
		Auth:    AuthConfig{Mode: "token", TokenEnv: "FW_API_TOKEN"},
		Access:  AccessConfig{Default: "open"},
		Model:   "anthropic/claude-opus-5-5",
		Effort:  "high",
		Secrets: SecretsConfig{Driver: "env"},
		Ledger:  LedgerConfig{Driver: "file"},
		Bus:     BusConfig{Driver: "mem"},
		Tools: ToolsConfig{
			Builtin: []string{"bash", "files", "http", "memory", "skill", "clock", "ask_user", "spawn_task", "await_task"},
			Files: FilesToolConfig{
				Deny:         append([]string(nil), defaultDeny...),
				ReadOnly:     []string{".git"},
				MaxFileBytes: DefaultMaxFileBytes,
			},
			Memory: MemoryToolConfig{Scope: MemoryScopeAgent},
			Limits: ToolLimits{Limit: Limit{
				MaxInputBytes:  DefaultMaxInputBytes,
				MaxOutputBytes: DefaultMaxOutputBytes,
				Timeout:        DefaultToolTimeout,
			}},
			SearchThreshold: 24,
		},
		Policy: PolicyConfig{Rules: DefaultRules()},
		Budget: Budget{
			MaxTurns:        200,
			MaxTokens:       2_000_000,
			Deadline:        2 * time.Hour,
			MaxToolCalls:    DefaultMaxToolCalls,
			MaxCallsPerTurn: DefaultMaxCallsPerTurn,
			MaxRepeats:      DefaultMaxRepeats,
		},
		Compaction: CompactionConfig{At: DefaultCompactAt},
		Time:       TimeConfig{NoteAfter: DefaultNoteAfter},
		Spawn:      SpawnConfig{MaxDepth: DefaultSpawnDepth, MaxChildren: DefaultSpawnChildren},
		Surfaces:   []string{"native", "a2a", "metrics"},
		A2A:        A2AConfig{Name: "featherweight", Description: "A featherweight agent"},
		MCP:        MCPSurfaceConfig{Path: "/mcp", Tasks: true, TaskTTL: 24 * time.Hour},
		AX: AXConfig{
			Server: "ax-server.ax-system.svc.cluster.local:8080",
			Router: "atenet-router.ate-system.svc.cluster.local",
		},
		Telemetry: TelemetryConfig{Metrics: "/metrics", Log: "info"},
		MaxRuns:   16,
		Agents:    map[string]AgentProfile{},
	}
	registry.mu.RLock()
	fns := slices.Clone(registry.defaults)
	registry.mu.RUnlock()
	for _, fn := range fns {
		fn(c)
	}
	return c
}

// ProfileNamed returns the profile with that name, or the default profile.
// The default profile is "default" when configured, else an empty profile.
func (c *Config) ProfileNamed(name string) AgentProfile {
	if name == "" {
		name = "default"
	}
	if p, ok := c.Agents[name]; ok {
		p.Name = name
		return p
	}
	return AgentProfile{Name: name}
}

// ModelSelector resolves a profile model name through Config.Models.
func (c *Config) ModelSelector(name string) string {
	if name == "" {
		return c.Model
	}
	if s, ok := c.Models[name]; ok {
		return s
	}
	return name
}

// SkillDirs returns the directories that hold skills: Tools.Skills, or the
// skills directory under the home directory.
func (c *Config) SkillDirs() []string {
	if len(c.Tools.Skills) > 0 {
		return c.Tools.Skills
	}
	home := c.Home
	if home == "" {
		home = DefaultHome()
	}
	return []string{filepath.Join(home, "skills")}
}

// Reserved returns the paths that hold the state and the configuration of
// fw: the home directory, the directory of the file ledger, and the
// configuration file. A tool that a model drives never opens a path under
// them, because a change there changes what fw allows at its next start.
func (c *Config) Reserved() []string {
	home := c.Home
	if home == "" {
		home = DefaultHome()
	}
	out := []string{home}
	if c.Ledger.Path != "" {
		out = append(out, c.Ledger.Path)
	}
	if c.ConfigFile != "" {
		out = append(out, c.ConfigFile)
	}
	return out
}

// Efforts lists the effort levels that a profile or a request can name.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// ValidEffort reports whether s is empty or one of Efforts.
func ValidEffort(s string) bool {
	if s == "" {
		return true
	}
	for _, e := range Efforts {
		if e == strings.ToLower(s) {
			return true
		}
	}
	return false
}
