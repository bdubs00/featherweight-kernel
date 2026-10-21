package telemetry

// This file exports what a process knows to an OpenTelemetry collector over
// OTLP (docs/ARCHITECTURE.md section 10.3): the metrics of its Prometheus
// registry, pushed at an interval; each ledger event as a log record; and
// the traces of its runs. The standard OTEL_EXPORTER_OTLP_* variables turn
// each signal on and point it at the collector, so a Task that sleeps
// stops pushing and no scrape wakes it. Export is the fw.Service that
// starts all three for an agent.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/prometheus/client_golang/prometheus"
	bridge "go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// ServiceName is the service.name of the exports of fw. OTEL_SERVICE_NAME
// overrides it.
const ServiceName = "fw"

// Signals of the export, as the OTEL_EXPORTER_OTLP_<SIGNAL>_ENDPOINT
// variables name them.
const (
	SignalTraces  = "TRACES"
	SignalMetrics = "METRICS"
	SignalLogs    = "LOGS"
)

// Exporting reports whether the variables name a collector for a signal:
// OTEL_EXPORTER_OTLP_ENDPOINT for every signal, or the variable of the
// signal alone.
func Exporting(signal string) bool {
	return strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != "" ||
		strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT")) != ""
}

// Endpoint returns the collector that a signal goes to, for a report.
func Endpoint(signal string) string {
	if v := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_" + signal + "_ENDPOINT")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
}

// newResource builds the resource of the process: the service name, the
// version, and the instance id. OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES win, because the options apply in order.
func newResource(ctx context.Context, serviceName, instance string) (*resource.Resource, error) {
	version, _ := buildVersion()
	attrs := []resource.Option{resource.WithAttributes(semconv.ServiceName(serviceName), semconv.ServiceVersion(version))}
	if instance != "" {
		attrs = append(attrs, resource.WithAttributes(semconv.ServiceInstanceID(instance)))
	}
	attrs = append(attrs, resource.WithTelemetrySDK(), resource.WithFromEnv())
	res, err := resource.New(ctx, attrs...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: resource: %w", err)
	}
	return res, nil
}

// NewMetricsPush pushes the metrics of a Prometheus registry to the
// collector over OTLP HTTP, at the interval of OTEL_METRIC_EXPORT_INTERVAL,
// 60 seconds by default. The series keep their Prometheus names. The
// returned function flushes and stops the push.
func NewMetricsPush(ctx context.Context, reg prometheus.Gatherer, serviceName, instance string) (func(context.Context) error, error) {
	res, err := newResource(ctx, serviceName, instance)
	if err != nil {
		return nil, err
	}
	exp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("telemetry: otlp metric exporter: %w", err)
	}
	producer := bridge.NewMetricProducer(bridge.WithGatherer(reg))
	reader := sdkmetric.NewPeriodicReader(exp, sdkmetric.WithProducer(producer))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(res))
	return func(ctx context.Context) error {
		if err := errors.Join(mp.ForceFlush(ctx), mp.Shutdown(ctx)); err != nil {
			return fmt.Errorf("telemetry: stop the metrics push: %w", err)
		}
		return nil
	}, nil
}

// Logs sends each ledger event to the collector as a log record. It
// implements fw.Observer. The body is the data of the event, the
// attributes are its envelope, and the record carries the trace of the run,
// so a trace store and a log store meet on the same ids.
type Logs struct {
	logger log.Logger
}

// NewLogs builds the log export over OTLP HTTP. The returned function
// flushes and stops it.
func NewLogs(ctx context.Context, serviceName, instance string) (*Logs, func(context.Context) error, error) {
	res, err := newResource(ctx, serviceName, instance)
	if err != nil {
		return nil, nil, err
	}
	exp, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("telemetry: otlp log exporter: %w", err)
	}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)), sdklog.WithResource(res))
	shutdown := func(ctx context.Context) error {
		if err := errors.Join(lp.ForceFlush(ctx), lp.Shutdown(ctx)); err != nil {
			return fmt.Errorf("telemetry: stop the log export: %w", err)
		}
		return nil
	}
	return NewLogsFrom(lp), shutdown, nil
}

// NewLogsFrom builds the log export on a logger provider that the caller
// owns.
func NewLogsFrom(lp log.LoggerProvider) *Logs {
	return &Logs{logger: lp.Logger(instrumentationName)}
}

// Observe implements fw.Observer.
func (l *Logs) Observe(e fw.Event) {
	var r log.Record
	if !e.Time.IsZero() {
		r.SetTimestamp(e.Time)
	}
	r.SetObservedTimestamp(time.Now())
	r.SetEventName(e.Type)
	sev, text := severityOf(e)
	r.SetSeverity(sev)
	r.SetSeverityText(text)
	if len(e.Data) > 0 {
		r.SetBody(attribute.StringValue(string(e.Data)))
	}
	r.AddAttributes(
		attribute.String("fw.run", e.Subject),
		attribute.String("fw.event", e.Type),
		attribute.Int64("fw.seq", int64(e.Seq)),
		attribute.String("fw.agent", agentOf(e.Source)),
	)
	r.AddAttributes(qualityAttributes(e)...)
	if e.Node != "" {
		r.AddAttributes(attribute.String("fw.node", e.Node))
	}
	if e.Source != "" {
		r.AddAttributes(attribute.String("fw.source", e.Source))
	}
	ctx := context.Background()
	if sc, ok := spanContextOf(e.TraceParent); ok {
		ctx = trace.ContextWithSpanContext(ctx, sc)
	}
	l.logger.Emit(ctx, r)
}

// severityOf grades an event: a failed run and a budget stop are errors, a
// loop and a suspend are warnings, and the rest is information.
func severityOf(e fw.Event) (log.Severity, string) {
	switch e.Type {
	case fw.EvRunFinished:
		var d fw.RunFinishedData
		if e.Decode(&d) == nil && d.Outcome == fw.OutcomeFailed {
			return log.SeverityError, "ERROR"
		}
	case fw.EvBudgetExceeded:
		return log.SeverityError, "ERROR"
	case fw.EvLoopDetected, fw.EvRunSuspended:
		return log.SeverityWarn, "WARN"
	}
	return log.SeverityInfo, "INFO"
}

// spanContextOf reads a W3C traceparent, "00-<trace id>-<span id>-<flags>".
func spanContextOf(traceparent string) (trace.SpanContext, bool) {
	parts := strings.Split(strings.TrimSpace(traceparent), "-")
	if len(parts) != 4 {
		return trace.SpanContext{}, false
	}
	tid, err := trace.TraceIDFromHex(parts[1])
	if err != nil {
		return trace.SpanContext{}, false
	}
	sid, err := trace.SpanIDFromHex(parts[2])
	if err != nil {
		return trace.SpanContext{}, false
	}
	flags := trace.TraceFlags(0)
	if len(parts[3]) == 2 && parts[3] != "00" {
		flags = trace.FlagsSampled
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: flags, Remote: true})
	return sc, sc.IsValid()
}

func init() {
	fw.RegisterService(Export)
}

// registrar is the metrics surface: it holds the Prometheus registry once
// the surface mounts.
type registrar interface {
	Registry() *prometheus.Registry
}

// Export is the fw.Service of the export. For each signal that the
// variables name, it starts the export of the agent: the traces and the
// logs at once, and the metrics as soon as the metrics surface mounts and
// its registry exists. Without any variable, it starts nothing.
func Export(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	ex := &export{a: a}
	instance := a.Node()
	if Exporting(SignalTraces) {
		t, stop, err := NewTracer(ctx, ServiceName)
		if err != nil {
			return nil, err
		}
		a.Observe(t)
		ex.stops = append(ex.stops, stop)
	}
	if Exporting(SignalLogs) {
		l, stop, err := NewLogs(ctx, ServiceName, instance)
		if err != nil {
			ex.close()
			return nil, err
		}
		a.Observe(l)
		ex.stops = append(ex.stops, stop)
	}
	if Exporting(SignalMetrics) {
		ex.wait = make(chan struct{})
		go ex.metrics(instance)
	}
	if len(ex.stops) == 0 && ex.wait == nil {
		return nil, nil
	}
	a.Logger().Info("telemetry exports over OTLP", "traces", Exporting(SignalTraces), "logs", Exporting(SignalLogs), "metrics", Exporting(SignalMetrics), "endpoint", Endpoint(SignalMetrics))
	return ex, nil
}

// export is the running export of one agent.
type export struct {
	a    *fw.Agent
	wait chan struct{} // closed by Close, ends the wait for the registry

	mu    sync.Mutex
	stops []func(context.Context) error
}

// metrics waits for the registry of the metrics surface and then starts
// the push. The surface mounts when the agent serves, after the services
// start.
func (ex *export) metrics(instance string) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		for _, s := range ex.a.Surfaces() {
			if r, ok := s.(registrar); ok {
				if reg := r.Registry(); reg != nil {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					stop, err := NewMetricsPush(ctx, reg, ServiceName, instance)
					cancel()
					if err != nil {
						ex.a.Logger().Warn("the metrics push did not start", "error", err)
						return
					}
					ex.mu.Lock()
					ex.stops = append(ex.stops, stop)
					ex.mu.Unlock()
					return
				}
			}
		}
		select {
		case <-ex.wait:
			return
		case <-t.C:
		}
	}
}

// Close flushes and stops every export.
func (ex *export) Close() error {
	if ex.wait != nil {
		select {
		case <-ex.wait:
		default:
			close(ex.wait)
		}
	}
	return ex.close()
}

func (ex *export) close() error {
	ex.mu.Lock()
	stops := ex.stops
	ex.stops = nil
	ex.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var errs []error
	for i := len(stops) - 1; i >= 0; i-- {
		errs = append(errs, stops[i](ctx))
	}
	return errors.Join(errs...)
}
