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
	"strconv"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
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

const axUsage = `Usage: fw ax <render|up|status|events|down> [flags]

render prints the Workspace and the Task with a placeholder for each secret.
up applies them, resumes the Task, and waits for /readyz through the router.
status prints the Task and GET /v1/status of its kernel. events prints the
ledger of a run. down deletes the Task and its Workspace. The verbs dial
--server (else $AX_SERVER, else ax.server) and --router (else ax.router);
outside the cluster, forward svc/ax-server and svc/atenet-router first.
`

// axArgs holds the flags of fw ax.
type axArgs struct {
	file, name, atespace, image, server, router, format string
	egress, env, files                                  stringList
	debug                                               bool
	timeout                                             time.Duration
}

func cmdAX(ctx context.Context, g *globals, args []string) int {
	verbs := map[string]func(context.Context, *globals, *axArgs, []string) error{
		"render": axRender, "up": axUp, "status": axStatus, "events": axEvents, "down": axDown,
	}
	if len(args) == 0 || verbs[args[0]] == nil {
		help := len(args) > 0 && slices.Contains([]string{"help", "-h", "--help"}, args[0])
		if len(args) > 0 && !help {
			fmt.Fprintf(g.stderr, "fw ax: unknown command %q\n", args[0])
		}
		fmt.Fprint(g.stderr, axUsage)
		return map[bool]int{true: exitOK, false: exitUsage}[help]
	}
	sub, a := args[0], &axArgs{}
	fs := g.axFlags(sub, a)
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return flagExit(err)
	}
	if want := map[string]int{"events": 1}[sub]; len(pos) != want || a.name == "" {
		fs.Usage()
		return exitUsage
	}
	// up waits for its --timeout, and down tries its delete for a minute.
	ctx, cancel := context.WithTimeout(ctx, a.timeout+axDeleteLimit+time.Minute)
	defer cancel()
	if err := verbs[sub](ctx, g, a, pos); err != nil {
		return g.fail("ax "+sub, err)
	}
	return exitOK
}

// axFlags returns the flag set of a subcommand of fw ax.
func (g *globals) axFlags(sub string, a *axArgs) *flag.FlagSet {
	in := func(subs ...string) bool { return slices.Contains(subs, sub) }
	fs := g.newFlagSet("ax "+sub, map[string]string{"render": "-f <profile file> --name <task> --image <image@sha256:...>",
		"up": "-f <profile file> --name <task> --image <image@sha256:...>", "events": "--name <task> <run-id>"}[sub])
	fs.StringVar(&a.name, "name", "", "the `name` of the Task (required)")
	fs.StringVar(&a.atespace, "atespace", "default", "the `atespace` of the Task")
	if in("render", "up") {
		fs.StringVar(&a.file, "f", "", "the profile `file`: a fragment of fw.yaml, such as policy, access, and agents")
		fs.StringVar(&a.image, "image", "", "the `image` of the Task by digest, name@sha256:... (required)")
		fs.Var(&a.egress, "egress", "allow one more `destination`: host:port (http), tls:host:port, https:host:port, or a URL; repeat for more")
		fs.Var(&a.env, "env", "set `NAME=VALUE` in spec.env of the Task; repeat for more")
		fs.Var(&a.files, "workspace-file", "ship the local file at `path=localpath` in the Workspace; repeat for more")
		fs.BoolVar(&a.debug, "debug", false, "set spec.debug, which turns on the guest services of ax ssh; they carry no authentication")
	}
	if in("up", "status", "down") {
		fs.StringVar(&a.server, "server", "", "the ax API `address` (default: $AX_SERVER, else ax.server)")
	}
	if in("up", "status", "events") {
		fs.StringVar(&a.router, "router", "", "the atenet router `address` (default: ax.router)")
	}
	if sub == "up" {
		fs.DurationVar(&a.timeout, "timeout", 10*time.Minute, "how long to wait until /readyz answers")
	}
	if sub == "events" {
		fs.StringVar(&a.format, "format", "jsonl", "the output `format`: jsonl or text")
	}
	return fs
}

// axRendered is one Task and its Workspace as fw ax renders them.
type axRendered struct {
	ws       *v1alpha1.Workspace
	task     *v1alpha1.Task
	warnings []string
}

func (r *axRendered) warn(format string, args ...any) {
	r.warnings = append(r.warnings, fmt.Sprintf(format, args...))
}

// axSecrets returns the value of a secret variable for the Task: the value
// of --env, else the value of the secrets of the caller.
func axSecrets(ctx context.Context, cfg *fw.Config, a *axArgs) func(string) (string, bool) {
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
		return strings.TrimSpace(v), err == nil && v != ""
	}
}

func axRender(ctx context.Context, g *globals, a *axArgs, _ []string) error {
	cfg, err := g.loadConfig()
	if err != nil {
		return err
	}
	r, err := renderAX(cfg, a, axSecrets(ctx, cfg, a), true)
	if err != nil {
		return err
	}
	text, err := yamlText(r.ws, r.task)
	fmt.Fprintf(g.stdout, "# fw ax render: the Workspace and the Task of %s/%s, in the order of apply.\n", a.atespace, a.name)
	fmt.Fprint(g.stdout, "# A value <secret: NAME> is a placeholder. fw ax up sends the value of NAME from --env or from its own environment.\n")
	for _, w := range r.warnings {
		fmt.Fprintf(g.stdout, "# Warning: %s\n", w)
	}
	fmt.Fprint(g.stdout, text)
	return err
}

// yamlText renders documents as one YAML stream with an indent of two.
func yamlText(docs ...any) (string, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	for _, d := range docs {
		if err := enc.Encode(d); err != nil {
			return "", fmt.Errorf("ax: encode: %w", err)
		}
	}
	return b.String(), enc.Close()
}

// renderAX renders the Workspace and the Task for a. Each secret variable
// of the Task takes its value from lookup, or a placeholder that names it
// when placeholders is set.
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
	text, tcfg, err := axConfig(cfg, a, r)
	if err != nil {
		return nil, err
	}
	env, err := axTaskEnv(tcfg, a, lookup, placeholders, r)
	if err != nil {
		return nil, err
	}
	files := []*v1alpha1.File{{Path: ".fw/fw.yaml", Content: text}}
	for _, f := range a.files {
		p, local, ok := strings.Cut(f, "=")
		p = path.Clean(strings.TrimSpace(p))
		if !ok || p == "." || p == ".fw/fw.yaml" || path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") {
			return nil, fmt.Errorf("ax: --workspace-file %q is not path=localpath with a path inside the workspace other than .fw/fw.yaml, which fw ax renders", f)
		}
		b, err := os.ReadFile(local)
		if err != nil {
			return nil, fmt.Errorf("ax: --workspace-file %s: %w", p, err)
		}
		files = append(files, &v1alpha1.File{Path: p, Content: string(b)})
	}
	rules, err := axRules(tcfg, a.egress, os.Getenv, r)
	if err != nil {
		return nil, err
	}
	egress := &v1alpha1.EgressSpec{}
	for _, rule := range rules {
		h := &v1alpha1.EgressHosts{Hostnames: []string{rule.host}, Ports: []int32{rule.port}}
		egress.Rules = append(egress.Rules, map[string]*v1alpha1.EgressRule{"http": {Http: h}, "tls": {Tls: h}, "https": {Https: h}}[rule.kind])
	}
	meta := func(name string) *v1alpha1.ObjectMeta { return &v1alpha1.ObjectMeta{Name: name, Atespace: a.atespace} }
	r.ws = &v1alpha1.Workspace{ApiVersion: ax.APIVersion, Kind: ax.KindWorkspace, Metadata: meta(ws), Spec: &v1alpha1.WorkspaceSpec{Files: files}}
	r.task = &v1alpha1.Task{ApiVersion: ax.APIVersion, Kind: ax.KindTask, Metadata: meta(a.name), Spec: &v1alpha1.TaskSpec{
		Image: a.image, Env: env, Debug: a.debug, Egress: egress,
		Workspaces: []*v1alpha1.WorkspaceRef{{Name: ws, Path: ax.DefaultWorkspacePath}},
	}}
	return r, nil
}

// axCopied is the part of the configuration of the caller that fw ax
// copies into the Task. A provider and an MCP server leave their headers
// behind, because a header can hold a secret.
type axCopied struct {
	Model     string                       `yaml:"model,omitempty"`
	Models    map[string]string            `yaml:"models,omitempty"`
	Effort    string                       `yaml:"effort,omitempty"`
	Providers map[string]fw.ProviderConfig `yaml:"providers,omitempty"`
	Bus       *fw.BusConfig                `yaml:"bus,omitempty"`
	Tokens    *fw.TokensConfig             `yaml:"tokens,omitempty"`
	Tools     struct {
		MCP []fw.MCPServerConfig `yaml:"mcp,omitempty"`
	} `yaml:"tools,omitempty"`
}

// axConfig returns the text of the fw.yaml of the Task, the profile file
// over the copied part, and the configuration that the kernel in the Task
// loads from it.
func axConfig(cfg *fw.Config, a *axArgs, r *axRendered) (string, *fw.Config, error) {
	// The configuration of the caller is expanded, so a copied value can
	// hold a secret in the open: a password in a URL, or a token in the
	// options of the bus. Such a value never goes into a Task.
	urls := map[string]string{"bus.url": cfg.Bus.URL, "tokens.url": cfg.Tokens.URL}
	for name, p := range cfg.Providers {
		urls["providers."+name+".baseUrl"] = p.BaseURL
	}
	for _, m := range cfg.Tools.MCP {
		urls["tools.mcp "+m.Name+" endpoint"] = m.Endpoint
	}
	for key, v := range urls {
		if credentialURL(v) {
			return "", nil, fmt.Errorf("ax: %s holds a credential in its URL; write it as ${NAME} in the profile file and pass --env NAME=<value>, which the Task gets as a secret", key)
		}
	}
	for key := range cfg.Bus.Options {
		if secretOption(key) {
			return "", nil, fmt.Errorf("ax: bus.options.%s can hold a credential; set it in the profile file as ${NAME} and pass --env NAME=<value>", key)
		}
	}
	c := axCopied{Model: cfg.Model, Models: cfg.Models, Effort: cfg.Effort, Providers: map[string]fw.ProviderConfig{}}
	if cfg.Bus.Driver != "mem" {
		c.Bus = &cfg.Bus
	}
	if cfg.Tokens.URL != "" {
		c.Tokens = &cfg.Tokens
	}
	headers := func(key string, h map[string]string) {
		if len(h) > 0 {
			r.warn("%s: the headers stay on this machine, because a header can hold a secret; set them in the profile file", key)
		}
	}
	for name, p := range cfg.Providers {
		headers("providers."+name, p.Headers)
		p.Headers = nil
		c.Providers[name] = p
	}
	for _, s := range cfg.Tools.MCP {
		headers("tools.mcp "+s.Name, s.Headers)
		s.Headers = nil
		c.Tools.MCP = append(c.Tools.MCP, s)
	}
	var doc, prof yaml.Node
	if err := doc.Encode(c); err != nil {
		return "", nil, fmt.Errorf("ax: encode the configuration: %w", err)
	}
	tidy(&doc)
	doc.Style = 0 // an empty part encodes as {}, and the profile file adds to it
	if a.file != "" {
		data, err := os.ReadFile(a.file)
		if err == nil {
			err = yaml.Unmarshal(data, &prof)
		}
		if err == nil && len(prof.Content) > 0 && prof.Content[0].Kind != yaml.MappingNode {
			err = errors.New("it is not a mapping of fw.yaml keys")
		}
		if err != nil {
			return "", nil, fmt.Errorf("ax: profile file %s: %w", a.file, err)
		}
		if len(prof.Content) > 0 {
			mergeNode(&doc, prof.Content[0])
		}
	}
	doc.HeadComment = fmt.Sprintf("The configuration of the Task %s/%s, from fw ax render.", a.atespace, a.name)
	text, err := yamlText(&doc)
	if err != nil {
		return "", nil, err
	}
	// The kernel in the Task reads the text over its defaults and refuses
	// a key that it does not know, so a typo stops here, not at the boot.
	tcfg := fw.DefaultConfig()
	dec := yaml.NewDecoder(strings.NewReader(text))
	dec.KnownFields(true)
	if err := dec.Decode(tcfg); err != nil && !errors.Is(err, io.EOF) {
		return "", nil, fmt.Errorf("ax: the configuration of the Task does not load: %w", err)
	}
	for k := range tcfg.Extensions {
		if !slices.Contains(fw.RegisteredExtensions(), k) {
			return "", nil, fmt.Errorf("ax: the profile file %s has the unknown key %s", a.file, k)
		}
	}
	// fw ax never chooses a model. Without one, every run of the Task fails.
	named := tcfg.Model != ""
	for _, p := range tcfg.Agents {
		named = named || p.Model != ""
	}
	if !named {
		return "", nil, errors.New("ax: the Task names no model; set model or FW_MODEL here, or the model of a profile in the profile file")
	}
	return text, tcfg, nil
}

// tidy drops the keys whose value is an empty mapping or list, and doubles
// every $ of a value. The kernel expands ${NAME} when it loads the file,
// and $$ gives one $, so a value that this process expanded stays as it is.
func tidy(n *yaml.Node) {
	if n.Kind == yaml.ScalarNode {
		n.Value = strings.ReplaceAll(n.Value, "$", "$$")
	}
	keep := n.Content[:0]
	for i := 0; i < len(n.Content); i++ {
		if n.Kind != yaml.MappingNode {
			tidy(n.Content[i])
			keep = append(keep, n.Content[i])
			continue
		}
		k, v := n.Content[i], n.Content[i+1]
		i++
		if tidy(v); v.Kind == yaml.ScalarNode || len(v.Content) > 0 {
			keep = append(keep, k, v)
		}
	}
	n.Content = keep
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

// axOTLPNames are the variables of the OTLP endpoints, which the Task gets
// from the environment of the caller.
var axOTLPNames = []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"}

// axTaskEnv returns spec.env of the Task: the static token, the key of each
// model provider, the bearer of each MCP server, and the credential for
// the token broker, then the OTLP endpoints of the caller, then --env,
// which wins. Without placeholders, a secret that lookup does not give is
// an error.
func axTaskEnv(c *fw.Config, a *axArgs, lookup func(string) (string, bool), placeholders bool, r *axRendered) ([]*v1alpha1.EnvVar, error) {
	var env []*v1alpha1.EnvVar
	set := func(name, value string) {
		if i := slices.IndexFunc(env, func(e *v1alpha1.EnvVar) bool { return e.Name == name }); i >= 0 {
			env[i].Value = value
		} else {
			env = append(env, &v1alpha1.EnvVar{Name: name, Value: value})
		}
	}
	token := firstNonEmpty(c.Auth.TokenEnv, "FW_API_TOKEN")
	var names, missing []string
	if slices.Contains(c.Auth.Modes(), "token") {
		names = append(names, token)
	}
	for _, p := range axProviders(c) {
		names = append(names, firstNonEmpty(c.ProviderNamed(p).APIKeyEnv, axProviderDefaults[p][1]))
	}
	for _, s := range c.Tools.MCP {
		names = append(names, s.Auth.Bearer)
	}
	if c.Tokens.URL != "" {
		names = append(names, firstNonEmpty(c.Tokens.Credential, token))
	}
	for i, n := range names {
		if n == "" || slices.Contains(names[:i], n) {
			continue
		}
		v, ok := lookup(n)
		if !ok && placeholders {
			r.warn("%s is not set here, so fw ax up stops until you export it or pass --env %s=VALUE", n, n)
		} else if !ok {
			missing = append(missing, n)
		}
		if placeholders {
			v = "<secret: " + n + ">"
		}
		set(n, v)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("ax: the Task needs %s, not set here; export each or pass --env NAME=VALUE", strings.Join(missing, ", "))
	}
	for _, n := range axOTLPNames {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			set(n, v)
		}
	}
	for _, kv := range a.env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			return nil, fmt.Errorf("ax: --env %q is not NAME=VALUE", kv)
		}
		if placeholders && ax.SecretName(name) {
			value = "<secret: " + name + ">"
		}
		set(name, value)
	}
	return env, nil
}

// axProviderDefaults holds the endpoint and the key variable of each model
// adapter, for a provider without a baseUrl or an apiKeyEnv.
var axProviderDefaults = map[string][2]string{
	"anthropic": {"https://api.anthropic.com", "ANTHROPIC_API_KEY"},
	"gemini":    {"https://generativelanguage.googleapis.com", "GEMINI_API_KEY"},
	"openai":    {"https://api.openai.com/v1", "OPENAI_API_KEY"},
}

// axProviders returns the providers that the configuration uses or
// configures with a baseUrl, sorted.
func axProviders(c *fw.Config) []string {
	var out []string
	add := func(sel string) {
		if p, _ := fw.SplitSelector(c.ModelSelector(sel)); sel != "" && p != "" {
			out = append(out, p)
		}
	}
	add(c.Model)
	for _, v := range c.Models {
		add(v)
	}
	for _, p := range c.Agents {
		add(p.Model)
	}
	for name, p := range c.Providers {
		if p.BaseURL != "" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// axEndpoint is one address that the kernel in a Task dials.
type axEndpoint struct {
	what string // what dials it, such as "model openai" or "mcp github"
	url  string // the URL or host:port as configured
}

// axEndpoints lists the addresses that the kernel of c dials: the model
// providers, the bus, the token broker, the OTLP endpoints in getenv, and
// the MCP servers.
func axEndpoints(c *fw.Config, getenv func(string) string) []axEndpoint {
	var out []axEndpoint
	add := func(what, raw string) {
		if raw = strings.TrimSpace(raw); raw != "" {
			out = append(out, axEndpoint{what, raw})
		}
	}
	for _, p := range axProviders(c) {
		add("model "+p, firstNonEmpty(c.ProviderNamed(p).BaseURL, axProviderDefaults[p][0]))
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
	return out
}

// axRule is one egress rule: one protocol to one host and port. kind is
// http, tls, or https, or "" for a scheme that the egress gateway cannot
// carry, because it carries only HTTP and TLS.
type axRule struct {
	kind, host string
	port       int32
}

func (r axRule) addr() string { return net.JoinHostPort(r.host, strconv.Itoa(int(r.port))) }

// parseAXURL reads an endpoint, a URL or a host:port that means http, as
// the rule that it needs: http for http and ws, tls for https, wss, and
// rediss.
func parseAXURL(raw string) (axRule, error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return axRule{}, fmt.Errorf("%q is not a URL with a host", ax.RedactURL(raw))
	}
	s := strings.ToLower(u.Scheme)
	r := axRule{kind: map[string]string{"http": "http", "ws": "http", "https": "tls", "wss": "tls", "rediss": "tls"}[s], host: strings.ToLower(u.Hostname())}
	r.port = map[string]int32{"http": 80, "ws": 80, "https": 443, "wss": 443, "nats": 4222, "redis": 6379, "rediss": 6379}[s]
	if n, err := strconv.ParseUint(u.Port(), 10, 16); err == nil && n > 0 {
		r.port = int32(n)
	} else if u.Port() != "" || r.port == 0 {
		return axRule{}, fmt.Errorf("%q names no valid port", ax.RedactURL(raw))
	}
	return r, nil
}

// axRules derives one egress rule for each endpoint of c and adds the
// extras of --egress. An endpoint that no rule can carry gives a warning.
func axRules(c *fw.Config, extras []string, getenv func(string) string, w *axRendered) ([]axRule, error) {
	var rules []axRule
	for _, e := range axEndpoints(c, getenv) {
		r, err := parseAXURL(e.url)
		switch {
		case err != nil:
			w.warn("%s: %v, so it gets no egress rule", e.what, err)
		case r.kind == "":
			w.warn("%s %s: the egress gateway carries only HTTP and TLS, so a Task cannot reach it; use ws:// for NATS or rediss:// for Redis", e.what, r.addr())
		case r.host == "localhost" || net.ParseIP(r.host) != nil:
			w.warn("%s %s: an egress rule names a DNS name, and a Task cannot reach this machine; use a name inside the cluster", e.what, r.addr())
		case !slices.Contains(rules, r):
			rules = append(rules, r)
		}
	}
	for _, s := range extras {
		// host:port is http, a URL takes the rule of its scheme, and
		// tls:host:port and https:host:port name theirs, port 443 by default.
		r, err := parseAXURL(s)
		if k, rest, _ := strings.Cut(s, ":"); (k == "tls" || k == "https") && !strings.HasPrefix(rest, "//") {
			r, err = parseAXURL("https://" + rest)
			r.kind = k
		}
		if err != nil || r.kind == "" || (!strings.Contains(s, "://") && strings.Contains(s, "/")) {
			return nil, fmt.Errorf("ax: --egress %q is not host:port, tls:host:port, https:host:port, or a URL", s)
		}
		if !slices.Contains(rules, r) {
			rules = append(rules, r)
		}
	}
	return rules, nil
}

// axConn holds the connections of a verb: the ax API and the router.
type axConn struct {
	g              *globals
	cfg            *fw.Config
	server, router string
	client         v1alpha1.AXClient
	http           *http.Client
}

// axOpen loads the configuration of the caller, resolves the addresses of
// a verb, dials the ax API when server is set, and tells the person which
// addresses are in use. release closes the connection to the ax API.
func (g *globals) axOpen(a *axArgs, server, router bool) (c *axConn, release func(), err error) {
	cfg, err := g.loadConfig()
	if err != nil {
		return nil, nil, err
	}
	trim := func(s string) string {
		return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "http://"), "/")
	}
	c, release = &axConn{g: g, cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}}, func() {}
	if router {
		c.router = trim(firstNonEmpty(a.router, cfg.AX.Router))
		if u, err := url.Parse("http://" + c.router); c.router == "" || err != nil || u.Host == "" || u.Path != "" {
			return nil, nil, fmt.Errorf("ax: the router %q is not host:port; pass --router or set ax.router (FW_AX_ROUTER)", c.router)
		}
		fmt.Fprintf(g.stderr, "fw ax: the router at %s\n", c.router)
	}
	if server {
		c.server = trim(firstNonEmpty(a.server, os.Getenv("AX_SERVER"), cfg.AX.Server))
		conn, err := grpc.NewClient(c.server, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, nil, fmt.Errorf("ax: the ax API address %q: %w", c.server, err)
		}
		c.client, release = v1alpha1.NewAXClient(conn), func() { _ = conn.Close() }
		fmt.Fprintf(g.stderr, "fw ax: the ax API at %s\n", c.server)
	}
	return c, release, nil
}

// apiErr explains an error of the ax API, with the fix for an API that
// does not answer.
func (c *axConn) apiErr(what string, err error) error {
	msg := status.Convert(err).Message()
	if code := status.Code(err); code == codes.Unavailable || code == codes.DeadlineExceeded {
		return fmt.Errorf("ax: %s: the ax API at %s does not answer (%s). Outside the cluster, run kubectl -n ax-system port-forward svc/ax-server 8080:8080 and pass --server 127.0.0.1:8080", what, c.server, msg)
	}
	return fmt.Errorf("ax: %s: %s", what, msg)
}

// get sends one request to the kernel of actor through the router, with
// the static token of the caller when auth is set, and returns the status
// and the body. With auth, an answer other than 200 is an error.
func (c *axConn) get(ctx context.Context, actor, path string, auth bool) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+c.router+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("ate-target-actor", actor)
	req.Header.Set("Accept", "application/json")
	if name := firstNonEmpty(c.cfg.Auth.TokenEnv, "FW_API_TOKEN"); auth {
		token, ok := axSecrets(ctx, c.cfg, &axArgs{})(name)
		if !ok {
			return 0, nil, fmt.Errorf("ax: %s is not set here, so fw ax cannot reach the native API of the Task", name)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("ax: the router at %s does not answer: %w", c.router, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err == nil && auth && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("ax: GET %s of %s answered %d: %s", strings.SplitN(path, "?", 2)[0], actor, resp.StatusCode, bytes.TrimSpace(body))
	}
	return resp.StatusCode, body, err
}

func axUp(ctx context.Context, g *globals, a *axArgs, _ []string) error {
	c, release, err := g.axOpen(a, true, true)
	if err != nil {
		return err
	}
	defer release()
	r, err := renderAX(c.cfg, a, axSecrets(ctx, c.cfg, a), false)
	if err != nil {
		return err
	}
	for _, w := range r.warnings {
		fmt.Fprintf(g.stderr, "fw ax: warning: %s\n", w)
	}
	actor, ws := a.atespace+"/"+a.name, a.atespace+"/"+r.ws.GetMetadata().GetName()
	// A router that does not answer stops the verb before it changes
	// anything. Any HTTP answer shows the way to the router.
	if _, _, err := c.get(ctx, actor, "/healthz", false); err != nil {
		return fmt.Errorf("%w. Outside the cluster, run kubectl -n ate-system port-forward svc/atenet-router 8001:80 and pass --router 127.0.0.1:8001", err)
	}
	if _, err := c.client.UpdateWorkspace(ctx, &v1alpha1.UpdateWorkspaceRequest{Workspace: r.ws}); err != nil {
		return c.apiErr("apply the Workspace "+ws, err)
	}
	fmt.Fprintf(g.stderr, "fw ax: applied the Workspace %s\n", ws)
	if _, err := c.client.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: r.task}); status.Code(err) == codes.AlreadyExists {
		return fmt.Errorf("ax: the Task %s exists, and a Task does not change; fw ax down --name %s deletes it", actor, a.name)
	} else if err != nil {
		return c.apiErr("create the Task "+actor, err)
	}
	// ax creates every Task suspended until a caller resumes it.
	if _, err := c.client.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: a.atespace, Name: a.name}); err != nil {
		return c.apiErr("resume the Task "+actor, err)
	}
	fmt.Fprintf(g.stderr, "fw ax: created and resumed the Task %s; waiting for /readyz through the router\n", actor)
	if err := c.awaitReady(ctx, a, actor); err != nil {
		return err
	}
	fmt.Fprintf(g.stdout, "%s is ready at http://%s with the header ate-target-actor: %s\n", actor, c.router, actor)
	return nil
}

// awaitReady waits until /readyz of the Task answers 200 through the
// router. A Task that ax reports as suspended or failed, as after a crash
// at boot, is resumed again, up to three times. The router answers while
// the Task boots, so three misses in a row mean that its way is gone.
func (c *axConn) awaitReady(ctx context.Context, a *axArgs, actor string) error {
	deadline := time.Now().Add(a.timeout)
	resumes, lost, phase := 0, 0, ""
	for {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		code, _, err := c.get(rctx, actor, "/readyz", false)
		cancel()
		switch {
		case err == nil && code == http.StatusOK:
			return nil
		case err == nil || ctx.Err() != nil:
			lost = 0
		case lost == 2:
			return err
		default:
			lost++
		}
		t, err := c.client.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: a.atespace, Name: a.name})
		if err != nil {
			return c.apiErr("read the Task "+actor, err)
		}
		if p := t.GetStatus().GetPhase(); p != phase {
			phase = p
			fmt.Fprintf(c.g.stderr, "fw ax: the Task is %s\n", phase)
		}
		if (phase == "Suspended" || phase == "Failed") && resumes < 3 {
			resumes++
			if _, err := c.client.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: a.atespace, Name: a.name}); err != nil {
				fmt.Fprintf(c.g.stderr, "fw ax: resume the Task: %s\n", status.Convert(err).Message())
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ax: the Task %s is %s and its /readyz did not answer within %s; ax -a %s describe task %s shows why, and fw ax down --name %s deletes it",
				actor, phase, a.timeout, a.atespace, a.name, a.name)
		}
		if err := sleep(ctx, axPoll); err != nil {
			return err
		}
	}
}

// axStatus prints the phase and the conditions of the Task and, while it
// runs, GET /v1/status of its kernel as JSON. A read through the router
// wakes a Task that sleeps, so it reads no kernel of such a Task.
func axStatus(ctx context.Context, g *globals, a *axArgs, _ []string) error {
	c, release, err := g.axOpen(a, true, true)
	if err != nil {
		return err
	}
	defer release()
	actor := a.atespace + "/" + a.name
	t, err := c.client.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: a.atespace, Name: a.name})
	if err != nil {
		return c.apiErr("read the Task "+actor, err)
	}
	phase := t.GetStatus().GetPhase()
	fmt.Fprintf(g.stdout, "task      %s\nphase     %s\n", actor, phase)
	for _, cd := range t.GetStatus().GetConditions() {
		fmt.Fprintf(g.stdout, "condition %s is %s: %s, %s\n", cd.GetType(), cd.GetStatus(), cd.GetReason(), cd.GetMessage())
	}
	if phase != "Running" {
		fmt.Fprintf(g.stdout, "kernel    not read, because a request through the router wakes a Task that is %s\n", phase)
		return nil
	}
	_, body, err := c.get(ctx, actor, "/v1/status", true)
	if err == nil {
		fmt.Fprintf(g.stdout, "kernel    %s\n", bytes.TrimSpace(body))
	}
	return err
}

// axEvents prints the ledger of a run through the router, a page of 1,000
// events at a time, in the format of fw events.
func axEvents(ctx context.Context, g *globals, a *axArgs, pos []string) error {
	if a.format != "jsonl" && a.format != "text" {
		return fmt.Errorf("ax: --format %q is not jsonl or text", a.format)
	}
	c, _, err := g.axOpen(a, false, true)
	if err != nil {
		return err
	}
	tools := map[string]string{}
	for from := uint64(1); ; {
		var page struct {
			Events []fw.Event `json:"events"`
		}
		_, body, err := c.get(ctx, a.atespace+"/"+a.name, fmt.Sprintf("/v1/runs/%s/events?from=%d&limit=1000", url.PathEscape(pos[0]), from), true)
		if err == nil {
			err = json.Unmarshal(body, &page)
		}
		for i := 0; err == nil && i < len(page.Events); i++ {
			err, from = writeEvent(g.stdout, page.Events[i], a.format, tools), page.Events[i].Seq+1
		}
		if err != nil || len(page.Events) < 1000 {
			return err
		}
	}
}

// axDown deletes the Task, then its Workspace. Substrate refuses to delete
// an actor while a resume or a crash is in progress, so the delete goes on
// for a while. Then down waits until ax forgets the Task, so the next up
// can create it again.
func axDown(ctx context.Context, g *globals, a *axArgs, _ []string) error {
	c, release, err := g.axOpen(a, true, false)
	if err != nil {
		return err
	}
	defer release()
	actor, ws := a.atespace+"/"+a.name, a.name+"-ws"
	deadline := time.Now().Add(axDeleteLimit)
	for deleted := false; ; {
		if !deleted {
			_, err = c.client.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Atespace: a.atespace, Name: a.name})
			if deleted = err == nil; deleted {
				fmt.Fprintf(g.stderr, "fw ax: deleting the Task %s\n", actor)
			} else if status.Code(err) == codes.NotFound {
				fmt.Fprintf(g.stderr, "fw ax: the Task %s does not exist\n", actor)
				break
			}
		}
		if deleted {
			if _, err = c.client.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: a.atespace, Name: a.name}); status.Code(err) == codes.NotFound {
				break
			} else if err == nil {
				err = errors.New("ax still lists the Task")
			}
		}
		if time.Now().After(deadline) {
			return c.apiErr("delete the Task "+actor, err)
		}
		if err := sleep(ctx, axPoll); err != nil {
			return err
		}
	}
	if _, err := c.client.DeleteWorkspace(ctx, &v1alpha1.DeleteWorkspaceRequest{Atespace: a.atespace, Name: ws}); err != nil && status.Code(err) != codes.NotFound {
		return c.apiErr("delete the Workspace "+a.atespace+"/"+ws, err)
	}
	fmt.Fprintf(g.stdout, "deleted the Task %s and the Workspace %s/%s\n", actor, a.atespace, ws)
	return nil
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

// credentialURL reports whether s is a URL with a user part, which can
// hold a password.
func credentialURL(s string) bool {
	if s == "" {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.User != nil
}

// secretOption reports whether an option key names a credential.
func secretOption(key string) bool {
	k := strings.ToLower(key)
	for _, w := range []string{"token", "password", "credential", "secret", "key"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}
