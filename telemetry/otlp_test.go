package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// recorder keeps the records that the SDK exports.
type recorder struct {
	mu   sync.Mutex
	recs []sdklog.Record
}

func (r *recorder) Export(_ context.Context, recs []sdklog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, recs...)
	return nil
}

func (r *recorder) Shutdown(context.Context) error   { return nil }
func (r *recorder) ForceFlush(context.Context) error { return nil }

func attrsOf(rec sdklog.Record) map[string]string {
	out := map[string]string{}
	rec.WalkAttributes(func(kv attribute.KeyValue) bool {
		out[string(kv.Key)] = kv.Value.Emit()
		return true
	})
	return out
}

func TestLogsCarryTheEvent(t *testing.T) {
	rec := &recorder{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(rec)))
	l := NewLogsFrom(lp)
	tp := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	when := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	fin := fw.MustEvent("01RUN", fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "boom"})
	fin.Seq, fin.Time, fin.Node, fin.Source, fin.TraceParent = 7, when, "ax:team/alpha", "fw://team/reviewer/01RUN", tp
	l.Observe(fin)
	started := fw.MustEvent("01RUN", fw.EvRunStarted, fw.RunStartedData{})
	l.Observe(started)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.recs) != 2 {
		t.Fatalf("records = %d", len(rec.recs))
	}
	r := rec.recs[0]
	at := attrsOf(r)
	if r.EventName() != fw.EvRunFinished || r.Severity() != log.SeverityError || !r.Timestamp().Equal(when) ||
		at["fw.run"] != "01RUN" || at["fw.seq"] != "7" || at["fw.node"] != "ax:team/alpha" || at["fw.agent"] != "reviewer" || at["fw.event"] != fw.EvRunFinished {
		t.Fatalf("record = %s %v %v %v", r.EventName(), r.Severity(), r.Timestamp(), at)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(r.Body().AsString()), &body); err != nil || body["error"] != "boom" {
		t.Fatalf("body = %q", r.Body().AsString())
	}
	if r.TraceID().String() != "0af7651916cd43dd8448eb211c80319c" || r.SpanID().String() != "b7ad6b7169203331" {
		t.Fatalf("trace = %s %s", r.TraceID(), r.SpanID())
	}
	if rec.recs[1].Severity() != log.SeverityInfo || rec.recs[1].TraceID().IsValid() {
		t.Fatalf("plain record = %v %s", rec.recs[1].Severity(), rec.recs[1].TraceID())
	}
}

func TestSpanContextOf(t *testing.T) {
	sc, ok := spanContextOf("00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	if !ok || !sc.IsSampled() || !sc.IsRemote() {
		t.Fatalf("spanContextOf = %+v %v", sc, ok)
	}
	for _, bad := range []string{"", "00-xyz-b7ad6b7169203331-01", "00-0af7651916cd43dd8448eb211c80319c-0000000000000000-01"} {
		if _, ok := spanContextOf(bad); ok {
			t.Fatalf("spanContextOf(%q) accepted", bad)
		}
	}
	if _, ok := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c"); ok != nil {
		t.Fatal(ok)
	}
}

// collector is an OTLP HTTP endpoint that counts what arrives.
type collector struct {
	*httptest.Server
	mu      sync.Mutex
	metrics []string // metric names
	logs    int
}

func newCollector(t *testing.T) *collector {
	t.Helper()
	c := &collector{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req colmetrics.ExportMetricsServiceRequest
		if err := proto.Unmarshal(b, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		for _, rm := range req.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					c.metrics = append(c.metrics, m.Name)
				}
			}
		}
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		out, _ := proto.Marshal(&colmetrics.ExportMetricsServiceResponse{})
		_, _ = w.Write(out)
	})
	mux.HandleFunc("POST /v1/logs", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req collogs.ExportLogsServiceRequest
		if err := proto.Unmarshal(b, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				c.logs += len(sl.LogRecords)
			}
		}
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		out, _ := proto.Marshal(&collogs.ExportLogsServiceResponse{})
		_, _ = w.Write(out)
	})
	c.Server = httptest.NewServer(mux)
	t.Cleanup(c.Close)
	return c
}

func TestMetricsPushAndLogsReachTheCollector(t *testing.T) {
	c := newCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", c.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "100")
	if !Exporting(SignalMetrics) || Endpoint(SignalLogs) != c.URL {
		t.Fatal("Exporting or Endpoint misread the variables")
	}
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	f := newFeed(m, "01RUN", "default")
	f.emit(fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Origin: fw.OriginHTTP})
	f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	ctx := context.Background()
	stop, err := NewMetricsPush(ctx, reg, ServiceName, "box:8080")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		names := strings.Join(c.metrics, " ")
		c.mu.Unlock()
		if strings.Contains(names, "fw_runs_total") || time.Now().After(deadline) {
			if !strings.Contains(names, "fw_runs_total") {
				t.Fatalf("the collector did not get fw_runs_total: %q", names)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := stop(ctx); err != nil {
		t.Fatal(err)
	}
	l, stopLogs, err := NewLogs(ctx, ServiceName, "box:8080")
	if err != nil {
		t.Fatal(err)
	}
	l.Observe(fw.MustEvent("01RUN", fw.EvRunStarted, fw.RunStartedData{}))
	if err := stopLogs(ctx); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	logs := c.logs
	c.mu.Unlock()
	if logs != 1 {
		t.Fatalf("the collector got %d log records, want 1", logs)
	}
}

func TestExportingOff(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "http://collector:4318/v1/logs")
	if Exporting(SignalMetrics) || !Exporting(SignalLogs) || Endpoint(SignalLogs) != "http://collector:4318/v1/logs" {
		t.Fatal("Exporting misread the variables of one signal")
	}
}
