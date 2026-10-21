// Package config resolves the configuration of one fw process. It starts
// from fw.DefaultConfig, overlays the file fw.yaml, overlays the FW_*
// environment variables, reads the agent profiles, and validates the result.
// See docs/ARCHITECTURE.md section 12.1 and examples/fw.yaml.
//
// A value in fw.yaml and in a profile file expands ${NAME} and
// ${NAME:-default} from the environment, so a file names an endpoint by its
// variable. A map and a list of named entries take variables too, one per
// key or per field of an entry: FW_MODELS_FAST sets models.fast and
// FW_TOOLS_MCP_GIT_ENDPOINT sets the endpoint of the tools.mcp entry named
// git. EnvNames lists every name and pattern.
//
// Importing the package sets fw.ConfigLoader, so fw.WithConfigFile works.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

func init() {
	fw.ConfigLoader = Load
	fw.ConfigFinder = Find
}

// Load returns the configuration: the defaults, then the file at path when
// path is not empty, then the FW_* variables. Then it reads the profiles and
// validates the result. A key in the file that the schema does not know is
// an error that names the key.
func Load(path string) (*fw.Config, error) { return LoadWith(path, nil) }

// LoadWith is Load with an override. The override runs after the variables
// and before the profiles are read and the result is validated. The command
// line passes its flags this way, so a flag wins over a variable.
func LoadWith(path string, override func(*fw.Config)) (*fw.Config, error) {
	return load(path, override, os.Environ())
}

// LoadBeforeMerge is LoadWith for a caller that adds tools.mcp entries
// after the load, such as fw serve under ax, which merges the MCP servers
// of the Workspace. It skips the check that policy.external.mcp names an
// entry of tools.mcp, because the entry can come from the merge. The caller runs CheckControlServers after the merge.
func LoadBeforeMerge(path string, override func(*fw.Config)) (*fw.Config, error) {
	return loadChecked(path, override, os.Environ(), false)
}

// load is LoadWith with the environment as NAME=VALUE pairs, so a test
// injects one.
func load(path string, override func(*fw.Config), list []string) (*fw.Config, error) {
	return loadChecked(path, override, list, true)
}

// loadChecked is load. servers selects the check that the control servers
// exist in tools.mcp.
func loadChecked(path string, override func(*fw.Config), list []string, servers bool) (*fw.Config, error) {
	env := parseEnviron(list)
	cfg := fw.DefaultConfig()
	dir := ""
	if path != "" {
		if err := decodeFile(path, cfg, env.lookup); err != nil {
			return nil, err
		}
		dir = filepath.Dir(path)
		cfg.ConfigFile = path
	}
	if err := applyEnv(cfg, list); err != nil {
		return nil, err
	}
	if override != nil {
		override(cfg)
	}
	if err := loadProfiles(cfg, dir, env.lookup); err != nil {
		return nil, err
	}
	if err := validate(cfg, servers); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Find returns the configuration file to load: $FW_CONFIG when it is set,
// else the first file that exists of ./fw.yaml and <home>/fw.yaml, else "".
// The home is $FW_HOME, or fw.DefaultHome() when FW_HOME is unset.
//
// Find returns $FW_CONFIG even when the file does not exist, so that Load
// reports the missing file instead of loading another one.
func Find() string { return FindIn("") }

// FindIn is Find with home in place of $FW_HOME. An empty home means Find.
func FindIn(home string) string {
	if p := os.Getenv("FW_CONFIG"); p != "" {
		return p
	}
	if isFile("fw.yaml") {
		return "fw.yaml"
	}
	if home == "" {
		home = os.Getenv("FW_HOME")
	}
	if home == "" {
		home = fw.DefaultHome()
	}
	if p := filepath.Join(home, "fw.yaml"); isFile(p) {
		return p
	}
	return ""
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// decodeFile overlays a YAML file on v, with the variables in its values
// expanded through lookup. Unknown keys and unset variables are errors
// that name the file.
func decodeFile(path string, v any, lookup lookupFunc) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := decodeYAML(data, v, lookup); err != nil {
		return fmt.Errorf("config: %s: %w", path, err)
	}
	return nil
}

// maxSystemPath is the longest System value that is tried as a file path.
const maxSystemPath = 1024

// loadProfiles names every profile after its key, adds one profile per file
// under <home>/agents, and replaces a System value or an Instructions entry
// that names a file with the file contents. A profile from fw.yaml wins
// over a file of the same name. The names that came from files are
// recorded for the profile store (see store.go).
func loadProfiles(cfg *fw.Config, configDir string, lookup lookupFunc) error {
	home := cfg.Home
	if home == "" {
		home = fw.DefaultHome()
	}
	if cfg.Agents == nil {
		cfg.Agents = map[string]fw.AgentProfile{}
	}
	// searchDirs holds the extra directory to search first for the prompt
	// files of a profile loaded from <home>/agents.
	searchDirs := map[string]string{}
	for name, p := range cfg.Agents {
		p.Name = name
		cfg.Agents[name] = p
	}

	agentsDir := filepath.Join(home, "agents")
	var files []string
	for _, pattern := range []string{"*.yaml", "*.yml"} {
		m, err := filepath.Glob(filepath.Join(agentsDir, pattern))
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}
		files = append(files, m...)
	}
	sort.Strings(files)
	var errs []string
	fromFiles := map[string]bool{}
	for _, f := range files {
		var p fw.AgentProfile
		if err := decodeFile(f, &p, lookup); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		name := p.Name
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))
		}
		if _, ok := cfg.Agents[name]; ok {
			continue
		}
		p.Name = name
		cfg.Agents[name] = p
		searchDirs[name] = agentsDir
		fromFiles[name] = true
	}
	cfg.FileProfiles = fromFiles

	for name, p := range cfg.Agents {
		text, err := resolvePrompt(p.System, searchDirs[name], configDir, home)
		if err != nil {
			errs = append(errs, fmt.Sprintf("config: agents.%s.system: %v", name, err))
			continue
		}
		p.System = text
		for i, s := range p.Instructions {
			text, err := resolvePrompt(s, searchDirs[name], configDir, home)
			if err != nil {
				errs = append(errs, fmt.Sprintf("config: agents.%s.instructions[%d]: %v", name, i, err))
				continue
			}
			p.Instructions[i] = text
		}
		cfg.Agents[name] = p
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return errors.New(strings.Join(errs, "\n"))
	}
	return nil
}

// resolvePrompt returns the contents of the file that s names, searched
// relative to each non-empty directory in order and then as given. A value
// that names no file is the prompt text itself.
func resolvePrompt(s string, dirs ...string) (string, error) {
	if s == "" || len(s) > maxSystemPath || strings.ContainsAny(s, "\n\r") || !promptFile(s) {
		return s, nil
	}
	var candidates []string
	if filepath.IsAbs(s) {
		candidates = []string{s}
	} else {
		for _, d := range dirs {
			if d != "" {
				candidates = append(candidates, filepath.Join(d, s))
			}
		}
		candidates = append(candidates, s)
	}
	for _, c := range candidates {
		if !isFile(c) {
			continue
		}
		// The check of the path runs on the file itself, not on a link
		// to it, so a link notes.md to /proc/self/environ reads nothing.
		real, err := filepath.EvalSymlinks(c)
		if err != nil || !promptPath(real) {
			return s, nil
		}
		b, err := os.ReadFile(real)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return s, nil
}

// promptFile reports whether s can name a prompt file: a path that ends
// in .md or .txt, outside /proc, /sys, and /dev. Any other value is the
// prompt text itself, so a profile that reaches the process through the
// API or a rendered workspace cannot read a file of the process, such as
// /proc/self/environ, into its prompt.
func promptFile(s string) bool {
	switch strings.ToLower(filepath.Ext(s)) {
	case ".md", ".txt":
	default:
		return false
	}
	return promptPath(s)
}

// promptPath reports whether a path is outside /proc, /sys, and /dev.
func promptPath(s string) bool {
	clean := filepath.Clean(s)
	for _, p := range []string{"/proc/", "/sys/", "/dev/"} {
		if strings.HasPrefix(clean, p) {
			return false
		}
	}
	return true
}

// The allowed values of the enumerated keys.
var (
	authModes      = []string{"token", "none", "mtls", "oauth", "oidc", "spiffe"}
	logLevels      = []string{"debug", "info", "warn", "error"}
	clientAuths    = []string{"none", "request", "require"}
	accessDefaults = []string{"open", "closed"}
	principalKinds = []string{fw.KindUser, fw.KindWorkload, fw.KindToken, fw.KindLocal, fw.KindAnonymous}
)

// ValidationError lists every problem that Validate found.
type ValidationError struct {
	Problems []string
}

// Error implements error.
func (e *ValidationError) Error() string {
	return "config: invalid configuration:\n  - " + strings.Join(e.Problems, "\n  - ")
}

// Validate checks a configuration and returns one *ValidationError that
// lists every problem, or nil.
func Validate(c *fw.Config) error { return validate(c, true) }

// CheckControlServers makes sure that policy.external.mcp names an entry
// of tools.mcp. It returns one *ValidationError, or nil. Validate runs it
// too. A caller of LoadBeforeMerge runs it after it adds its entries.
func CheckControlServers(c *fw.Config) error {
	var p []string
	for _, x := range externals(c) {
		if x.ext != nil && x.ext.MCP != "" && !hasServer(c, x.ext.MCP) {
			p = append(p, fmt.Sprintf("%s.mcp names %q, but tools.mcp has no entry with that name", x.key, x.ext.MCP))
		}
	}
	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

// validate is Validate. servers selects CheckControlServers.
func validate(c *fw.Config, servers bool) error {
	var p []string
	p = append(p, checkAuth(c.Auth)...)
	p = append(p, checkTLS(c.TLS)...)
	p = append(p, checkAccessConfig(c.Access)...)
	for _, x := range externals(c) {
		p = append(p, checkExternal(x.key, x.ext)...)
	}
	if servers {
		var ve *ValidationError
		if errors.As(CheckControlServers(c), &ve) {
			p = append(p, ve.Problems...)
		}
	}
	if !fw.ValidEffort(c.Effort) {
		p = append(p, fmt.Sprintf("effort %q is not one of %s", c.Effort, strings.Join(fw.Efforts, ", ")))
	}
	if strings.TrimSpace(c.Listen) == "" {
		p = append(p, "listen is empty")
	}
	if c.Model != "" && !isSelector(c.Model) {
		p = append(p, fmt.Sprintf("model %q is not a provider/model selector", c.Model))
	}
	for _, name := range sortedKeys(c.Models) {
		if !isSelector(c.Models[name]) {
			p = append(p, fmt.Sprintf("models.%s: %q is not a provider/model selector", name, c.Models[name]))
		}
	}
	for _, name := range sortedKeys(c.Providers) {
		if !isProviderName(name) {
			p = append(p, fmt.Sprintf("providers.%s: a provider name is a lowercase word such as openai", name))
		}
		if c.Providers[name].Timeout < 0 {
			p = append(p, fmt.Sprintf("providers.%s.timeout %s is negative", name, c.Providers[name].Timeout))
		}
		if c.Providers[name].MaxConcurrent < 0 {
			p = append(p, fmt.Sprintf("providers.%s.maxConcurrent %d is negative", name, c.Providers[name].MaxConcurrent))
		}
	}
	p = append(p, checkLimit("tools.limits", c.Tools.Limits.Limit)...)
	for _, name := range sortedKeys(c.Tools.Limits.PerTool) {
		if _, err := path.Match(name, ""); err != nil {
			p = append(p, fmt.Sprintf("tools.limits.perTool.%s is not a glob", name))
		}
		p = append(p, checkLimit("tools.limits.perTool."+name, c.Tools.Limits.PerTool[name])...)
	}
	if c.Tools.Bash.Timeout < 0 {
		p = append(p, fmt.Sprintf("tools.bash.timeout %s is negative", c.Tools.Bash.Timeout))
	}
	switch strings.ToLower(strings.TrimSpace(c.Tools.Memory.Scope)) {
	case "", fw.MemoryScopeAgent, fw.MemoryScopeShared:
	default:
		p = append(p, fmt.Sprintf("tools.memory.scope %q is not one of %s, %s", c.Tools.Memory.Scope, fw.MemoryScopeAgent, fw.MemoryScopeShared))
	}
	p = append(p, checkBudget("budget", c.Budget)...)
	if c.Compaction.At < 0 {
		p = append(p, fmt.Sprintf("compaction.at %d is negative", c.Compaction.At))
	}
	if m := c.Compaction.Model; m != "" && !isSelector(c.ModelSelector(m)) {
		p = append(p, fmt.Sprintf("compaction.model %q is neither a provider/model selector nor a key of models", m))
	}
	if _, _, err := fw.LoadZone(c.Time.Zone); err != nil {
		p = append(p, fmt.Sprintf("time.zone %q is not an IANA time zone such as America/Chicago, or UTC", c.Time.Zone))
	}
	if c.Time.NoteAfter < 0 {
		p = append(p, fmt.Sprintf("time.noteAfter %s is negative", c.Time.NoteAfter))
	}
	if c.Spawn.MaxDepth < 0 {
		p = append(p, fmt.Sprintf("spawn.maxDepth %d is negative", c.Spawn.MaxDepth))
	}
	if c.Spawn.MaxChildren < 0 {
		p = append(p, fmt.Sprintf("spawn.maxChildren %d is negative", c.Spawn.MaxChildren))
	}
	if c.Spawn.MaxDescendants < 0 {
		p = append(p, fmt.Sprintf("spawn.maxDescendants %d is negative", c.Spawn.MaxDescendants))
	}
	if c.Spawn.MaxRunning < 0 {
		p = append(p, fmt.Sprintf("spawn.maxRunning %d is negative", c.Spawn.MaxRunning))
	}
	for _, name := range sortedKeys(c.Agents) {
		p = append(p, profileProblems(c, name, c.Agents[name])...)
	}
	for i, s := range c.Surfaces {
		if strings.TrimSpace(s) == "" {
			p = append(p, fmt.Sprintf("surfaces[%d] is empty", i))
		}
	}
	if c.Telemetry.Log != "" && !slices.Contains(logLevels, strings.ToLower(c.Telemetry.Log)) {
		p = append(p, fmt.Sprintf("telemetry.log %q is not one of %s", c.Telemetry.Log, strings.Join(logLevels, ", ")))
	}
	if c.MaxRuns < 0 {
		p = append(p, fmt.Sprintf("maxRuns %d is negative", c.MaxRuns))
	}
	if c.Bus.IntakeRetries < 0 {
		p = append(p, fmt.Sprintf("bus.intakeRetries %d is negative", c.Bus.IntakeRetries))
	}
	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

// profileProblems lists the problems of one profile, with each keyed under
// agents.<name>. The profile store runs it before it writes a file, so the
// next load accepts the file.
func profileProblems(c *fw.Config, name string, prof fw.AgentProfile) []string {
	var p []string
	key := "agents." + name
	if prof.Model != "" && !isSelector(c.ModelSelector(prof.Model)) {
		p = append(p, fmt.Sprintf("%s.model %q is neither a provider/model selector nor a key of models", key, prof.Model))
	}
	if !fw.ValidEffort(prof.Effort) {
		p = append(p, fmt.Sprintf("%s.effort %q is not one of %s", key, prof.Effort, strings.Join(fw.Efforts, ", ")))
	}
	if prof.TimeZone != "" {
		if _, _, err := fw.LoadZone(prof.TimeZone); err != nil {
			p = append(p, fmt.Sprintf("%s.timeZone %q is not an IANA time zone such as America/Chicago, or UTC", key, prof.TimeZone))
		}
	}
	for i, s := range prof.Skills {
		if strings.TrimSpace(s) == "" {
			p = append(p, fmt.Sprintf("%s.skills[%d] is empty", key, i))
		}
	}
	for i, s := range prof.Tools {
		if strings.TrimSpace(s) == "" {
			p = append(p, fmt.Sprintf("%s.tools[%d] is empty", key, i))
		}
	}
	if prof.Budget != nil {
		p = append(p, checkBudget(key+".budget", *prof.Budget)...)
	}
	if err := fw.CheckOutcome(c, prof.Outcome); err != nil {
		p = append(p, key+"."+strings.TrimPrefix(err.Error(), fw.ErrInvalidRequest.Error()+": "))
	}
	if prof.Access != nil {
		p = append(p, checkEntries(key+".access.owners", prof.Access.Owners)...)
		p = append(p, checkEntries(key+".access.operators", prof.Access.Operators)...)
		p = append(p, checkEntries(key+".access.viewers", prof.Access.Viewers)...)
	}
	return p
}

// checkAuth lists the problems of the auth block: every mode is known, no
// mode repeats, none stands alone, the block of a mode is present, the
// issuer is an https URL, and the trust domain is a bare host name.
func checkAuth(a fw.AuthConfig) []string {
	var p []string
	known := slices.Clone(authModes)
	for _, m := range fw.RegisteredAuthns() {
		if !slices.Contains(known, m) {
			known = append(known, m)
		}
	}
	modes := a.Modes()
	seen := map[string]bool{}
	for _, m := range modes {
		if !slices.Contains(known, m) {
			p = append(p, fmt.Sprintf("auth.mode %q is not one of %s", m, strings.Join(known, ", ")))
		}
		if seen[m] {
			p = append(p, fmt.Sprintf("auth.mode lists %s twice", m))
		}
		seen[m] = true
	}
	if seen["none"] && len(modes) > 1 {
		p = append(p, "auth.mode none accepts every request, so the other modes in the chain never apply; set one mode")
	}
	if seen["oidc"] && a.OIDC == nil {
		p = append(p, "auth.mode oidc needs the auth.oidc block with issuer")
	}
	if seen["spiffe"] && a.SPIFFE == nil {
		p = append(p, "auth.mode spiffe needs the auth.spiffe block with trustDomain")
	}
	if o := a.OIDC; o != nil {
		switch {
		case strings.TrimSpace(o.Issuer) == "":
			p = append(p, "auth.oidc.issuer is empty")
		case !isHTTPURL(o.Issuer, o.Insecure):
			p = append(p, fmt.Sprintf("auth.oidc.issuer %q is not an https:// URL (auth.oidc.insecure allows http:// for a test)", o.Issuer))
		}
		if o.JWKSURL != "" && !isHTTPURL(o.JWKSURL, o.Insecure) {
			p = append(p, fmt.Sprintf("auth.oidc.jwksUrl %q is not an https:// URL", o.JWKSURL))
		}
	}
	if s := a.SPIFFE; s != nil {
		switch {
		case strings.TrimSpace(s.TrustDomain) == "":
			p = append(p, "auth.spiffe.trustDomain is empty")
		case !isTrustDomain(s.TrustDomain):
			p = append(p, fmt.Sprintf("auth.spiffe.trustDomain %q is not a bare host name such as example.org (no scheme, port, path, or upper case)", s.TrustDomain))
		}
	}
	return p
}

// checkTLS lists the problems of the tls block.
func checkTLS(t fw.ListenTLSConfig) []string {
	var p []string
	provider := strings.ToLower(strings.TrimSpace(t.Provider))
	switch provider {
	case "":
	case "file":
		if t.Cert == "" || t.Key == "" {
			p = append(p, "tls.provider file needs tls.cert and tls.key")
		}
	default:
		// A registered provider, such as spiffe, builds its own configuration.
		// The binary registers its providers before the configuration loads,
		// so an unknown name is a typo or a driver that the build left out.
		if !isProviderName(provider) || !slices.Contains(fw.RegisteredTLSProviders(), provider) {
			known := append([]string{"file"}, fw.RegisteredTLSProviders()...)
			p = append(p, fmt.Sprintf("tls.provider %q is not \"\" or one of %s", t.Provider, strings.Join(known, ", ")))
		}
	}
	clientAuth := strings.ToLower(strings.TrimSpace(t.ClientAuth))
	switch {
	case clientAuth != "" && !slices.Contains(clientAuths, clientAuth):
		p = append(p, fmt.Sprintf("tls.clientAuth %q is not none, request, or require", t.ClientAuth))
	case clientAuth != "" && clientAuth != "none" && provider == "":
		p = append(p, fmt.Sprintf("tls.clientAuth %s needs a tls.provider; a plain HTTP listener verifies no client", clientAuth))
	}
	return p
}

// checkAccessConfig lists the problems of the access block.
func checkAccessConfig(a fw.AccessConfig) []string {
	var p []string
	if d := strings.ToLower(strings.TrimSpace(a.Default)); d != "" && !slices.Contains(accessDefaults, d) {
		p = append(p, fmt.Sprintf("access.default %q is not open or closed", a.Default))
	}
	p = append(p, checkEntries("access.admins", a.Admins)...)
	p = append(p, checkEntries("access.creators", a.Creators)...)
	return p
}

// external is one external decision point of a configuration, by key.
type external struct {
	key string
	ext *fw.ExternalPolicyConfig
}

// externals returns the external decision points of the kernel, set or
// not: policy.external. The control plane of the workbench is in its own
// block, and the workbench checks it.
func externals(c *fw.Config) []external {
	return []external{{"policy.external", c.Policy.External}}
}

// checkExternal lists the problems of one external decision point: it
// sets exactly one of mcp and bus, and its timeout is not negative.
func checkExternal(key string, e *fw.ExternalPolicyConfig) []string {
	if e == nil {
		return nil
	}
	var p []string
	if e.Transport() == "" {
		if e.MCP != "" && e.Bus != "" {
			p = append(p, fmt.Sprintf("%s sets both mcp %q and bus %q; set exactly one", key, e.MCP, e.Bus))
		} else {
			p = append(p, fmt.Sprintf("%s sets neither mcp nor bus; set exactly one", key))
		}
	}
	if e.Timeout < 0 {
		p = append(p, fmt.Sprintf("%s.timeout %s is negative", key, e.Timeout))
	}
	if e.Bus != "" && strings.ContainsAny(e.Bus, " \t*>") {
		p = append(p, fmt.Sprintf("%s.bus %q is not a subject: it holds a space or a wildcard", key, e.Bus))
	}
	return p
}

// hasServer reports whether tools.mcp has an entry with that name. Case
// and the difference between '-' and '_' do not matter, as for FW_*
// variables.
func hasServer(c *fw.Config, name string) bool {
	for _, s := range c.Tools.MCP {
		if sameName(s.Name, name) {
			return true
		}
	}
	return false
}

// checkEntries lists the access entries of a list that are empty, that
// name no group or issuer after their prefix, or that name an unknown kind.
func checkEntries(key string, entries []string) []string {
	var p []string
	for i, e := range entries {
		e = strings.TrimSpace(e)
		switch {
		case e == "":
			p = append(p, fmt.Sprintf("%s[%d] is empty", key, i))
		case e == "group:" || e == "issuer:" || e == "user:" || e == "kind:":
			p = append(p, fmt.Sprintf("%s[%d] %q names nothing after its prefix", key, i, e))
		case strings.HasPrefix(e, "kind:") && !slices.Contains(principalKinds, strings.ToLower(e[len("kind:"):])):
			p = append(p, fmt.Sprintf("%s[%d] %q names no kind; the kinds are %s", key, i, e, strings.Join(principalKinds, ", ")))
		}
	}
	return p
}

// isHTTPURL reports whether s is an https:// URL with a host, or an
// http:// one when insecure is set.
func isHTTPURL(s string, insecure bool) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "https" || (insecure && u.Scheme == "http")
}

// isTrustDomain reports whether s is a SPIFFE trust domain: lowercase
// letters, digits, dots, hyphens, and underscores, with no scheme, port,
// or path.
func isTrustDomain(s string) bool {
	if s == "" || s[0] == '.' || s[len(s)-1] == '.' {
		return false
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

// checkBudget lists the fields of a budget that are negative.
func checkBudget(key string, b fw.Budget) []string {
	var p []string
	for _, f := range []struct {
		name string
		neg  bool
		val  any
	}{
		{"maxTurns", b.MaxTurns < 0, b.MaxTurns},
		{"maxTokens", b.MaxTokens < 0, b.MaxTokens},
		{"maxCostUSD", b.MaxCostUSD < 0, b.MaxCostUSD},
		{"deadline", b.Deadline < 0, b.Deadline},
		{"maxToolCalls", b.MaxToolCalls < 0, b.MaxToolCalls},
		{"maxCallsPerTurn", b.MaxCallsPerTurn < 0, b.MaxCallsPerTurn},
		{"maxRepeats", b.MaxRepeats < 0, b.MaxRepeats},
		{"reportMaxTokens", b.ReportMaxTokens < 0, b.ReportMaxTokens},
		{"reportTimeout", b.ReportTimeout < 0, b.ReportTimeout},
	} {
		if f.neg {
			p = append(p, fmt.Sprintf("%s.%s %v is negative", key, f.name, f.val))
		}
	}
	return p
}

// checkLimit lists the fields of a limit that are negative.
func checkLimit(key string, l fw.Limit) []string {
	var p []string
	if l.MaxInputBytes < 0 {
		p = append(p, fmt.Sprintf("%s.maxInputBytes %d is negative", key, l.MaxInputBytes))
	}
	if l.MaxOutputBytes < 0 {
		p = append(p, fmt.Sprintf("%s.maxOutputBytes %d is negative", key, l.MaxOutputBytes))
	}
	if l.Timeout < 0 {
		p = append(p, fmt.Sprintf("%s.timeout %s is negative", key, l.Timeout))
	}
	return p
}

// isSelector reports whether s has the form provider/model.
func isSelector(s string) bool {
	provider, model := fw.SplitSelector(s)
	return provider != "" && model != "" && strings.Contains(s, "/")
}

// isProviderName reports whether s is a lowercase word: a lowercase
// letter, then lowercase letters and digits.
func isProviderName(s string) bool {
	for i, r := range s {
		if !(r >= 'a' && r <= 'z') && !(i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return s != ""
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Masked returns the configuration as YAML with the value of every string
// whose key contains token, secret, key, or password, in any case, replaced
// by "***", and every value under a headers key, because a header can hold
// a token. Most secrets in the configuration are names, not values, so
// masking hides little that matters.
func Masked(c *fw.Config) ([]byte, error) {
	var n yaml.Node
	if err := n.Encode(c); err != nil {
		return nil, fmt.Errorf("config: encode: %w", err)
	}
	mask(&n, false)
	out, err := yaml.Marshal(&n)
	if err != nil {
		return nil, fmt.Errorf("config: encode: %w", err)
	}
	return out, nil
}

// mask replaces the strings of n when secret is set. A mapping judges each
// value by its own key, so secrets.driver stays visible while
// auth.oauth.clientSecret does not. A headers mapping masks every value.
func mask(n *yaml.Node, secret bool) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			mask(c, secret)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i].Value, n.Content[i+1]
			all := secret || strings.EqualFold(key, "headers")
			if value.Kind == yaml.MappingNode {
				mask(value, all)
			} else {
				mask(value, all || SecretKey(key))
			}
		}
	case yaml.ScalarNode:
		if secret && n.Tag == "!!str" && n.Value != "" {
			n.Value = "***"
			n.Style = 0
		}
	}
}

// SecretKey reports whether a configuration key names a secret: it contains
// token, secret, key, or password, in any case.
func SecretKey(k string) bool {
	k = strings.ToLower(k)
	// A key that names where a secret lives, such as tokenEnv, holds a name,
	// not a value. A key that names a mode, such as the OpenAI options
	// maxTokensField and promptCacheKey, holds a word, not a value.
	if strings.HasSuffix(k, "env") || strings.HasSuffix(k, "field") || k == "promptcachekey" {
		return false
	}
	for _, w := range []string{"token", "secret", "key", "password"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}
