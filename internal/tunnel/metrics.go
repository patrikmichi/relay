package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// metricPrefix names the connector's Prometheus metrics after the binary.
var metricPrefix = strings.ReplaceAll(BinaryName, "-", "_")

// Metrics are the counters behind /metrics.
type Metrics struct {
	mu         sync.Mutex
	exchanges  map[[2]string]int64
	durSum     map[string]float64
	durCount   map[string]int64
	upSum      map[string]float64
	reconnects atomic.Int64
	violations atomic.Int64
	security   atomic.Int64
}

func newMetrics() *Metrics {
	return &Metrics{exchanges: map[[2]string]int64{}, durSum: map[string]float64{}, durCount: map[string]int64{}, upSum: map[string]float64{}}
}

func (m *Metrics) exchange(target, outcome string, d, up time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exchanges[[2]string{target, outcome}]++
	m.durSum[target] += d.Seconds()
	m.durCount[target]++
	m.upSum[target] += up.Seconds()
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// WritePrometheus writes the metrics in the Prometheus text format.
func (a *Agent) WritePrometheus(w io.Writer) {
	m := a.metrics
	p := metricPrefix
	connected := 0
	if a.connected.Load() {
		connected = 1
	}
	m.mu.Lock()
	keys := make([][2]string, 0, len(m.exchanges))
	for k := range m.exchanges {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	fmt.Fprintf(w, "# TYPE %s_exchanges_total counter\n", p)
	for _, k := range keys {
		fmt.Fprintf(w, "%s_exchanges_total{target=\"%s\",outcome=\"%s\"} %d\n", p, escapeLabel(k[0]), escapeLabel(k[1]), m.exchanges[k])
	}
	targets := make([]string, 0, len(m.durSum))
	for t := range m.durSum {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	fmt.Fprintf(w, "# TYPE %s_exchange_duration_seconds summary\n", p)
	for _, t := range targets {
		fmt.Fprintf(w, "%s_exchange_duration_seconds_sum{target=\"%s\"} %g\n%s_exchange_duration_seconds_count{target=\"%s\"} %d\n", p, escapeLabel(t), m.durSum[t], p, escapeLabel(t), m.durCount[t])
	}
	fmt.Fprintf(w, "# TYPE %s_upstream_duration_seconds summary\n", p)
	for _, t := range targets {
		fmt.Fprintf(w, "%s_upstream_duration_seconds_sum{target=\"%s\"} %g\n%s_upstream_duration_seconds_count{target=\"%s\"} %d\n", p, escapeLabel(t), m.upSum[t], p, escapeLabel(t), m.durCount[t])
	}
	m.mu.Unlock()
	fmt.Fprintf(w, "# TYPE %s_connected gauge\n%s_connected %d\n", p, p, connected)
	fmt.Fprintf(w, "# TYPE %s_reconnects_total counter\n%s_reconnects_total %d\n", p, p, m.reconnects.Load())
	fmt.Fprintf(w, "# TYPE %s_policy_violations_total counter\n%s_policy_violations_total %d\n", p, p, m.violations.Load())
	fmt.Fprintf(w, "# TYPE %s_security_events_total counter\n%s_security_events_total %d\n", p, p, m.security.Load())
	fmt.Fprintf(w, "# TYPE %s_inflight gauge\n%s_inflight %d\n", p, p, a.inflight.Load())
	fmt.Fprintf(w, "# TYPE %s_build_info gauge\n%s_build_info{version=\"%s\",proto=\"%d\"} 1\n", p, p, escapeLabel(Version), ProtoVersion)
}

// HealthHandler serves /healthz (the process is alive), /readyz (connected to
// the broker with a valid config) and /metrics.
func (a *Agent) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if a.ready.Load() && a.connected.Load() {
			_, _ = io.WriteString(w, "ready\n")
			return
		}
		http.Error(w, "not connected to the broker", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		a.WritePrometheus(w)
	})
	return mux
}

// HealthAddr is the address the config asks the health endpoint to listen on.
func (a *Agent) HealthAddr() string { return a.config().Health.Listen }

// ServeHealth listens on addr until ctx ends. A non-loopback address is used
// only because the config says so explicitly.
func (a *Agent) ServeHealth(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: a.HealthHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func osHostname() (string, error) { return os.Hostname() }
