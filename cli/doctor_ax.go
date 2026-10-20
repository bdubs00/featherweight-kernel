package cli

// This file adds the lines of fw doctor for the facts that the sandbox of
// an ax Task hides (docs/ARCHITECTURE.md section 21): the reach of each
// endpoint through the egress gateway, the trust bundle of the gateway,
// the user of the processes of tools, and the durable paths. They run
// inside the Task, through ax ssh or a debug Task, and need no cluster
// client. Outside ax, one line says that they do not apply.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/axmeta"
)

// Paths that the checks read. Tests point them at temporary files.
var (
	// axTrustBundle is where a SystemInfo volume of Agent Substrate puts
	// the trust bundle of the egress gateway, as docs/egress-trust-bundle.md
	// of Substrate mounts it. ax mounts no such volume yet.
	axTrustBundle = "/run/ate/egress-trust-bundle/trust-bundle.pem"
	axMountInfo   = "/proc/self/mountinfo"
	axWorkspace   = ax.DefaultWorkspacePath
)

// axProbeLimit bounds one probe of an endpoint.
const axProbeLimit = 5 * time.Second

// The process environment decides at start whether fw runs under ax, so a
// laptop gets one line and a Task gets four.
func init() {
	for _, c := range axDoctorChecks(underAX()) {
		RegisterDoctorCheck(c)
	}
}

// underAX reports whether this process runs inside an ax Task: ax gave it
// the Task, or the metadata endpoints of the kernel, as ax ssh does.
func underAX() bool {
	return ax.Detect() || strings.TrimSpace(os.Getenv(ax.EnvMetadataURL)) != ""
}

func axDoctorChecks(under bool) []DoctorCheck {
	if !under {
		return []DoctorCheck{func(context.Context, *fw.Config, fw.Secrets, time.Duration) (bool, string) {
			return true, "ax: not under ax (no AX_TASK_YAML and no AX_METADATA_URL); the lines of a Task run inside one, through ax ssh"
		}}
	}
	return []DoctorCheck{doctorAXReach, doctorAXTrust, doctorAXToolUser, doctorAXDurable}
}

// axManifests returns the Task and its Workspaces from AX_TASK_YAML, or
// from the metadata endpoints at AX_METADATA_URL.
func axManifests(ctx context.Context, timeout time.Duration) (*ax.Task, []*ax.Workspace, error) {
	if ax.Detect() {
		return ax.LoadFromEnv()
	}
	base := strings.TrimRight(os.Getenv(ax.EnvMetadataURL), "/")
	client := &http.Client{Timeout: timeout}
	// The endpoints answer 404 when the kernel has no manifest of a kind.
	read := func(p string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+p, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK:
			return io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes))
		case http.StatusNotFound:
			return nil, nil
		}
		return nil, fmt.Errorf("%s answered %s", base+p, resp.Status)
	}
	data, err := read(axmeta.PathTask)
	if err != nil {
		return nil, nil, fmt.Errorf("ax: read the Task: %w", err)
	}
	task, err := ax.ParseTask(data)
	if err != nil {
		return nil, nil, err
	}
	if data, err = read(axmeta.PathWorkspaces); err != nil {
		return nil, nil, fmt.Errorf("ax: read the Workspaces: %w", err)
	}
	ws, err := ax.ParseWorkspaces(data)
	return task, ws, err
}

// doctorAXReach probes each endpoint of the kernel through the egress
// gateway, as the kernel dials it, with the MCP servers of the Workspaces.
func doctorAXReach(ctx context.Context, cfg *fw.Config, _ fw.Secrets, timeout time.Duration) (bool, string) {
	c := *cfg
	c.Tools.MCP = slices.Clone(cfg.Tools.MCP)
	task, wss, err := axManifests(ctx, timeout)
	if err != nil {
		return false, "ax reach: " + err.Error()
	}
	for _, b := range ax.Bindings(task, wss) {
		if b.Workspace == nil {
			continue
		}
		for _, s := range b.Workspace.Spec.MCP.Servers {
			if s.Name != "" && !slices.ContainsFunc(c.Tools.MCP, func(m fw.MCPServerConfig) bool { return m.Name == s.Name }) {
				c.Tools.MCP = append(c.Tools.MCP, fw.MCPServerConfig{Name: s.Name, Endpoint: s.Endpoint})
			}
		}
	}
	eps := axEndpoints(&c, os.Getenv)
	if len(eps) == 0 {
		return true, "ax reach: the configuration names no endpoint"
	}
	limit := min(timeout, axProbeLimit)
	notes := make([]string, len(eps))
	oks := make([]bool, len(eps))
	var wg sync.WaitGroup
	for i, e := range eps {
		wg.Go(func() { oks[i], notes[i] = probeAX(ctx, e, limit) })
	}
	wg.Wait()
	var good, bad []string
	for i, e := range eps {
		if oks[i] {
			good = append(good, e.what+" "+notes[i])
		} else {
			bad = append(bad, e.what+" "+notes[i])
		}
	}
	if len(bad) == 0 {
		return true, fmt.Sprintf("ax reach: %d %s through the egress gateway: %s", len(good), plural(len(good), "endpoint answers", "endpoints answer"), strings.Join(good, ", "))
	}
	line := fmt.Sprintf("ax reach: %d of %d endpoints %s not answer: %s; fix the egress of the Task (fw ax render derives a rule for each endpoint)",
		len(bad), len(eps), plural(len(bad), "does", "do"), strings.Join(bad, ", "))
	if len(good) > 0 {
		line += "; these answer: " + strings.Join(good, ", ")
	}
	return false, line
}

// probeAX dials one endpoint and returns whether it answers and a note
// that names it. An http endpoint gets one GET: any answer but the
// refusal of the egress gateway, a 403 with the body "egress denied",
// shows the way. A TLS endpoint gets a handshake, which the gateway ends
// when no tls rule covers the name. No probe sends a credential.
func probeAX(ctx context.Context, e axEndpoint, limit time.Duration) (bool, string) {
	a, err := parseAXURL(e.url)
	if err != nil {
		return false, fmt.Sprintf("(%v)", err)
	}
	name := a.addr()
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	switch a.kind {
	case "http":
		raw := e.url
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil {
			return false, fmt.Sprintf("%s (%v)", name, err)
		}
		u.Scheme, u.User = "http", nil // ws:// goes as http://, and no user information goes at all
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return false, fmt.Sprintf("%s (%v)", name, err)
		}
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			return false, fmt.Sprintf("%s (no answer: %v)", name, shortErr(err))
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			head, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
			if strings.HasPrefix(strings.TrimSpace(string(head)), "egress denied") {
				return false, name + " (the egress gateway denies it)"
			}
		}
		return true, name
	case "tls":
		d := &tls.Dialer{NetDialer: &net.Dialer{}, Config: &tls.Config{ServerName: a.host, MinVersion: tls.VersionTLS12}}
		conn, err := d.DialContext(ctx, "tcp", name)
		var verify *tls.CertificateVerificationError
		switch {
		case errors.As(err, &verify):
			return true, name + " (its certificate does not verify here)"
		case err != nil:
			return false, fmt.Sprintf("%s (the TLS handshake fails: %v)", name, shortErr(err))
		}
		_ = conn.Close()
		return true, name
	}
	return false, name + " (the egress gateway carries only HTTP and TLS)"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// shortErr drops the operation and the address that a dial error repeats.
func shortErr(err error) string {
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Err.Error()
	}
	return err.Error()
}

// doctorAXTrust reports the trust bundle of the egress gateway, which an
// https egress rule needs, because the gateway terminates its TLS. An
// absent bundle is a fact of the platform that nobody in the Task can
// change, so only a bundle without a certificate fails.
func doctorAXTrust(context.Context, *fw.Config, fw.Secrets, time.Duration) (bool, string) {
	data, err := os.ReadFile(axTrustBundle)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return true, fmt.Sprintf("ax trust bundle: absent at %s; https egress rules need the SystemInfo volume there, so use http and tls rules", axTrustBundle)
	case err != nil:
		return false, fmt.Sprintf("ax trust bundle: %s does not read: %v", axTrustBundle, err)
	}
	n := 0
	for {
		var b *pem.Block
		if b, data = pem.Decode(data); b == nil {
			break
		}
		if _, err := x509.ParseCertificate(b.Bytes); b.Type == "CERTIFICATE" && err == nil {
			n++
		}
	}
	if n == 0 {
		return false, fmt.Sprintf("ax trust bundle: %s holds no certificate; the SystemInfo volume of the Task names no trust bundle", axTrustBundle)
	}
	return true, fmt.Sprintf("ax trust bundle: present at %s with %d %s; https egress rules can work", axTrustBundle, n, plural(n, "certificate", "certificates"))
}

// doctorAXToolUser reports the user that runs the processes of tools, as
// ax.ApplyToConfig chooses it for fw serve, and whether it is another
// user than the one of fw.
func doctorAXToolUser(_ context.Context, cfg *fw.Config, _ fw.Secrets, _ time.Duration) (bool, string) {
	self := "uid " + strconv.Itoa(os.Geteuid())
	if u, err := user.LookupId(strconv.Itoa(os.Geteuid())); err == nil {
		self = fmt.Sprintf("%s (%s)", u.Username, u.Uid)
	}
	spec, why := strings.TrimSpace(cfg.Tools.User), "tools.user is none"
	missing := fw.MissingToolCaps()
	switch {
	case spec == "" && os.Geteuid() != 0:
		why = "fw does not run as root"
	case spec == "" && len(missing) > 0:
		why = "root lacks " + strings.Join(missing, ", ") + " in this sandbox"
	case spec == "":
		spec = ax.DefaultToolUser
	case spec != fw.ToolUserNone && os.Geteuid() == 0 && len(missing) > 0:
		return false, fmt.Sprintf("ax tools user: tools.user is %s, but root lacks %s in this sandbox, so fw stops at start; set tools.user: none", spec, strings.Join(missing, ", "))
	}
	u, err := fw.LookupToolUser(spec)
	switch {
	case err != nil:
		return false, "ax tools user: " + err.Error()
	case u == nil || int(u.UID) == os.Geteuid():
		if u != nil {
			why = "tools.user names that user"
		}
		return true, fmt.Sprintf("ax tools user: the tools run as %s, the user of fw, because %s; no separate user is in effect, so a tool process can read the files of fw", self, why)
	}
	return true, fmt.Sprintf("ax tools user: the tools run as %s, a separate user from %s, the user of fw", u, self)
}

// doctorAXDurable checks that the home of fw is writable on the durable
// volume, that the volume is a mount, and that every binding is on it,
// because only that volume survives a suspend.
func doctorAXDurable(ctx context.Context, cfg *fw.Config, _ fw.Secrets, timeout time.Duration) (bool, string) {
	var problems []string
	home, _ := filepath.Abs(cfg.Home)
	if !withinDir(home, axWorkspace) {
		problems = append(problems, fmt.Sprintf("FW_HOME %s is not under %s, so the ledger is lost at every suspend; unset FW_HOME", home, axWorkspace))
	} else if err := writable(home); err != nil {
		problems = append(problems, fmt.Sprintf("FW_HOME %s is not writable: %v", home, err))
	}
	mount := "the volume at " + axWorkspace
	switch ok, err := isMount(axWorkspace); {
	case err != nil:
		mount = fmt.Sprintf("%s (the mount table does not read: %v)", axWorkspace, err)
	case !ok:
		problems = append(problems, fmt.Sprintf("%s is not a mount, so the Task has no durable volume and loses its files at a suspend", axWorkspace))
	}
	task, wss, err := axManifests(ctx, timeout)
	if err != nil {
		problems = append(problems, err.Error())
	}
	bindings := ax.Bindings(task, wss)
	for _, b := range bindings {
		if !withinDir(b.Path, axWorkspace) {
			problems = append(problems, fmt.Sprintf("the binding %s at %s is not on the durable volume; bind it under %s", b.Ref.Name, b.Path, axWorkspace))
		}
	}
	if len(problems) > 0 {
		return false, "ax durable: " + strings.Join(problems, "; ")
	}
	return true, fmt.Sprintf("ax durable: FW_HOME %s is writable on %s, with %d %s on it", home, mount, len(bindings), plural(len(bindings), "binding", "bindings"))
}

// withinDir reports whether path is dir or below it.
func withinDir(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// isMount reports whether dir is the mount point of a mount in the mount
// table of this process.
func isMount(dir string) (bool, error) {
	data, err := os.ReadFile(axMountInfo)
	if err != nil {
		return false, err
	}
	dir = filepath.Clean(dir)
	for _, line := range strings.Split(string(data), "\n") {
		// The fifth field is the mount point, with a space as \040.
		if f := strings.Fields(line); len(f) > 4 && strings.ReplaceAll(f[4], `\040`, " ") == dir {
			return true, nil
		}
	}
	return false, nil
}
