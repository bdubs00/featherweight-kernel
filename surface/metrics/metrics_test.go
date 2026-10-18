package metrics_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/metrics"
	"github.com/bdubs00/featherweight-kernel/telemetry"
	"github.com/prometheus/client_golang/prometheus"
)

func newAgent(t *testing.T, cfg *fw.Config, s fw.Surface, opts ...fw.Option) *fw.Agent {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	if cfg.Model == "" {
		// The kernel has no default model; the series under test name this one.
		cfg.Model = "anthropic/claude-opus-5-5"
	}
	model := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text("ok")}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 3, OutputTokens: 2}}, nil
	}}
	a, err := fw.New(append([]fw.Option{
		fw.WithConfig(cfg),
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.TokenAuthn{Token: "s3cret"}),
		fw.WithModel(model),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		fw.WithSurfaces(s),
	}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

func scrape(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestServesRegistryWithoutToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := fw.DefaultConfig()
	cfg.Version = "9.9.9"
	s := metrics.New()
	a := newAgent(t, cfg, s)
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	if r, err := a.Run(ctx, "go"); err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	// The kernel notifies observers after Run sees the terminal state.
	var status int
	var body string
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		status, body = scrape(t, h, "/metrics")
		if strings.Contains(body, "fw_runs_total{") || time.Now().After(deadline) {
			break
		}
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	for _, want := range []string{
		`fw_runs_total{agent="default",batch="",outcome="completed"} 1`,
		`fw_model_tokens_total{kind="input",model="claude-opus-5-5",provider="anthropic"} 3`,
		`fw_build_info{commit=`,
		`go="` + runtime.Version() + `",version="9.9.9"} 1`,
		"go_goroutines ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape lacks %q", want)
		}
	}
	if runtime.GOOS == "linux" && !strings.Contains(body, "process_cpu_seconds_total") {
		t.Error("scrape lacks the process collector")
	}

	// A second Handler reuses the registry and observes each event once.
	h2, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	if _, body2 := scrape(t, h2, "/metrics"); !strings.Contains(body2, `fw_runs_total{agent="default",batch="",outcome="completed"} 1`) {
		t.Error("the second handler does not serve the same registry")
	}
	if s.Metrics() == nil || s.Registry() == nil {
		t.Fatal("the surface does not expose its metrics")
	}
	s.Metrics().SetMCPServerUp("git", true)
	if _, body3 := scrape(t, h, "/metrics"); !strings.Contains(body3, `fw_mcp_server_up{server="git"} 1`) {
		t.Error("SetMCPServerUp is not served")
	}

	// Another agent cannot share the surface.
	other := newAgent(t, fw.DefaultConfig(), metrics.New())
	if err := s.Mount(http.NewServeMux(), other); err == nil {
		t.Fatal("a surface mounted for a second agent")
	}
}

func TestCustomPath(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Telemetry.Metrics = "/internal/prom"
	a := newAgent(t, cfg, metrics.New())
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	if status, body := scrape(t, h, "/internal/prom"); status != http.StatusOK || !strings.Contains(body, "fw_build_info") {
		t.Fatalf("custom path = %d", status)
	}
	if status, _ := scrape(t, h, "/metrics"); status != http.StatusNotFound {
		t.Fatalf("/metrics = %d", status)
	}
	cfg.Telemetry.Metrics = "metrics"
	if err := metrics.New().Mount(http.NewServeMux(), a); err == nil {
		t.Fatal("a relative path mounted")
	}
}

func TestRegistered(t *testing.T) {
	s, err := fw.NewSurface(metrics.Name, fw.DefaultConfig())
	if err != nil || s == nil {
		t.Fatalf("NewSurface = %v, %v", s, err)
	}
}

// A collector that a linked package registers appears in the scrape; an
// agent without one serves none of its series.
func TestRegisteredCollector(t *testing.T) {
	a := newAgent(t, fw.DefaultConfig(), metrics.New())
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	if _, body := scrape(t, h, "/metrics"); strings.Contains(body, "fw_extra_") {
		t.Error("an agent without a registered collector reports its series")
	}
	metrics.RegisterCollector(func(*fw.Agent) prometheus.Collector {
		g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "fw_extra_up", Help: "A gauge of a linked package."})
		g.Set(1)
		return g
	})
	b := newAgent(t, fw.DefaultConfig(), metrics.New())
	h, err = b.Handler()
	if err != nil {
		t.Fatal(err)
	}
	if _, body := scrape(t, h, "/metrics"); !strings.Contains(body, "fw_extra_up 1") {
		t.Errorf("the registered collector is missing:\n%s", body)
	}
}

// recorder is a prometheus.Registerer that keeps the collectors.
type recorder struct{ cs []prometheus.Collector }

func (r *recorder) Register(c prometheus.Collector) error { r.cs = append(r.cs, c); return nil }

func (r *recorder) MustRegister(cs ...prometheus.Collector) { r.cs = append(r.cs, cs...) }

func (r *recorder) Unregister(prometheus.Collector) bool { return false }

// TestExamplesUseServedMetrics reads the alert rules and the dashboard of
// examples/observability. Every fw metric that they name must be one that
// the surface serves.
func TestExamplesUseServedMetrics(t *testing.T) {
	rec := &recorder{}
	telemetry.NewMetrics(rec, nil)
	// The workbench adds the fleet gauges through metrics.RegisterCollector,
	// and the examples name them, so the test knows them.
	served := map[string]bool{"fw_fleet_member_up": true, "fw_fleet_member_last_seen_timestamp_seconds": true, "fw_fleet_runs": true, "fw_fleet_approvals_oldest_timestamp_seconds": true}
	fqName := regexp.MustCompile(`fqName: "([^"]+)"`)
	histogram := map[string]bool{}
	for _, c := range rec.cs {
		ch := make(chan *prometheus.Desc, 64)
		c.Describe(ch)
		close(ch)
		for d := range ch {
			if m := fqName.FindStringSubmatch(d.String()); m != nil {
				served[m[1]] = true
			}
		}
		if _, ok := c.(prometheus.ObserverVec); ok {
			ch := make(chan *prometheus.Desc, 1)
			c.Describe(ch)
			histogram[fqName.FindStringSubmatch((<-ch).String())[1]] = true
		}
	}
	name := regexp.MustCompile(`\bfw_[a-z_]+\b`)
	for _, file := range []string{"alerts.yml", "grafana/fw.json", "README.md"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "examples", "observability", file))
		if err != nil {
			t.Fatal(err)
		}
		used := 0
		for _, n := range name.FindAllString(string(b), -1) {
			used++
			base := n
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				if trimmed, ok := strings.CutSuffix(n, suffix); ok && histogram[trimmed] {
					base = trimmed
				}
			}
			if !served[base] {
				t.Errorf("%s names %s, which the surface does not serve", file, n)
			}
		}
		if used == 0 {
			t.Errorf("%s names no fw metric", file)
		}
	}
}
