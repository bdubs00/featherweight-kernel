package cli

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/axmeta"
)

// axDoctorEnv fakes the environment of an ax Task the way the tests of
// package ax do: the Task and its Workspaces in the variables of the
// runner contract. The Task binds ws at bindPath.
func axDoctorEnv(t *testing.T, bindPath, mcpEndpoint string) {
	t.Helper()
	env(t)
	for _, n := range axOTLPNames {
		t.Setenv(n, "")
	}
	t.Setenv(ax.EnvMetadataURL, "")
	t.Setenv(ax.EnvTaskYAML, "apiVersion: ax.io/v1alpha1\nkind: Task\nmetadata: {name: reviewer, atespace: default}\nspec:\n  workspaces:\n    - {name: ws, path: "+bindPath+"}\n")
	ws := "apiVersion: ax.io/v1alpha1\nkind: Workspace\nmetadata: {name: ws}\nspec: {}\n"
	if mcpEndpoint != "" {
		ws = "apiVersion: ax.io/v1alpha1\nkind: Workspace\nmetadata: {name: ws}\nspec:\n  mcp:\n    servers:\n      - {name: tickets, endpoint: " + mcpEndpoint + "}\n"
	}
	t.Setenv(ax.EnvWorkspacesYAML, ws)
}

// The checks of a Task run under ax only; elsewhere one line says so.
func TestAXDoctorChecks(t *testing.T) {
	axDoctorEnv(t, "/workspace", "")
	if !underAX() {
		t.Error("AX_TASK_YAML does not count as ax")
	}
	t.Setenv(ax.EnvTaskYAML, "")
	if underAX() {
		t.Error("no ax variable counts as ax")
	}
	t.Setenv(ax.EnvMetadataURL, "http://127.0.0.1:8099")
	if !underAX() {
		t.Error("AX_METADATA_URL does not count as ax")
	}
	if n := len(axDoctorChecks(true)); n != 4 {
		t.Errorf("%d checks under ax, want 4", n)
	}
	off := axDoctorChecks(false)
	if len(off) != 1 {
		t.Fatalf("%d checks outside ax, want 1", len(off))
	}
	if ok, line := off[0](context.Background(), fw.DefaultConfig(), nil, time.Second); !ok || !strings.HasPrefix(line, "ax: not under ax") {
		t.Errorf("outside ax: %v %q", ok, line)
	}
}

// The reach line probes each endpoint with a short timeout: one that
// answers, one that the egress gateway denies, one that does not answer,
// a TLS endpoint, an MCP server of the Workspace, and a bus that the
// gateway cannot carry.
func TestDoctorAXReach(t *testing.T) {
	answering := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("a probe sent a credential")
		}
		http.NotFound(w, r)
	}))
	defer answering.Close()
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "egress denied", http.StatusForbidden)
	}))
	defer denied.Close()
	secure := httptest.NewUnstartedServer(http.NotFoundHandler())
	secure.Config.ErrorLog = log.New(io.Discard, "", 0) // the probe refuses the certificate of the test
	secure.StartTLS()
	defer secure.Close()
	closed := closedAddr(t)
	axDoctorEnv(t, "/workspace", answering.URL+"/mcp")

	cfg := fw.DefaultConfig()
	cfg.Model = "openai/qwen"
	cfg.Providers = map[string]fw.ProviderConfig{"openai": {BaseURL: answering.URL + "/v1"}}
	cfg.Bus = fw.BusConfig{Driver: "nats", URL: "nats://127.0.0.1:4222," + strings.Replace(answering.URL, "http://", "ws://user:bus-secret@", 1)}
	cfg.Tokens.URL = "http://" + closed
	cfg.Tools.MCP = []fw.MCPServerConfig{{Name: "github", Endpoint: denied.URL}, {Name: "secure", Endpoint: secure.URL}}
	host := func(u string) string { return strings.TrimPrefix(strings.TrimPrefix(u, "http://"), "https://") }

	ok, line := doctorAXReach(context.Background(), cfg, nil, 2*time.Second)
	if ok || strings.Contains(line, "bus-secret") {
		t.Errorf("the line passed or holds a secret: %s", line)
	}
	for _, want := range []string{
		"ax reach: 3 of 7 endpoints do not answer: ",
		"bus " + host(answering.URL) + ", ",
		"bus 127.0.0.1:4222 (the egress gateway carries only HTTP and TLS)",
		"tokens " + closed + " (no answer: ",
		"mcp github " + host(denied.URL) + " (the egress gateway denies it)",
		"fw ax render derives a rule for each endpoint",
		"; these answer: model openai " + host(answering.URL),
		"mcp secure " + host(secure.URL) + " (its certificate does not verify here)",
		"mcp tickets " + host(answering.URL),
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the line lacks %q:\n%s", want, line)
		}
	}

	cfg.Bus, cfg.Tokens.URL, cfg.Tools.MCP = fw.BusConfig{Driver: "mem"}, "", nil
	t.Setenv(ax.EnvWorkspacesYAML, "")
	if ok, line := doctorAXReach(context.Background(), cfg, nil, 2*time.Second); !ok || line != "ax reach: 1 endpoint answers through the egress gateway: model openai "+host(answering.URL) {
		t.Errorf("one endpoint: %v %q", ok, line)
	}
	if ok, line := doctorAXReach(context.Background(), fw.DefaultConfig(), nil, time.Second); !ok || line != "ax reach: the configuration names no endpoint" {
		t.Errorf("no endpoint: %v %q", ok, line)
	}
}

// Under ax ssh without AX_TASK_YAML, the checks read the manifests from
// the metadata endpoints of the kernel.
func TestAXManifestsFromMetadata(t *testing.T) {
	axDoctorEnv(t, "/workspace", "")
	taskYAML := os.Getenv(ax.EnvTaskYAML)
	meta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case axmeta.PathTask:
			_, _ = w.Write([]byte(taskYAML))
		default:
			http.NotFound(w, r)
		}
	}))
	defer meta.Close()
	t.Setenv(ax.EnvTaskYAML, "")
	t.Setenv(ax.EnvMetadataURL, meta.URL)
	task, wss, err := axManifests(context.Background(), time.Second)
	if err != nil || task == nil || task.Metadata.Name != "reviewer" || len(wss) != 0 {
		t.Errorf("manifests %+v %v %v", task, wss, err)
	}
}

// The trust bundle line says absent and names the path until a SystemInfo
// volume puts the bundle there. Only a bundle without a certificate fails.
func TestDoctorAXTrust(t *testing.T) {
	dir := t.TempDir()
	prev := axTrustBundle
	axTrustBundle = filepath.Join(dir, "egress-trust-bundle", "trust-bundle.pem")
	t.Cleanup(func() { axTrustBundle = prev })

	ok, line := doctorAXTrust(context.Background(), nil, nil, time.Second)
	if !ok || line != "ax trust bundle: absent at "+axTrustBundle+"; https egress rules need the SystemInfo volume there, so use http and tls rules" {
		t.Errorf("absent: %v %q", ok, line)
	}
	if err := os.MkdirAll(filepath.Dir(axTrustBundle), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(axTrustBundle, []byte("not a bundle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, line := doctorAXTrust(context.Background(), nil, nil, time.Second); ok || !strings.Contains(line, "holds no certificate") {
		t.Errorf("garbage: %v %q", ok, line)
	}
	cert, _ := writeCert(t, dir, time.Now().Add(time.Hour))
	data, err := os.ReadFile(cert)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(axTrustBundle, append(data, data...), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, line := doctorAXTrust(context.Background(), nil, nil, time.Second); !ok || line != "ax trust bundle: present at "+axTrustBundle+" with 2 certificates; https egress rules can work" {
		t.Errorf("present: %v %q", ok, line)
	}
}

// The tool user line names the user of the tools and whether it is a
// separate user from the one of fw.
func TestDoctorAXToolUser(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Tools.User = "none"
	ok, line := doctorAXToolUser(context.Background(), cfg, nil, time.Second)
	if !ok || !strings.HasPrefix(line, "ax tools user: the tools run as ") || !strings.Contains(line, ", the user of fw, because tools.user is none; no separate user is in effect") {
		t.Errorf("none: %v %q", ok, line)
	}
	if os.Geteuid() == 0 {
		t.Skip("the rest needs a user that is not root")
	}
	cfg.Tools.User = ""
	if ok, line := doctorAXToolUser(context.Background(), cfg, nil, time.Second); !ok || !strings.Contains(line, "because fw does not run as root") {
		t.Errorf("not root: %v %q", ok, line)
	}
	cfg.Tools.User = "fw-no-such-user"
	if ok, line := doctorAXToolUser(context.Background(), cfg, nil, time.Second); ok || !strings.Contains(line, "fw-no-such-user") {
		t.Errorf("unknown user: %v %q", ok, line)
	}
}

// The durable line checks that FW_HOME is writable under the volume, that
// the volume is a mount, and that every binding is on it.
func TestDoctorAXDurable(t *testing.T) {
	vol := t.TempDir()
	mounts := filepath.Join(t.TempDir(), "mountinfo")
	prevWS, prevMounts := axWorkspace, axMountInfo
	axWorkspace, axMountInfo = vol, mounts
	t.Cleanup(func() { axWorkspace, axMountInfo = prevWS, prevMounts })
	writeMounts := func(points ...string) {
		var b strings.Builder
		b.WriteString("25 1 0:21 / / rw,relatime - overlay overlay rw\n")
		for i, p := range points {
			b.WriteString(strings.Join([]string{"3" + string(rune('0'+i)), "25", "0:32", "/", strings.ReplaceAll(p, " ", `\040`), "rw,relatime", "-", "ext4", "/dev/sda1", "rw"}, " ") + "\n")
		}
		if err := os.WriteFile(mounts, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	axDoctorEnv(t, filepath.Join(vol, "repo"), "")
	writeMounts(vol)
	cfg := fw.DefaultConfig()
	cfg.Home = filepath.Join(vol, ".fw")

	ok, line := doctorAXDurable(context.Background(), cfg, nil, time.Second)
	if !ok || line != "ax durable: FW_HOME "+cfg.Home+" is writable on the volume at "+vol+", with 1 binding on it" {
		t.Errorf("good: %v %q", ok, line)
	}

	writeMounts("/other")
	cfg.Home = filepath.Join(t.TempDir(), ".fw")
	axDoctorEnv(t, "/elsewhere", "")
	ok, line = doctorAXDurable(context.Background(), cfg, nil, time.Second)
	for _, want := range []string{
		"ax durable: FW_HOME " + cfg.Home + " is not under " + vol + ", so the ledger is lost at every suspend; unset FW_HOME",
		vol + " is not a mount, so the Task has no durable volume",
		"the binding ws at /elsewhere is not on the durable volume; bind it under " + vol,
	} {
		if ok || !strings.Contains(line, want) {
			t.Errorf("bad: %v, the line lacks %q:\n%s", ok, want, line)
		}
	}
}
