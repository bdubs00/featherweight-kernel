package cli

// This file is fw ax, the developer loop for one ax Task from one profile
// file, without a front (docs/ARCHITECTURE.md section 21). render prints
// the Workspace and the Task, up applies them, resumes the Task, and waits
// until its kernel is ready, status and events read the Task and its
// kernel, and down deletes both. The verbs dial the ax API over gRPC, as
// the spawner of package ax does, and reach the kernel through the atenet
// router.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"
)

// Seams of fw ax. Tests shorten the waits.
var (
	axPoll        = 2 * time.Second // between two reads while up waits and while down deletes
	axDeleteLimit = time.Minute     // how long down tries a delete that Substrate refuses
)

const (
	axConfigFile = ".fw/fw.yaml" // the configuration of the kernel in the Workspace
	axResumes    = 3             // how often up resumes a Task that stops while it waits
	axPageLimit  = 1000          // the events of one page of GET /v1/runs/{id}/events
)

// axUsage is the help of fw ax without a subcommand.
const axUsage = `Usage: fw ax <command> [flags]

Commands:
  render  print the Workspace and the Task of one worker, with a placeholder for each secret
  up      render, apply, resume, and wait until /readyz answers through the router
  status  print the phase and the egress of the Task, and GET /v1/status of its kernel
  events  print the ledger of a run through the router
  down    delete the Task and its Workspace

The verbs dial the ax API at --server, else $AX_SERVER, else ax.server, and
the atenet router at --router, else ax.router. Outside the cluster, forward
the ports first:

  kubectl -n ax-system port-forward svc/ax-server 8080:8080
  kubectl -n ate-system port-forward svc/atenet-router 8001:80
  fw ax up -f agents/reviewer.yaml --name reviewer --image <image@sha256:...> \
      --server 127.0.0.1:8080 --router 127.0.0.1:8001

Run fw ax <command> -h for the flags of a command.
`

// axArgs holds the flags of fw ax.
type axArgs struct {
	file, name, atespace, image string
	egress, env, files          stringList
	debug                       bool
	server, router              string
	timeout                     time.Duration
	from                        uint64
	format                      string
}

func cmdAX(ctx context.Context, g *globals, args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(g.stderr, axUsage)
		if len(args) == 0 {
			return exitUsage
		}
		return exitOK
	}
	sub, rest := args[0], args[1:]
	run := map[string]func(context.Context, *globals, *axArgs, []string) int{
		"render": axRender, "up": axUp, "status": axStatus, "events": axEvents, "down": axDown,
	}[sub]
	if run == nil {
		fmt.Fprintf(g.stderr, "fw ax: unknown command %q\n\n%s", sub, axUsage)
		return exitUsage
	}
	a := &axArgs{}
	fs := g.axFlags(sub, a)
	pos, err := parseArgs(fs, rest)
	if err != nil {
		return flagExit(err)
	}
	if want := map[string]int{"events": 1}[sub]; len(pos) != want || a.name == "" {
		fs.Usage()
		return exitUsage
	}
	return run(ctx, g, a, pos)
}

// axFlags returns the flag set of a subcommand of fw ax.
func (g *globals) axFlags(sub string, a *axArgs) *flag.FlagSet {
	use := map[string]string{
		"render": "-f <profile file> --name <task> --image <image@sha256:...>",
		"up":     "-f <profile file> --name <task> --image <image@sha256:...>",
		"status": "--name <task>", "events": "--name <task> <run-id>", "down": "--name <task>",
	}[sub]
	fs := g.newFlagSet("ax "+sub, use)
	fs.StringVar(&a.name, "name", "", "the `name` of the Task (required)")
	fs.StringVar(&a.atespace, "atespace", "default", "the `atespace` of the Task")
	if sub == "render" || sub == "up" {
		fs.StringVar(&a.file, "f", "", "the profile `file`: a fragment of fw.yaml, such as policy, access, and agents")
		fs.StringVar(&a.image, "image", "", "the `image` of the Task by digest, name@sha256:... (required)")
		fs.Var(&a.egress, "egress", "allow one more `destination`: host:port (http), tls:host:port, https:host:port, or a URL; repeat for more")
		fs.Var(&a.env, "env", "set `NAME=VALUE` in spec.env of the Task; repeat for more")
		fs.Var(&a.files, "workspace-file", "ship the local file at `path=localpath` in the Workspace; repeat for more")
		fs.BoolVar(&a.debug, "debug", false, "set spec.debug, which turns on the guest services of ax ssh; they carry no authentication")
	}
	if sub == "up" || sub == "status" || sub == "down" {
		fs.StringVar(&a.server, "server", "", "the ax API `address` (default: $AX_SERVER, else ax.server)")
	}
	if sub == "up" || sub == "status" || sub == "events" {
		fs.StringVar(&a.router, "router", "", "the atenet router `address` (default: ax.router)")
	}
	switch sub {
	case "up":
		fs.DurationVar(&a.timeout, "timeout", 10*time.Minute, "how long to wait until /readyz answers")
	case "events":
		fs.Uint64Var(&a.from, "from", 1, "the first sequence `number` to print")
		fs.StringVar(&a.format, "format", "jsonl", "the output `format`: jsonl or text")
	}
	return fs
}

// axRendered is one Task and its Workspace as fw ax renders them.
type axRendered struct {
	ws       *v1alpha1.Workspace
	task     *v1alpha1.Task
	cfg      *fw.Config // the configuration of the kernel in the Task
	warnings []string
}

// axSecretLookup returns the value of a secret variable for the Task: the
// value of --env, else the value of the secrets of this process.
func axSecretLookup(ctx context.Context, cfg *fw.Config, a *axArgs) func(string) (string, bool) {
	secrets, err := fw.OpenSecrets(ctx, cfg)
	if err != nil {
		secrets = fw.EnvSecrets{}
	}
	return func(name string) (string, bool) {
		for i := len(a.env) - 1; i >= 0; i-- {
			if n, v, _ := strings.Cut(a.env[i], "="); n == name {
				return v, true
			}
		}
		v, err := secrets.Get(ctx, name)
		return v, err == nil && v != ""
	}
}

func axRender(ctx context.Context, g *globals, a *axArgs, _ []string) int {
	cfg, err := g.loadConfig()
	if err != nil {
		return g.fail("ax render", err)
	}
	r, err := renderAX(cfg, a, axSecretLookup(ctx, cfg, a), true)
	if err != nil {
		return g.fail("ax render", err)
	}
	if err := writeAX(g.stdout, r, a); err != nil {
		return g.fail("ax render", err)
	}
	return exitOK
}

// renderAX renders the Workspace and the Task for a. The configuration of
// the Task is cfg, the configuration of the caller, reduced to the model,
// the providers, the bus, the tokens, and the MCP servers, with the
// profile file of a on top. Each secret variable of the Task takes its
// value from lookup, or a placeholder that names it when placeholders is
// set.
func renderAX(cfg *fw.Config, a *axArgs, lookup func(string) (string, bool), placeholders bool) (*axRendered, error) {
	ws := a.name + "-ws"
	for _, n := range []string{a.name, ws, a.atespace} {
		if err := v1alpha1.ValidateName(n); err != nil {
			return nil, fmt.Errorf("ax: %q is not a name for ax: %w", n, err)
		}
	}
	if !strings.Contains(a.image, "@sha256:") {
		return nil, fmt.Errorf("ax: the image %q has no digest; Substrate pulls an image by digest only, so pass --image name@sha256:<digest>", a.image)
	}
	r := &axRendered{}
	text, tcfg, err := renderAXConfig(cfg, a, r)
	if err != nil {
		return nil, err
	}
	r.cfg = tcfg

	env, err := r.taskEnv(a, lookup, placeholders)
	if err != nil {
		return nil, err
	}
	files := []*v1alpha1.File{{Path: axConfigFile, Content: text}}
	for _, f := range a.files {
		p, local, ok := strings.Cut(f, "=")
		p = strings.TrimPrefix(path.Clean(strings.TrimSpace(p)), "./")
		if !ok || p == "" || p == "." || path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") {
			return nil, fmt.Errorf("ax: --workspace-file %q is not path=localpath with a path inside the workspace", f)
		}
		if p == axConfigFile {
			return nil, fmt.Errorf("ax: --workspace-file %s: fw ax renders %s itself; put its keys in the profile file", p, axConfigFile)
		}
		b, err := os.ReadFile(local)
		if err != nil {
			return nil, fmt.Errorf("ax: --workspace-file %s: %w", p, err)
		}
		files = append(files, &v1alpha1.File{Path: p, Content: string(b)})
	}

	rules, warnings, err := axRules(tcfg, a.egress, os.Getenv)
	if err != nil {
		return nil, err
	}
	r.warnings = append(r.warnings, warnings...)
	egress := &v1alpha1.EgressSpec{}
	for _, rule := range rules {
		egress.Rules = append(egress.Rules, rule.api())
	}

	meta := func(name string) *v1alpha1.ObjectMeta { return &v1alpha1.ObjectMeta{Name: name, Atespace: a.atespace} }
	r.ws = &v1alpha1.Workspace{ApiVersion: ax.APIVersion, Kind: ax.KindWorkspace, Metadata: meta(ws),
		Spec: &v1alpha1.WorkspaceSpec{Files: files}}
	r.task = &v1alpha1.Task{ApiVersion: ax.APIVersion, Kind: ax.KindTask, Metadata: meta(a.name), Spec: &v1alpha1.TaskSpec{
		Image:      a.image,
		Env:        env,
		Workspaces: []*v1alpha1.WorkspaceRef{{Name: ws, Path: ax.DefaultWorkspacePath}},
		Debug:      a.debug,
		Egress:     egress,
	}}
	return r, nil
}

// axCopied is the part of the configuration of the caller that fw ax
// copies into the Task.
type axCopied struct {
	Model     string                `yaml:"model,omitempty"`
	Models    map[string]string     `yaml:"models,omitempty"`
	Effort    string                `yaml:"effort,omitempty"`
	Providers map[string]axProvider `yaml:"providers,omitempty"`
	Bus       *fw.BusConfig         `yaml:"bus,omitempty"`
	Tokens    *fw.TokensConfig      `yaml:"tokens,omitempty"`
	Tools     *axTools              `yaml:"tools,omitempty"`
}

// axProvider is a provider without its headers and its TLS files, which
// name secrets and files of this machine.
type axProvider struct {
	BaseURL   string            `yaml:"baseUrl,omitempty"`
	APIKeyEnv string            `yaml:"apiKeyEnv,omitempty"`
	Timeout   time.Duration     `yaml:"timeout,omitempty"`
	Options   map[string]string `yaml:"options,omitempty"`
}

type axTools struct {
	MCP []fw.MCPServerConfig `yaml:"mcp,omitempty"`
}

// renderAXConfig returns the text of the fw.yaml of the Task and the
// configuration that the kernel in the Task loads from it.
func renderAXConfig(cfg *fw.Config, a *axArgs, r *axRendered) (string, *fw.Config, error) {
	def := fw.DefaultConfig()
	c := axCopied{Model: cfg.Model, Models: cfg.Models}
	if cfg.Effort != def.Effort {
		c.Effort = cfg.Effort
	}
	for name, p := range cfg.Providers {
		if c.Providers == nil {
			c.Providers = map[string]axProvider{}
		}
		c.Providers[name] = axProvider{BaseURL: p.BaseURL, APIKeyEnv: p.APIKeyEnv, Timeout: p.Timeout, Options: p.Options}
		if len(p.Headers) > 0 || p.TLS != (fw.TLSConfig{}) {
			r.warn("providers.%s: the headers and the TLS files stay on this machine; set them in the profile file", name)
		}
	}
	if cfg.Bus.Driver != def.Bus.Driver {
		b := cfg.Bus
		c.Bus = &b
	}
	if cfg.Tokens.URL != "" {
		t := cfg.Tokens
		if t.Subject == def.Tokens.Subject {
			t.Subject = ""
		}
		c.Tokens = &t
	}
	for _, s := range cfg.Tools.MCP {
		if c.Tools == nil {
			c.Tools = &axTools{}
		}
		if len(s.Headers) > 0 || s.TLS != (fw.TLSConfig{}) {
			r.warn("tools.mcp %s: the headers and the TLS files stay on this machine; set them in the profile file", s.Name)
		}
		s.Headers, s.TLS = nil, fw.TLSConfig{}
		c.Tools.MCP = append(c.Tools.MCP, s)
	}
	var doc yaml.Node
	if err := doc.Encode(c); err != nil {
		return "", nil, fmt.Errorf("ax: encode the configuration: %w", err)
	}
	pruneEmpty(&doc)
	escapeValues(&doc)
	doc.Style = 0 // an empty part encodes as {}, and the profile adds to it
	if a.file != "" {
		data, err := os.ReadFile(a.file)
		if err != nil {
			return "", nil, fmt.Errorf("ax: read the profile file: %w", err)
		}
		var prof yaml.Node
		if err := yaml.Unmarshal(data, &prof); err != nil {
			return "", nil, fmt.Errorf("ax: profile file %s: %w", a.file, err)
		}
		if len(prof.Content) > 0 {
			if prof.Content[0].Kind != yaml.MappingNode {
				return "", nil, fmt.Errorf("ax: profile file %s is not a mapping of fw.yaml keys", a.file)
			}
			mergeNode(&doc, prof.Content[0])
		}
	}
	doc.HeadComment = fmt.Sprintf("The configuration of the Task %s/%s, from fw ax render.", a.atespace, a.name)
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", nil, fmt.Errorf("ax: encode the configuration: %w", err)
	}
	_ = enc.Close()

	// The kernel in the Task reads the text over its defaults and refuses
	// a key that it does not know, so a typo stops here, not at the boot.
	tcfg := fw.DefaultConfig()
	dec := yaml.NewDecoder(bytes.NewReader(b.Bytes()))
	dec.KnownFields(true)
	if err := dec.Decode(tcfg); err != nil && !errors.Is(err, io.EOF) {
		return "", nil, fmt.Errorf("ax: the configuration of the Task does not load: %w", err)
	}
	for k := range tcfg.Extensions {
		if !slices.Contains(fw.RegisteredExtensions(), k) {
			return "", nil, fmt.Errorf("ax: the profile file %s has the unknown key %s", a.file, k)
		}
	}
	// fw ax never chooses a model. Without one, every run of the Task
	// fails.
	named := tcfg.Model != "" || len(tcfg.Agents) > 0
	for _, p := range tcfg.Agents {
		named = named && (tcfg.Model != "" || p.Model != "")
	}
	if !named {
		return "", nil, errors.New("ax: the Task names no model; set model or FW_MODEL here, or a model for each profile in the profile file")
	}
	return b.String(), tcfg, nil
}

// pruneEmpty removes the keys whose value is an empty mapping or list.
func pruneEmpty(n *yaml.Node) {
	for _, c := range n.Content {
		pruneEmpty(c)
	}
	if n.Kind != yaml.MappingNode {
		return
	}
	var keep []*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		v := n.Content[i+1]
		if (v.Kind == yaml.MappingNode || v.Kind == yaml.SequenceNode) && len(v.Content) == 0 {
			continue
		}
		keep = append(keep, n.Content[i], v)
	}
	n.Content = keep
}

// escapeValues doubles every $ in the values under n. The kernel expands
// ${NAME} when it loads the file, and $$ gives one $, so a value that this
// process already expanded reaches the Task unchanged.
func escapeValues(n *yaml.Node) {
	switch n.Kind {
	case yaml.ScalarNode:
		n.Value = strings.ReplaceAll(n.Value, "$", "$$")
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			escapeValues(n.Content[i])
		}
	default:
		for _, c := range n.Content {
			escapeValues(c)
		}
	}
}

// mergeNode lays the mapping src over the mapping dst: a key of src
// replaces the same key of dst, and two mappings merge key by key.
func mergeNode(dst, src *yaml.Node) {
	for i := 0; i+1 < len(src.Content); i += 2 {
		k, v := src.Content[i], src.Content[i+1]
		j := 0
		for j < len(dst.Content) && dst.Content[j].Value != k.Value {
			j += 2
		}
		switch {
		case j >= len(dst.Content):
			dst.Content = append(dst.Content, k, v)
		case dst.Content[j+1].Kind == yaml.MappingNode && v.Kind == yaml.MappingNode:
			mergeNode(dst.Content[j+1], v)
		default:
			dst.Content[j+1] = v
		}
	}
}

func (r *axRendered) warn(format string, args ...any) {
	r.warnings = append(r.warnings, fmt.Sprintf(format, args...))
}

// axSecretNames returns the variables of the Task that hold a secret: the
// static token, the key of each model provider, the bearer of each MCP
// server, and the credential for the token broker.
func axSecretNames(c *fw.Config) []string {
	var out []string
	add := func(n string) {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	token := firstNonEmpty(c.Auth.TokenEnv, "FW_API_TOKEN")
	if slices.Contains(c.Auth.Modes(), "token") {
		add(token)
	}
	for _, p := range axProviders(c) {
		add(axProviderKey(c, p))
	}
	for _, s := range c.Tools.MCP {
		add(s.Auth.Bearer)
	}
	if c.Tokens.URL != "" {
		add(firstNonEmpty(c.Tokens.Credential, token))
	}
	return out
}

// axOTLPNames are the variables of the OTLP endpoints, which the Task gets
// from the environment of the caller.
var axOTLPNames = []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"}

// taskEnv returns spec.env of the Task: the secret variables, the OTLP
// endpoints of the caller, and the entries of --env, which win. Without
// placeholders, a secret that lookup does not give is an error.
func (r *axRendered) taskEnv(a *axArgs, lookup func(string) (string, bool), placeholders bool) ([]*v1alpha1.EnvVar, error) {
	var env []*v1alpha1.EnvVar
	set := func(name, value string) {
		for _, e := range env {
			if e.Name == name {
				e.Value = value
				return
			}
		}
		env = append(env, &v1alpha1.EnvVar{Name: name, Value: value})
	}
	for _, kv := range a.env {
		if name, _, ok := strings.Cut(kv, "="); !ok || name == "" || strings.ContainsAny(name, " \t") {
			return nil, fmt.Errorf("ax: --env %q is not NAME=VALUE", kv)
		}
	}
	var missing []string
	for _, n := range axSecretNames(r.cfg) {
		v, ok := lookup(n)
		switch {
		case !ok && n == "GEMINI_API_KEY":
			r.warn("GEMINI_API_KEY is not set here, so the Task gets it only when its atespace has a Gemini credential")
			continue
		case !ok && placeholders:
			r.warn("%s is not set here, so fw ax up stops until you export it or pass --env %s=VALUE", n, n)
		case !ok:
			missing = append(missing, n)
		}
		if placeholders {
			v = axPlaceholder(n)
		}
		set(n, v)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("ax: the Task needs %s, which %s not set here; export %s or pass --env NAME=VALUE",
			strings.Join(missing, ", "), plural(len(missing), "is", "are"), plural(len(missing), "it", "them"))
	}
	for _, n := range axOTLPNames {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			set(n, v)
		}
	}
	for _, kv := range a.env {
		name, value, _ := strings.Cut(kv, "=")
		if placeholders && ax.SecretName(name) {
			value = axPlaceholder(name)
		}
		set(name, value)
	}
	return env, nil
}

// axPlaceholder stands for the value of a secret variable in the output of
// fw ax render.
func axPlaceholder(name string) string { return "<secret: " + name + ">" }

// axEndpoint is one address that the kernel in a Task dials.
type axEndpoint struct {
	what string // what dials it, such as "model openai" or "mcp github"
	url  string // the URL or host:port as configured
}

// Default endpoints of the model adapters, for a provider without a
// baseUrl.
var axProviderURLs = map[string]string{
	"anthropic": "https://api.anthropic.com",
	"gemini":    "https://generativelanguage.googleapis.com",
	"google":    "https://generativelanguage.googleapis.com",
	"openai":    "https://api.openai.com/v1",
}

// axProviders returns the providers that the configuration uses or
// configures with a baseUrl, sorted.
func axProviders(c *fw.Config) []string {
	var out []string
	add := func(p string) {
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	use := func(sel string) {
		if sel != "" {
			p, _ := fw.SplitSelector(c.ModelSelector(sel))
			add(p)
		}
	}
	use(c.Model)
	for _, v := range c.Models {
		use(v)
	}
	for _, p := range c.Agents {
		use(p.Model)
	}
	for name, p := range c.Providers {
		if p.BaseURL != "" {
			add(name)
		}
	}
	sort.Strings(out)
	return out
}

// axProviderKey returns the variable that holds the key of a provider.
func axProviderKey(c *fw.Config, provider string) string {
	if k := c.ProviderNamed(provider).APIKeyEnv; k != "" {
		return k
	}
	return map[string]string{"anthropic": "ANTHROPIC_API_KEY", "gemini": "GEMINI_API_KEY", "google": "GEMINI_API_KEY", "openai": "OPENAI_API_KEY"}[provider]
}

// axEndpoints lists every address that the kernel of c dials: the model
// providers, the bus, the token broker, the OTLP endpoints in getenv, the
// MCP servers, the A2A peers, the OIDC issuer, and the ax API and the
// router when the children run in child Tasks.
func axEndpoints(c *fw.Config, getenv func(string) string) []axEndpoint {
	var out []axEndpoint
	add := func(what, raw string) {
		if raw = strings.TrimSpace(raw); raw != "" {
			out = append(out, axEndpoint{what, raw})
		}
	}
	for _, p := range axProviders(c) {
		add("model "+p, firstNonEmpty(c.ProviderNamed(p).BaseURL, axProviderURLs[p]))
	}
	if c.Bus.Driver != "mem" {
		for _, u := range strings.Split(c.Bus.URL, ",") {
			add("bus", u)
		}
	}
	add("tokens", c.Tokens.URL)
	for _, n := range axOTLPNames {
		add("telemetry", getenv(n))
	}
	for _, s := range c.Tools.MCP {
		add("mcp "+s.Name, s.Endpoint)
	}
	router := false
	for _, p := range c.Tools.Peers {
		add("peer "+p.Name, p.Card)
		router = router || p.AX != ""
	}
	if c.Auth.OIDC != nil && slices.Contains(c.Auth.Modes(), "oidc") {
		add("oidc issuer", c.Auth.OIDC.Issuer)
	}
	if c.Spawn.Driver == "ax" {
		add("ax server", firstNonEmpty(c.AX.Server, ax.DefaultServer))
		router = true
	}
	if router {
		add("ax router", c.AX.Router)
	}
	return out
}

// axAddr is an endpoint split into its parts.
type axAddr struct {
	scheme, host string
	port         int32
}

func (a axAddr) String() string { return net.JoinHostPort(a.host, strconv.Itoa(int(a.port))) }

// kind is the egress rule that the scheme of the address needs: http for
// cleartext HTTP, tls for anything over TLS, or "" when the egress gateway
// cannot carry it.
func (a axAddr) kind() string {
	switch a.scheme {
	case "http", "ws":
		return "http"
	case "https", "wss", "rediss":
		return "tls"
	}
	return ""
}

// parseAXAddr splits a URL, or a host:port that means http, into its parts.
func parseAXAddr(raw string) (axAddr, error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return axAddr{}, fmt.Errorf("%q is not a URL with a host", ax.RedactURL(raw))
	}
	a := axAddr{scheme: strings.ToLower(u.Scheme), host: strings.ToLower(u.Hostname())}
	defaults := map[string]int32{"http": 80, "ws": 80, "https": 443, "wss": 443, "nats": 4222, "tls": 4222, "redis": 6379, "rediss": 6379}
	a.port = defaults[a.scheme]
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return axAddr{}, fmt.Errorf("%q has no valid port", ax.RedactURL(raw))
		}
		a.port = int32(n)
	}
	if a.port == 0 {
		return axAddr{}, fmt.Errorf("%q names no port", ax.RedactURL(raw))
	}
	return a, nil
}

// axRule is one egress rule of a Task: one protocol to one host and port.
type axRule struct {
	kind string // http, tls, or https
	host string
	port int32
}

func (r axRule) String() string {
	return r.kind + " " + net.JoinHostPort(r.host, strconv.Itoa(int(r.port)))
}

func (r axRule) api() *v1alpha1.EgressRule {
	h := &v1alpha1.EgressHosts{Hostnames: []string{r.host}, Ports: []int32{r.port}}
	switch r.kind {
	case "tls":
		return &v1alpha1.EgressRule{Tls: h}
	case "https":
		return &v1alpha1.EgressRule{Https: h}
	}
	return &v1alpha1.EgressRule{Http: h}
}

// axRules derives one egress rule for each endpoint of c and adds the
// extras of --egress. An endpoint that no rule can carry gives a warning.
func axRules(c *fw.Config, extras []string, getenv func(string) string) ([]axRule, []string, error) {
	var rules []axRule
	var warnings []string
	add := func(r axRule) {
		if !slices.Contains(rules, r) {
			rules = append(rules, r)
		}
	}
	for _, e := range axEndpoints(c, getenv) {
		a, err := parseAXAddr(e.url)
		switch {
		case err != nil:
			warnings = append(warnings, fmt.Sprintf("%s: %v, so it gets no egress rule", e.what, err))
		case a.kind() == "":
			warnings = append(warnings, fmt.Sprintf("%s %s: the egress gateway carries only HTTP and TLS, so a Task cannot reach %s://; use ws:// for NATS or rediss:// for Redis", e.what, a, a.scheme))
		case a.host == "localhost" || net.ParseIP(a.host) != nil:
			warnings = append(warnings, fmt.Sprintf("%s %s: an egress rule names a DNS name, and a Task cannot reach this machine; use a name inside the cluster", e.what, a))
		default:
			add(axRule{a.kind(), a.host, a.port})
		}
	}
	for _, x := range extras {
		r, err := parseAXEgress(x)
		if err != nil {
			return nil, nil, err
		}
		add(r)
	}
	return rules, warnings, nil
}

// parseAXEgress reads one --egress value: host:port for http, tls:host:port
// or https:host:port, or a URL, which takes the rule of its scheme.
func parseAXEgress(s string) (axRule, error) {
	bad := fmt.Errorf("ax: --egress %q is not host:port, tls:host:port, https:host:port, or a URL", s)
	if strings.Contains(s, "://") {
		a, err := parseAXAddr(s)
		if err != nil || a.kind() == "" {
			return axRule{}, bad
		}
		return axRule{a.kind(), a.host, a.port}, nil
	}
	r := axRule{kind: "http", port: 80}
	for _, k := range []string{"tls", "https", "http"} {
		if rest, ok := strings.CutPrefix(s, k+":"); ok {
			r.kind, s = k, rest
			if k != "http" {
				r.port = 443
			}
			break
		}
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		host = s
	} else if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return axRule{}, bad
	} else {
		r.port = int32(n)
	}
	if host == "" || strings.ContainsAny(host, "/: ") {
		return axRule{}, bad
	}
	r.host = strings.ToLower(host)
	return r, nil
}

// writeAX prints the Workspace and the Task as one stream of YAML
// documents, in the order of apply, after a comment with the warnings.
func writeAX(w io.Writer, r *axRendered, a *axArgs) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# fw ax render: the Workspace and the Task of %s/%s, in the order of apply.\n", a.atespace, a.name)
	b.WriteString("# A value <secret: NAME> is a placeholder. fw ax up sends the value of NAME from --env or from its own environment.\n")
	for _, warn := range r.warnings {
		fmt.Fprintf(&b, "# Warning: %s\n", warn)
	}
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	for _, doc := range []any{r.ws, r.task} {
		if err := enc.Encode(doc); err != nil {
			return fmt.Errorf("ax: encode: %w", err)
		}
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("ax: encode: %w", err)
	}
	_, err := w.Write(b.Bytes())
	return err
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// axConn holds the connections of a verb: the ax API and the router.
type axConn struct {
	g      *globals
	cfg    *fw.Config
	server string
	router string
	client v1alpha1.AXClient
	conn   *grpc.ClientConn
	http   *http.Client
}

// axConnect resolves the addresses of a verb, dials the ax API when
// server is set, and tells the person which addresses are in use.
func (g *globals) axConnect(cfg *fw.Config, a *axArgs, server, router bool) (*axConn, error) {
	def := fw.DefaultConfig().AX
	origin := func(key, value, standard string) string {
		if value == standard {
			return "the default of " + key + ", inside the cluster"
		}
		return key
	}
	c := &axConn{g: g, cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}}
	if router {
		c.router = a.router
		from := "--router"
		if c.router == "" {
			c.router, from = cfg.AX.Router, origin("ax.router", cfg.AX.Router, def.Router)
		}
		c.router = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(c.router), "http://"), "/")
		if u, err := url.Parse("http://" + c.router); c.router == "" || err != nil || u.Host == "" || u.Path != "" {
			return nil, fmt.Errorf("ax: the router %q is not host:port; pass --router or set ax.router (FW_AX_ROUTER)", c.router)
		}
		fmt.Fprintf(g.stderr, "fw ax: the router at %s (from %s)\n", c.router, from)
	}
	if server {
		c.server = a.server
		from := "--server"
		if c.server == "" {
			c.server, from = os.Getenv("AX_SERVER"), "AX_SERVER"
		}
		if c.server == "" {
			c.server, from = firstNonEmpty(cfg.AX.Server, def.Server), origin("ax.server", firstNonEmpty(cfg.AX.Server, def.Server), def.Server)
		}
		c.server = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(c.server), "http://"), "https://"), "/")
		conn, err := grpc.NewClient(c.server, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, fmt.Errorf("ax: the ax API address %q: %w", c.server, err)
		}
		c.conn, c.client = conn, v1alpha1.NewAXClient(conn)
		fmt.Fprintf(g.stderr, "fw ax: the ax API at %s (from %s)\n", c.server, from)
	}
	return c, nil
}

func (c *axConn) close() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
}

// apiErr explains an error of the ax API, with the fix for an API that
// does not answer.
func (c *axConn) apiErr(what string, err error) error {
	if status.Code(err) == codes.Unavailable || status.Code(err) == codes.DeadlineExceeded {
		return fmt.Errorf("ax: %s: the ax API at %s does not answer (%v). Outside the cluster, run kubectl -n ax-system port-forward svc/ax-server 8080:8080 and pass --server 127.0.0.1:8080", what, c.server, status.Convert(err).Message())
	}
	return fmt.Errorf("ax: %s: %v", what, status.Convert(err).Message())
}

// get sends one request to the kernel of actor through the router, with
// the static token when token is set, and returns the status and the body.
func (c *axConn) get(ctx context.Context, actor, path, token string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+c.router+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("ate-target-actor", actor)
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("ax: the router at %s does not answer: %w", c.router, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, body, err
}

// token returns the static token of the caller for the native API of the
// Task.
func (c *axConn) token(ctx context.Context) (string, string) {
	name := firstNonEmpty(c.cfg.Auth.TokenEnv, "FW_API_TOKEN")
	secrets, err := fw.OpenSecrets(ctx, c.cfg)
	if err != nil {
		secrets = fw.EnvSecrets{}
	}
	v, _ := secrets.Get(ctx, name)
	return name, strings.TrimSpace(v)
}

func axUp(ctx context.Context, g *globals, a *axArgs, _ []string) int {
	cfg, err := g.loadConfig()
	if err != nil {
		return g.fail("ax up", err)
	}
	r, err := renderAX(cfg, a, axSecretLookup(ctx, cfg, a), false)
	if err != nil {
		return g.fail("ax up", err)
	}
	for _, w := range r.warnings {
		fmt.Fprintf(g.stderr, "fw ax: warning: %s\n", w)
	}
	c, err := g.axConnect(cfg, a, true, true)
	if err != nil {
		return g.fail("ax up", err)
	}
	defer c.close()
	actor := a.atespace + "/" + a.name
	// A router that does not answer stops the verb before it changes
	// anything. Any HTTP answer shows the way to the router.
	if _, _, err := c.get(ctx, actor, "/healthz", ""); err != nil {
		return g.fail("ax up", fmt.Errorf("%w. Outside the cluster, run kubectl -n ate-system port-forward svc/atenet-router 8001:80 and pass --router 127.0.0.1:8001", err))
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout+time.Minute)
	defer cancel()
	if _, err := c.client.UpdateWorkspace(ctx, &v1alpha1.UpdateWorkspaceRequest{Workspace: r.ws}); err != nil {
		return g.fail("ax up", c.apiErr("apply the Workspace "+a.atespace+"/"+r.ws.GetMetadata().GetName(), err))
	}
	fmt.Fprintf(g.stderr, "fw ax: applied the Workspace %s/%s\n", a.atespace, r.ws.GetMetadata().GetName())
	if _, err := c.client.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: r.task}); err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return g.fail("ax up", fmt.Errorf("ax: the Task %s exists, and a Task does not change; fw ax down --name %s deletes it", actor, a.name))
		}
		return g.fail("ax up", c.apiErr("create the Task "+actor, err))
	}
	// ax creates every Task suspended until a caller resumes it.
	if _, err := c.client.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: a.atespace, Name: a.name}); err != nil {
		return g.fail("ax up", c.apiErr("resume the Task "+actor, err))
	}
	fmt.Fprintf(g.stderr, "fw ax: created and resumed the Task %s; waiting for /readyz through the router\n", actor)
	if err := c.awaitReady(ctx, a, actor); err != nil {
		return g.fail("ax up", err)
	}
	fmt.Fprintf(g.stdout, "%s is ready at http://%s with the header ate-target-actor: %s\n", actor, c.router, actor)
	return exitOK
}

// awaitReady waits until /readyz of the Task answers 200 through the
// router. A Task that ax reports as suspended or failed, as after a crash
// at boot, is resumed again, up to axResumes times.
func (c *axConn) awaitReady(ctx context.Context, a *axArgs, actor string) error {
	deadline := time.Now().Add(a.timeout)
	resumes, phase, lost := 0, "", 0
	var cond *v1alpha1.Condition
	for {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		code, _, err := c.get(rctx, actor, "/readyz", "")
		cancel()
		switch {
		case err == nil && code == http.StatusOK:
			return nil
		case err != nil && ctx.Err() == nil:
			// The router answers while the Task boots, so three misses in a
			// row mean that the way to the router is gone.
			if lost++; lost == 3 {
				return err
			}
		default:
			lost = 0
		}
		t, err := c.client.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: a.atespace, Name: a.name})
		if err != nil {
			return c.apiErr("read the Task "+actor, err)
		}
		if p := t.GetStatus().GetPhase(); p != phase {
			phase = p
			fmt.Fprintf(c.g.stderr, "fw ax: the Task is %s\n", phase)
		}
		if cs := t.GetStatus().GetConditions(); len(cs) > 0 {
			cond = cs[len(cs)-1]
		}
		if (phase == "Suspended" || phase == "Failed") && resumes < axResumes {
			resumes++
			if _, err := c.client.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: a.atespace, Name: a.name}); err != nil {
				fmt.Fprintf(c.g.stderr, "fw ax: resume the Task: %v\n", status.Convert(err).Message())
			}
		}
		if time.Now().After(deadline) {
			why := ""
			if cond != nil && cond.GetMessage() != "" {
				why = fmt.Sprintf(" (%s: %s)", cond.GetReason(), cond.GetMessage())
			}
			return fmt.Errorf("ax: the Task %s is %s and its /readyz did not answer within %s%s; ax -a %s describe task %s shows why, and fw ax down --name %s deletes it",
				actor, phase, a.timeout, why, a.atespace, a.name, a.name)
		}
		if err := sleep(ctx, axPoll); err != nil {
			return err
		}
	}
}

func axStatus(ctx context.Context, g *globals, a *axArgs, _ []string) int {
	cfg, err := g.loadConfig()
	if err != nil {
		return g.fail("ax status", err)
	}
	c, err := g.axConnect(cfg, a, true, true)
	if err != nil {
		return g.fail("ax status", err)
	}
	defer c.close()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	actor := a.atespace + "/" + a.name
	t, err := c.client.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: a.atespace, Name: a.name})
	if err != nil {
		return g.fail("ax status", c.apiErr("read the Task "+actor, err))
	}
	w := g.stdout
	phase := t.GetStatus().GetPhase()
	fmt.Fprintf(w, "task      %s\nphase     %s\n", actor, phase)
	for _, cd := range t.GetStatus().GetConditions() {
		fmt.Fprintf(w, "condition %s is %s: %s, %s\n", cd.GetType(), cd.GetStatus(), cd.GetReason(), cd.GetMessage())
	}
	label := "egress"
	for _, r := range t.GetSpec().GetEgress().GetRules() {
		for _, x := range []struct {
			kind  string
			hosts *v1alpha1.EgressHosts
		}{{"http", r.GetHttp()}, {"https", r.GetHttps()}, {"tls", r.GetTls()}} {
			if x.hosts == nil {
				continue
			}
			ports := "the default port"
			if len(x.hosts.GetPorts()) > 0 {
				ports = strings.Trim(fmt.Sprint(x.hosts.GetPorts()), "[]")
			}
			fmt.Fprintf(w, "%-9s %s %s on %s\n", label, x.kind, strings.Join(x.hosts.GetHostnames(), ", "), ports)
			label = ""
		}
	}
	if label != "" {
		fmt.Fprintf(w, "egress    none, so the Task has no network\n")
	}
	if phase != "Running" {
		fmt.Fprintf(w, "kernel    not read, because a request through the router wakes a Task that is %s\n", phase)
		return exitOK
	}
	name, token := c.token(ctx)
	if token == "" {
		return g.fail("ax status", fmt.Errorf("ax: %s is not set here, so fw ax status cannot read GET /v1/status of the kernel", name))
	}
	code, body, err := c.get(ctx, actor, "/v1/status", token)
	if err != nil {
		return g.fail("ax status", err)
	}
	var st view.Status
	if code != http.StatusOK || json.Unmarshal(body, &st) != nil {
		return g.fail("ax status", fmt.Errorf("ax: GET /v1/status of %s answered %d: %s", actor, code, problemText(body)))
	}
	idle := "working"
	if st.Idle {
		idle = "idle"
		if st.IdleSince != nil {
			idle += " since " + st.IdleSince.UTC().Format(time.RFC3339)
		}
	}
	fmt.Fprintf(w, "kernel    %s; %d %s, %d active, %d %s for input; %d prompt and %d completion tokens\n",
		idle, st.Runs.Total, plural(st.Runs.Total, "run", "runs"), st.Runs.Active, st.Runs.InputRequired, plural(st.Runs.InputRequired, "waits", "wait"),
		st.Usage.PromptTokens, st.Usage.CompletionTokens)
	if p := st.PendingApproval; p != nil {
		fmt.Fprintf(w, "approval  %s waits since %s: %s\n", p.ID, p.RequestedAt.UTC().Format(time.RFC3339), p.Action)
	}
	return exitOK
}

// problemText returns the detail of a problem body, or the body.
func problemText(body []byte) string {
	var p struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &p) == nil && p.Detail != "" {
		return p.Detail
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func axEvents(ctx context.Context, g *globals, a *axArgs, pos []string) int {
	if a.format != "jsonl" && a.format != "text" {
		return g.fail("ax events", fmt.Errorf("ax: --format %q is not jsonl or text", a.format))
	}
	cfg, err := g.loadConfig()
	if err != nil {
		return g.fail("ax events", err)
	}
	c, err := g.axConnect(cfg, a, false, true)
	if err != nil {
		return g.fail("ax events", err)
	}
	name, token := c.token(ctx)
	if token == "" {
		return g.fail("ax events", fmt.Errorf("ax: %s is not set here, so fw ax events cannot read the ledger", name))
	}
	actor, id := a.atespace+"/"+a.name, pos[0]
	tools := map[string]string{}
	from, n := max(a.from, 1), 0
	for {
		q := url.Values{"from": {strconv.FormatUint(from, 10)}, "limit": {strconv.Itoa(axPageLimit)}}
		code, body, err := c.get(ctx, actor, "/v1/runs/"+url.PathEscape(id)+"/events?"+q.Encode(), token)
		if err != nil {
			return g.fail("ax events", err)
		}
		var page struct {
			Events []fw.Event `json:"events"`
		}
		if code != http.StatusOK || json.Unmarshal(body, &page) != nil {
			return g.fail("ax events", fmt.Errorf("ax: the events of %s on %s answered %d: %s", id, actor, code, problemText(body)))
		}
		for _, e := range page.Events {
			if err := writeEvent(g.stdout, e, a.format, tools); err != nil {
				return g.fail("ax events", err)
			}
			from, n = e.Seq+1, n+1
		}
		if len(page.Events) < axPageLimit {
			break
		}
	}
	if n == 0 {
		fmt.Fprintf(g.stderr, "fw ax events: run %s has no event from %d\n", id, max(a.from, 1))
	}
	return exitOK
}

func axDown(ctx context.Context, g *globals, a *axArgs, _ []string) int {
	cfg, err := g.loadConfig()
	if err != nil {
		return g.fail("ax down", err)
	}
	c, err := g.axConnect(cfg, a, true, false)
	if err != nil {
		return g.fail("ax down", err)
	}
	defer c.close()
	ctx, cancel := context.WithTimeout(ctx, axDeleteLimit+time.Minute)
	defer cancel()
	actor, ws := a.atespace+"/"+a.name, a.name+"-ws"
	// Substrate refuses to delete an actor while a resume or a crash is in
	// progress, so the delete goes on for a while. Then down waits until
	// ax forgets the Task, so the next up can create it again.
	deadline := time.Now().Add(axDeleteLimit)
	deleted, gone := false, false
	for !gone {
		if !deleted {
			_, err = c.client.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Atespace: a.atespace, Name: a.name})
			switch {
			case status.Code(err) == codes.NotFound:
				fmt.Fprintf(g.stderr, "fw ax: the Task %s does not exist\n", actor)
				gone = true
				continue
			case err == nil:
				deleted = true
				fmt.Fprintf(g.stderr, "fw ax: deleting the Task %s\n", actor)
			}
		}
		if deleted {
			_, err = c.client.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: a.atespace, Name: a.name})
			if gone = status.Code(err) == codes.NotFound; gone {
				continue
			}
			if err == nil {
				err = errors.New("ax still lists the Task")
			}
		}
		if time.Now().After(deadline) {
			return g.fail("ax down", c.apiErr("delete the Task "+actor, err))
		}
		if err := sleep(ctx, axPoll); err != nil {
			return g.fail("ax down", err)
		}
	}
	if _, err := c.client.DeleteWorkspace(ctx, &v1alpha1.DeleteWorkspaceRequest{Atespace: a.atespace, Name: ws}); err != nil && status.Code(err) != codes.NotFound {
		return g.fail("ax down", c.apiErr("delete the Workspace "+a.atespace+"/"+ws, err))
	}
	fmt.Fprintf(g.stdout, "deleted the Task %s and the Workspace %s/%s\n", actor, a.atespace, ws)
	return exitOK
}

// sleep waits for d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
