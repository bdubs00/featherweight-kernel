package env

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/env/pkglist"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// doctor is the line of fw doctor: the state of the environment, whether
// uv and pixi are on PATH, and the registry hosts that the egress policy
// of a Task must let through.
func doctor(ctx context.Context, cfg *fw.Config, _ fw.Secrets, timeout time.Duration) (bool, string) {
	s, err := load(cfg)
	if err != nil {
		return false, "environment: " + err.Error()
	}
	if s == nil {
		return true, "environment: none (no block environment)"
	}
	ok := true
	st := s.state()
	var parts []string
	switch st.Kind {
	case StateReady:
		m := st.Marker
		line := fmt.Sprintf("ready %s in %s", short(m.Digest), s.dir)
		if m.Seconds > 0 {
			line += fmt.Sprintf(", installed %s in %.1fs", m.Ready.Format(time.DateOnly), m.Seconds)
		}
		if s.placement == PlacementImage && s.manifestsPresent() && m.Digest != st.Digest {
			ok = false
			line = fmt.Sprintf("the image holds %s, but the manifests give %s; build the image again with fw env dockerfile", short(m.Digest), short(st.Digest))
		}
		parts = append(parts, line)
	case StateFailed:
		ok = false
		line := fmt.Sprintf("failed at the step %s: %s", st.Failure.Step, st.Failure.Error)
		if st.Failure.Attempt > 0 {
			line += fmt.Sprintf(" (attempt %d", st.Failure.Attempt)
			if wait := time.Until(st.Failure.RetryAt).Round(time.Second); wait > 0 {
				line += fmt.Sprintf(", next try in %s", wait)
			}
			line += ")"
		}
		parts = append(parts, line)
	case StateMaterializing:
		parts = append(parts, fmt.Sprintf("materializing %s in %s", short(st.Digest), s.dir))
	default:
		parts = append(parts, fmt.Sprintf("none installed for %s in %s yet; fw serve installs it in the background, and the first tool process of a run waits for it", short(st.Digest), s.dir))
	}
	if s.placement == PlacementDurable {
		for _, tool := range s.managers() {
			v, err := probe(ctx, tool, timeout)
			if err != nil {
				ok = false
				parts = append(parts, fmt.Sprintf("%s is not on PATH; the image must carry it, because fw does not download it", tool))
				continue
			}
			parts = append(parts, v+" on PATH")
		}
		if s.isInline(pkglist.Python) {
			// uv downloads no interpreter for a list.
			v, err := probe(ctx, "python3", timeout)
			if err != nil {
				ok = false
				parts = append(parts, fmt.Sprintf("python3 is not on PATH; the image must carry it for %s.packages.python, because uv downloads no interpreter", Block))
			} else {
				parts = append(parts, v+" on PATH")
			}
		}
		if hosts := s.hosts(); len(hosts) > 0 {
			parts = append(parts, "the egress policy needs "+strings.Join(hosts, ", "))
		}
	}
	return ok, "environment: " + strings.Join(parts, "; ")
}

// manifestsPresent reports whether the workspace holds every manifest of
// the block, so its digest can be compared with the digest of an image.
func (s *spec) manifestsPresent() bool {
	for _, p := range []string{s.python, s.tools} {
		if p != "" && !exists(p) {
			return false
		}
	}
	return true
}

// managers names the managers that the block needs.
func (s *spec) managers() []string {
	var out []string
	if s.python != "" {
		out = append(out, "uv")
	}
	if s.tools != "" {
		out = append(out, "pixi")
	}
	return out
}

// probe returns the first line of `<name> --version`.
func probe(ctx context.Context, name string, timeout time.Duration) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, p, "--version").Output()
	if err != nil {
		return "", err
	}
	return firstLine(string(out)), nil
}

// hostRef matches the scheme, the host, and the port of a URL in a lock
// file.
var hostRef = regexp.MustCompile(`\b(https?)://([A-Za-z0-9][A-Za-z0-9.-]*[A-Za-z0-9](?::[0-9]+)?)`)

// pythonHost is where uv downloads a Python when the image has none.
const pythonHost = "releases.astral.sh"

// hosts lists the hosts that the lock files download from, as egress
// rules name them: "tls <host>" for https, which needs a rule with tls
// passthrough, and "http <host>" for plain http, each with the port of
// the URL when it names one. Without a lock file it names the default
// registry of the manager. Without python3 on PATH, uv downloads a Python
// for a manifest too. A mirror is the host of its manager. pixi takes
// every download of conda-forge from the mirror, and so does uv for a
// list, which the kernel locks against the mirror. A manifest of uv keeps
// the URLs of its lock file, so those hosts stay in the list too.
func (s *spec) hosts() []string {
	seen := map[string]bool{}
	add := func(rule string) {
		seen[rule] = true
	}
	scan := func(lock string, defaults ...string) {
		b, err := os.ReadFile(lock)
		if err != nil {
			for _, d := range defaults {
				add("tls " + d)
			}
			return
		}
		for _, m := range hostRef.FindAllSubmatch(b, -1) {
			scheme := "tls"
			if string(m[1]) == "http" {
				scheme = "http"
			}
			add(scheme + " " + strings.ToLower(string(m[2])))
		}
	}
	mirror := func(manager string) bool {
		m := s.mirrors[manager]
		if m == "" {
			return false
		}
		if u, err := url.Parse(m); err == nil {
			scheme := "tls"
			if u.Scheme == "http" {
				scheme = "http"
			}
			add(scheme + " " + strings.ToLower(u.Host))
		}
		return true
	}
	if s.python != "" {
		hasMirror := mirror(pkglist.Python)
		switch {
		case hasMirror && s.isInline(pkglist.Python):
			// The kernel locks a list against the mirror.
		case hasMirror:
			scan(s.uvLock())
		default:
			scan(s.uvLock(), "pypi.org", "files.pythonhosted.org")
		}
		if _, err := exec.LookPath("python3"); err != nil && !s.isInline(pkglist.Python) {
			add("tls " + pythonHost)
		}
	}
	if s.tools != "" && !mirror(pkglist.Tools) {
		scan(s.pixiLock(), "conda.anaconda.org")
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	slices.Sort(out)
	return out
}
