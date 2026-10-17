package config

import (
	"context"
	"crypto/tls"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The binary registers the spiffe TLS provider through its driver. The
// tests of this package register a stand-in under the same name, so the
// validation sees a registered provider.
func init() {
	fw.RegisterTLSProvider("spiffe", func(context.Context, *fw.Config, fw.Secrets) (*tls.Config, error) {
		return &tls.Config{}, nil
	})
}

// Every rule of the identity contract reports one problem that names its
// key.
func TestValidateIdentityRules(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), `
auth:
  mode: "oidc, password, none, oidc"
  oidc: {issuer: "idp.example.com", jwksUrl: "ftp://keys"}
  spiffe: {trustDomain: "spiffe://Example.org/path"}
tls:
  provider: "not a word"
  clientAuth: maybe
access:
  default: ajar
  admins: ["", "kind:robot", "group:"]
  creators: ["   "]
agents:
  a:
    access:
      owners: [""]
      operators: ["issuer:"]
      viewers: ["kind:Workload", "user:"]
`)
	_, err := Load(path)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error %v is not a ValidationError", err)
	}
	wants := []string{
		`auth.mode "password" is not one of token, none, mtls, oauth, oidc, spiffe`,
		"auth.mode lists oidc twice",
		"auth.mode none accepts every request",
		`auth.oidc.issuer "idp.example.com" is not an https:// URL`,
		`auth.oidc.jwksUrl "ftp://keys" is not an https:// URL`,
		`auth.spiffe.trustDomain "spiffe://Example.org/path" is not a bare host name`,
		`tls.provider "not a word" is not "" or one of file, spiffe`,
		`tls.clientAuth "maybe" is not none, request, or require`,
		`access.default "ajar" is not open or closed`,
		"access.admins[0] is empty",
		`access.admins[1] "kind:robot" names no kind; the kinds are user, workload, token, local, anonymous`,
		`access.admins[2] "group:" names nothing after its prefix`,
		"access.creators[0] is empty",
		"agents.a.access.owners[0] is empty",
		`agents.a.access.operators[0] "issuer:" names nothing`,
		`agents.a.access.viewers[1] "user:" names nothing`,
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error lacks %q:\n%v", w, err)
		}
	}
	if len(ve.Problems) != len(wants) {
		t.Errorf("%d problems, want %d:\n%v", len(ve.Problems), len(wants), err)
	}
	if strings.Contains(err.Error(), "kind:Workload") {
		t.Errorf("a kind is matched with case ignored:\n%v", err)
	}
}

func TestValidateIdentityEachRule(t *testing.T) {
	cases := []struct {
		name string
		set  func(*fw.Config)
		want string // "" for a valid configuration
	}{
		{"chain of known modes", func(c *fw.Config) {
			c.Auth.Mode = "oidc,spiffe,token"
			c.Auth.OIDC = &fw.OIDCConfig{Issuer: "https://idp.example.com/realm"}
			c.Auth.SPIFFE = &fw.SPIFFEConfig{TrustDomain: "prod.example.org"}
		}, ""},
		{"oidc needs its block", func(c *fw.Config) { c.Auth.Mode = "oidc" }, "auth.mode oidc needs the auth.oidc block with issuer"},
		{"spiffe needs its block", func(c *fw.Config) { c.Auth.Mode = "token,spiffe" }, "auth.mode spiffe needs the auth.spiffe block with trustDomain"},
		{"empty issuer", func(c *fw.Config) { c.Auth.OIDC = &fw.OIDCConfig{} }, "auth.oidc.issuer is empty"},
		{"http issuer", func(c *fw.Config) { c.Auth.OIDC = &fw.OIDCConfig{Issuer: "http://idp"} }, "auth.oidc.issuer \"http://idp\" is not an https:// URL"},
		{"http issuer when insecure", func(c *fw.Config) { c.Auth.OIDC = &fw.OIDCConfig{Issuer: "http://idp", Insecure: true} }, ""},
		{"issuer without a host", func(c *fw.Config) { c.Auth.OIDC = &fw.OIDCConfig{Issuer: "https://"} }, "auth.oidc.issuer"},
		{"empty trust domain", func(c *fw.Config) { c.Auth.SPIFFE = &fw.SPIFFEConfig{} }, "auth.spiffe.trustDomain is empty"},
		{"trust domain with a port", func(c *fw.Config) { c.Auth.SPIFFE = &fw.SPIFFEConfig{TrustDomain: "example.org:443"} }, "auth.spiffe.trustDomain"},
		{"trust domain with upper case", func(c *fw.Config) { c.Auth.SPIFFE = &fw.SPIFFEConfig{TrustDomain: "Example.org"} }, "auth.spiffe.trustDomain"},
		{"trust domain with a dot at the end", func(c *fw.Config) { c.Auth.SPIFFE = &fw.SPIFFEConfig{TrustDomain: "example.org."} }, "auth.spiffe.trustDomain"},
		{"trust domain with underscore and digits", func(c *fw.Config) { c.Auth.SPIFFE = &fw.SPIFFEConfig{TrustDomain: "team_1.example-org"} }, ""},
		{"tls file provider", func(c *fw.Config) { c.TLS = fw.ListenTLSConfig{Provider: "file", Cert: "/c", Key: "/k"} }, ""},
		{"tls file provider without a key", func(c *fw.Config) { c.TLS = fw.ListenTLSConfig{Provider: "File", Cert: "/c"} }, "tls.provider file needs tls.cert and tls.key"},
		{"tls registered provider by name", func(c *fw.Config) { c.TLS = fw.ListenTLSConfig{Provider: "spiffe"} }, ""},
		{"tls client auth", func(c *fw.Config) { c.TLS = fw.ListenTLSConfig{Provider: "spiffe", ClientAuth: "Require"} }, ""},
		{"tls client auth without a provider", func(c *fw.Config) { c.TLS = fw.ListenTLSConfig{ClientAuth: "require"} }, "tls.clientAuth require needs a tls.provider"},
		{"tls client auth none without a provider", func(c *fw.Config) { c.TLS = fw.ListenTLSConfig{ClientAuth: "none"} }, ""},
		{"access closed", func(c *fw.Config) { c.Access = fw.AccessConfig{Default: "Closed", Admins: []string{"local:*"}} }, ""},
		{"access default empty", func(c *fw.Config) { c.Access.Default = "" }, ""},
		{"access entries of every form", func(c *fw.Config) {
			c.Access.Admins = []string{"*", "alice@example.com", "spiffe://td/ns/*", "group:dev", "kind:local", "issuer:https://idp", "user:bob"}
		}, ""},
		{"profile access viewers empty entry", func(c *fw.Config) {
			c.Agents = map[string]fw.AgentProfile{"r": {Access: &fw.Access{Viewers: []string{"a", ""}}}}
		}, "agents.r.access.viewers[1] is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fw.DefaultConfig()
			tc.set(cfg)
			err := Validate(cfg)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

// The pointer blocks of auth and the access lists are reachable from the
// environment, in the file and in a profile.
func TestEnvIdentityBlocks(t *testing.T) {
	cases := []struct {
		name string
		file func(*fw.Config)
		env  map[string]string
		get  func(*fw.Config) any
		want any
	}{
		{"auth.oidc from nothing", nil, map[string]string{
			"FW_AUTH_OIDC_ISSUER": "https://idp.example.com", "FW_AUTH_OIDC_AUDIENCE": "fw, api", "FW_AUTH_OIDC_GROUPS_CLAIM": "groups",
			"FW_AUTH_OIDC_NAME_CLAIM": "email", "FW_AUTH_OIDC_JWKS_URL": "https://idp/keys", "FW_AUTH_OIDC_CA": "/ca.pem", "FW_AUTH_OIDC_INSECURE": "true", "FW_AUTH_OIDC_SCOPES": "a",
		},
			func(c *fw.Config) any { return c.Auth.OIDC },
			&fw.OIDCConfig{Issuer: "https://idp.example.com", Audience: []string{"fw", "api"}, GroupsClaim: "groups", NameClaim: "email", JWKSURL: "https://idp/keys", CA: "/ca.pem", Insecure: true, Scopes: []string{"a"}}},
		{"auth.oidc keeps the file fields", func(c *fw.Config) { c.Auth.OIDC = &fw.OIDCConfig{Issuer: "https://file", Audience: []string{"x"}} },
			map[string]string{"FW_AUTH_OIDC_ISSUER": "https://env"},
			func(c *fw.Config) any { return c.Auth.OIDC }, &fw.OIDCConfig{Issuer: "https://env", Audience: []string{"x"}}},
		{"auth.oidc stays nil", nil, map[string]string{"FW_AUTH_MODE": "token"},
			func(c *fw.Config) any { return c.Auth.OIDC == nil }, true},
		{"auth.spiffe", nil, map[string]string{
			"FW_AUTH_SPIFFE_TRUST_DOMAIN": "example.org", "FW_AUTH_SPIFFE_BUNDLE": "/bundle.json", "FW_AUTH_SPIFFE_SOCKET": "unix:///run/spire/agent.sock",
			"FW_AUTH_SPIFFE_AUDIENCE": "fw", "FW_AUTH_SPIFFE_X509": "true",
		},
			func(c *fw.Config) any { return c.Auth.SPIFFE },
			&fw.SPIFFEConfig{TrustDomain: "example.org", Bundle: "/bundle.json", Socket: "unix:///run/spire/agent.sock", Audience: []string{"fw"}, X509: true}},
		{"auth.mode chain", nil, map[string]string{"FW_AUTH_MODE": "oidc, token"},
			func(c *fw.Config) any { return c.Auth.Modes() }, []string{"oidc", "token"}},
		{"access lists", nil, map[string]string{"FW_ACCESS_DEFAULT": "closed", "FW_ACCESS_ADMINS": "local:*, alice@example.com", "FW_ACCESS_CREATORS": "group:dev"},
			func(c *fw.Config) any { return c.Access },
			fw.AccessConfig{Default: "closed", Admins: []string{"local:*", "alice@example.com"}, Creators: []string{"group:dev"}}},
		{"profile access from nothing", nil, map[string]string{"FW_AGENTS_REVIEWER_ACCESS_OWNERS": "alice,bob", "FW_AGENTS_REVIEWER_ACCESS_VIEWERS": "*"},
			func(c *fw.Config) any { return c.Agents["reviewer"].Access },
			&fw.Access{Owners: []string{"alice", "bob"}, Viewers: []string{"*"}}},
		{"profile access keeps the file lists", func(c *fw.Config) {
			c.Agents = map[string]fw.AgentProfile{"code-review": {Access: &fw.Access{Owners: []string{"file"}, Operators: []string{"ops"}}}}
		}, map[string]string{"FW_AGENTS_CODE_REVIEW_ACCESS_OWNERS": "env"},
			func(c *fw.Config) any { return c.Agents["code-review"].Access },
			&fw.Access{Owners: []string{"env"}, Operators: []string{"ops"}}},
		{"tls block", nil, map[string]string{"FW_TLS_PROVIDER": "file", "FW_TLS_CERT": "/c", "FW_TLS_KEY": "/k", "FW_TLS_CLIENT_CA": "/ca", "FW_TLS_CLIENT_AUTH": "require"},
			func(c *fw.Config) any { return c.TLS },
			fw.ListenTLSConfig{Provider: "file", Cert: "/c", Key: "/k", ClientCA: "/ca", ClientAuth: "require"}},
		{"mcp spiffe auth", nil, map[string]string{"FW_TOOLS_MCP_PLANNER_AUTH_SPIFFE_AUDIENCE": "planner"},
			func(c *fw.Config) any { return c.Tools.MCP[0].Auth.SPIFFE }, &fw.SPIFFEAuthConfig{Audience: "planner"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fw.DefaultConfig()
			if tc.file != nil {
				tc.file(cfg)
			}
			if err := applyEnv(cfg, envList(tc.env)); err != nil {
				t.Fatal(err)
			}
			if got := tc.get(cfg); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
	names := EnvNames()
	for _, want := range []string{
		"FW_AUTH_OIDC_ISSUER", "FW_AUTH_OIDC_AUDIENCE", "FW_AUTH_SPIFFE_TRUST_DOMAIN", "FW_AUTH_SPIFFE_X509",
		"FW_ACCESS_DEFAULT", "FW_ACCESS_ADMINS", "FW_ACCESS_CREATORS", "FW_AGENTS_<NAME>_ACCESS_OWNERS", "FW_AGENTS_<NAME>_ACCESS_VIEWERS",
		"FW_TLS_PROVIDER", "FW_TLS_CLIENT_AUTH", "FW_TOOLS_MCP_<NAME>_AUTH_SPIFFE_AUDIENCE",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("EnvNames lacks %s", want)
		}
	}
}

// The identity blocks reach Validate through the environment too: a file
// with the mode and the variables with the block.
func TestLoadIdentityFromEnv(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), "auth: {mode: oidc}\naccess: {default: closed}\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "auth.mode oidc needs the auth.oidc block") {
		t.Errorf("without the block: %v", err)
	}
	t.Setenv("FW_AUTH_OIDC_ISSUER", "https://idp.example.com")
	t.Setenv("FW_ACCESS_ADMINS", "local:*")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.OIDC == nil || cfg.Auth.OIDC.Issuer != "https://idp.example.com" || cfg.Access.Admins[0] != "local:*" {
		t.Errorf("config: %+v %+v", cfg.Auth.OIDC, cfg.Access)
	}
}

// Access lists hold names, not secrets, and stay visible. The TLS key path
// is masked like every key value.
func TestMaskedKeepsAccess(t *testing.T) {
	c := fw.DefaultConfig()
	c.Access = fw.AccessConfig{Default: "closed", Admins: []string{"local:*", "alice@example.com", "kind:token"}, Creators: []string{"group:dev"}}
	c.Agents = map[string]fw.AgentProfile{"r": {Access: &fw.Access{Owners: []string{"spiffe://td/ns/prod/sa/reviewer"}, Viewers: []string{"issuer:https://idp"}}}}
	c.Auth.OIDC = &fw.OIDCConfig{Issuer: "https://idp", Audience: []string{"fw"}, GroupsClaim: "groups", JWKSURL: "https://idp/jwks"}
	c.Auth.SPIFFE = &fw.SPIFFEConfig{TrustDomain: "example.org", Socket: "unix:///run/agent.sock"}
	c.TLS = fw.ListenTLSConfig{Provider: "file", Cert: "/etc/fw/cert.pem", Key: "/etc/fw/key.pem", ClientCA: "/etc/fw/ca.pem", ClientAuth: "require"}
	out, err := Masked(c)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, shown := range []string{
		"local:*", "alice@example.com", "kind:token", "group:dev", "spiffe://td/ns/prod/sa/reviewer", "issuer:https://idp",
		"default: closed", "issuer: https://idp", "groupsClaim: groups", "jwksUrl: https://idp/jwks", "trustDomain: example.org",
		"cert: /etc/fw/cert.pem", "clientCA: /etc/fw/ca.pem", "clientAuth: require",
	} {
		if !strings.Contains(s, shown) {
			t.Errorf("%q is missing:\n%s", shown, s)
		}
	}
	if strings.Contains(s, "/etc/fw/key.pem") {
		t.Errorf("the key path is not masked:\n%s", s)
	}
}
