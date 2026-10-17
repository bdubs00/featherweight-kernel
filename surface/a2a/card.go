package a2a

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// Card returns the agent card of a. The name and the description come from
// Config.A2A, the version from Config.Version, and each profile becomes one
// skill. The interface URLs start with Config.A2A.URL, or with "http://" and
// the listen address when Config.A2A.URL is empty.
func Card(a *fw.Agent) (*sdk.AgentCard, error) {
	base, err := configBase(a.Config())
	if err != nil {
		return nil, err
	}
	return cardFor(a, base), nil
}

// CardJSON returns the agent card of a as indented JSON, for the command line.
func CardJSON(a *fw.Agent) ([]byte, error) {
	c, err := Card(a)
	if err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("a2a: marshal card: %w", err)
	}
	return b, nil
}

func cardFor(a *fw.Agent, base string) *sdk.AgentCard {
	cfg := a.Config()
	name := cfg.A2A.Name
	if name == "" {
		name = "featherweight"
	}
	desc := cfg.A2A.Description
	if desc == "" {
		desc = name
	}
	version := cfg.Version
	if version == "" {
		version = "dev"
	}
	profiles := a.Profiles()
	skills := make([]sdk.AgentSkill, 0, len(profiles))
	for _, p := range profiles {
		skills = append(skills, sdk.AgentSkill{
			ID:          p.Name,
			Name:        p.Name,
			Description: firstLine(p.System, p.Name),
			Tags:        []string{"fw"},
		})
	}
	modes := []string{"text/plain"}
	card := &sdk.AgentCard{
		Name:        name,
		Description: desc,
		Version:     version,
		SupportedInterfaces: []*sdk.AgentInterface{
			sdk.NewAgentInterface(base+PathJSONRPC, sdk.TransportProtocolJSONRPC),
			sdk.NewAgentInterface(base+PathREST, sdk.TransportProtocolHTTPJSON),
		},
		Capabilities:       sdk.AgentCapabilities{Streaming: true, PushNotifications: true},
		DefaultInputModes:  modes,
		DefaultOutputModes: modes,
		Skills:             skills,
	}
	if !isNoAuthn(a.Authn()) {
		card.SecuritySchemes = sdk.NamedSecuritySchemes{
			"bearer": sdk.HTTPAuthSecurityScheme{Scheme: "Bearer"},
		}
		card.SecurityRequirements = sdk.SecurityRequirementsOptions{{"bearer": {}}}
	}
	return card
}

func isNoAuthn(a fw.Authn) bool {
	switch a.(type) {
	case fw.NoAuthn, *fw.NoAuthn:
		return true
	}
	return false
}

// firstLine returns the first line of a system prompt that holds text,
// without a leading Markdown heading mark, or def when there is none.
func firstLine(s, def string) string {
	for line := range strings.Lines(s) {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if line != "" {
			return line
		}
	}
	return def
}

// configBase returns the base URL of the interfaces from the configuration.
func configBase(cfg *fw.Config) (string, error) {
	if cfg.A2A.URL != "" {
		u, err := url.Parse(cfg.A2A.URL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return "", fmt.Errorf("a2a: a2a.url %q is not an absolute URL", cfg.A2A.URL)
		}
		return strings.TrimRight(cfg.A2A.URL, "/"), nil
	}
	return "http://" + listenHost(cfg.Listen), nil
}

// requestBase returns the base URL for a card that a request asked for:
// Config.A2A.URL when it is set, else the scheme and Host of the request.
func requestBase(cfg *fw.Config, r *http.Request) string {
	if cfg.A2A.URL != "" || r.Host == "" {
		base, _ := configBase(cfg) // Mount checked the URL
		return base
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// listenHost turns a listen address into the host part of a URL. Port 80 is
// dropped. An empty or unspecified host becomes "localhost", because a card
// with "http://:8080" names no host that a client can dial.
func listenHost(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		host, port = listen, ""
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "localhost"
	}
	if port == "" || port == "80" {
		if strings.Contains(host, ":") {
			return "[" + host + "]"
		}
		return host
	}
	return net.JoinHostPort(host, port)
}
