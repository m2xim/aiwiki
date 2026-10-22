package server

import (
	"bufio"
	"database/sql"
	"fmt"
	"io"
	"math"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Version is the build version reported by aiwiki_build_info. It can be set at
// link time (-ldflags "-X aiwiki/internal/server.Version=1.2.3"); otherwise the
// module version recorded by the Go toolchain is used.
var Version = ""

// histogramBuckets are the upper bounds in seconds; +Inf is implicit.
var histogramBuckets = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// maxSeries bounds every labelled family, so a bug that leaks an unbounded
// label value can never grow memory without limit.
const maxSeries = 2000

var processStart = time.Now()

type counter struct{ n atomic.Uint64 }

type histogram struct {
	buckets [len(histogramBuckets) + 1]atomic.Uint64 // non-cumulative, last is +Inf
	count   atomic.Uint64
	sumNs   atomic.Uint64
}

func (h *histogram) observe(d time.Duration) {
	if d < 0 {
		d = 0
	}
	sec := d.Seconds()
	i := 0
	for i < len(histogramBuckets) && sec > histogramBuckets[i] {
		i++
	}
	h.buckets[i].Add(1)
	h.count.Add(1)
	h.sumNs.Add(uint64(d))
}

// family is a mutex-protected map of series keyed by the bounded label set.
type family[K comparable, V any] struct {
	mu sync.RWMutex
	m  map[K]*V
}

func (f *family[K, V]) get(k K, overflow K) *V {
	f.mu.RLock()
	v := f.m[k]
	f.mu.RUnlock()
	if v != nil {
		return v
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if v = f.m[k]; v != nil {
		return v
	}
	if f.m == nil {
		f.m = map[K]*V{}
	}
	if len(f.m) >= maxSeries {
		k = overflow
		if v = f.m[k]; v != nil {
			return v
		}
	}
	v = new(V)
	f.m[k] = v
	return v
}

func (f *family[K, V]) snapshot() map[K]*V {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[K]*V, len(f.m))
	for k, v := range f.m {
		out[k] = v
	}
	return out
}

type requestKey struct{ route, method, code string }
type durationKey struct{ route, method string }

// Fixed label sets.
var (
	rateLimiterNames = []string{"artifact_frame", "artifact_public", "artifact_token", "mcp_oauth"}
	loginOutcomes    = []string{"denied", "expired", "failed", "ok", "unavailable"}
	knownMethods     = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "OPTIONS": true}
)

type metrics struct {
	requests  family[requestKey, counter]
	durations family[durationKey, histogram]
	inFlight  atomic.Int64
	toolCalls family[[2]string, counter]
	toolTimes family[string, histogram]
	limited   [4]counter
	logins    [5]counter
}

func newMetrics() *metrics { return &metrics{} }

// defaultMetrics collects process-wide counters; hooks in code that has no
// access to a Server (limiters, login callback) write here.
var defaultMetrics = newMetrics()

func normalizeMethod(m string) string {
	if knownMethods[m] {
		return m
	}
	return "OTHER"
}

func statusClass(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 100 && code < 200:
		return "2xx" // an interim status is never the final one; keep the set closed
	}
	return "5xx"
}

func (m *metrics) observeRequest(route, method string, code int, d time.Duration) {
	method = normalizeMethod(method)
	m.requests.get(requestKey{route, method, statusClass(code)}, requestKey{"other", method, statusClass(code)}).n.Add(1)
	m.durations.get(durationKey{route, method}, durationKey{"other", method}).observe(d)
}

func (m *metrics) rateLimited(limiter string) {
	for i, n := range rateLimiterNames {
		if n == limiter {
			m.limited[i].n.Add(1)
			return
		}
	}
}

func (m *metrics) login(outcome string) {
	for i, n := range loginOutcomes {
		if n == outcome {
			m.logins[i].n.Add(1)
			return
		}
	}
}

var (
	mcpToolNamesOnce sync.Once
	mcpToolNameSet   map[string]bool
)

// mcpMetricTool returns name when it is a registered tool, otherwise "unknown".
func mcpMetricTool(name string) string {
	mcpToolNamesOnce.Do(func() {
		mcpToolNameSet = map[string]bool{}
		for _, t := range mcpToolList() {
			if n, ok := t["name"].(string); ok {
				mcpToolNameSet[n] = true
			}
		}
	})
	if mcpToolNameSet[name] {
		return name
	}
	return "unknown"
}

func (m *metrics) observeTool(name string, isErr bool, d time.Duration) {
	tool := mcpMetricTool(name)
	outcome := "ok"
	if isErr {
		outcome = "error"
	}
	m.toolCalls.get([2]string{tool, outcome}, [2]string{"unknown", outcome}).n.Add(1)
	m.toolTimes.get(tool, "unknown").observe(d)
}

// ---- exposition (Prometheus text format 0.0.4) ----

type metricsSource struct {
	mode  string
	pools map[string]*sql.DB
	ready float64
}

func labelEscape(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func labels(kv ...string) string {
	if len(kv) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(kv[i])
		b.WriteString(`="`)
		b.WriteString(labelEscape(kv[i+1]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func fmtFloat(f float64) string {
	if math.IsInf(f, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func buildVersion() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func writeHistogram(w io.Writer, name string, kv []string, h *histogram) {
	with := func(le string) string {
		return labels(append(append([]string{}, kv...), "le", le)...)
	}
	var cum uint64
	for i, le := range histogramBuckets {
		cum += h.buckets[i].Load()
		fmt.Fprintf(w, "%s_bucket%s %d\n", name, with(fmtFloat(le)), cum)
	}
	cum += h.buckets[len(histogramBuckets)].Load()
	fmt.Fprintf(w, "%s_bucket%s %d\n", name, with("+Inf"), cum)
	fmt.Fprintf(w, "%s_sum%s %s\n", name, labels(kv...), fmtFloat(float64(h.sumNs.Load())/1e9))
	fmt.Fprintf(w, "%s_count%s %d\n", name, labels(kv...), h.count.Load())
}

func (m *metrics) write(out io.Writer, src metricsSource) {
	w := bufio.NewWriter(out)
	defer w.Flush()
	head := func(name, typ, help string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}

	head("aiwiki_build_info", "gauge", "Build information.")
	fmt.Fprintf(w, "aiwiki_build_info%s 1\n", labels("version", buildVersion(), "go_version", runtime.Version(), "mode", src.mode))

	head("aiwiki_http_requests_total", "counter", "HTTP requests by route pattern, method and status class.")
	reqs := m.requests.snapshot()
	keys := make([]requestKey, 0, len(reqs))
	for k := range reqs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.route != b.route {
			return a.route < b.route
		}
		if a.method != b.method {
			return a.method < b.method
		}
		return a.code < b.code
	})
	for _, k := range keys {
		fmt.Fprintf(w, "aiwiki_http_requests_total%s %d\n", labels("route", k.route, "method", k.method, "code", k.code), reqs[k].n.Load())
	}

	head("aiwiki_http_request_duration_seconds", "histogram", "HTTP request duration by route pattern and method.")
	durs := m.durations.snapshot()
	dkeys := make([]durationKey, 0, len(durs))
	for k := range durs {
		dkeys = append(dkeys, k)
	}
	sort.Slice(dkeys, func(i, j int) bool {
		if dkeys[i].route != dkeys[j].route {
			return dkeys[i].route < dkeys[j].route
		}
		return dkeys[i].method < dkeys[j].method
	})
	for _, k := range dkeys {
		writeHistogram(w, "aiwiki_http_request_duration_seconds", []string{"route", k.route, "method", k.method}, durs[k])
	}

	head("aiwiki_http_in_flight_requests", "gauge", "Requests currently being served.")
	fmt.Fprintf(w, "aiwiki_http_in_flight_requests %d\n", m.inFlight.Load())

	head("aiwiki_mcp_tool_calls_total", "counter", "MCP tools/call by tool and outcome.")
	calls := m.toolCalls.snapshot()
	ckeys := make([][2]string, 0, len(calls))
	for k := range calls {
		ckeys = append(ckeys, k)
	}
	sort.Slice(ckeys, func(i, j int) bool {
		if ckeys[i][0] != ckeys[j][0] {
			return ckeys[i][0] < ckeys[j][0]
		}
		return ckeys[i][1] < ckeys[j][1]
	})
	for _, k := range ckeys {
		fmt.Fprintf(w, "aiwiki_mcp_tool_calls_total%s %d\n", labels("tool", k[0], "outcome", k[1]), calls[k].n.Load())
	}

	head("aiwiki_mcp_tool_duration_seconds", "histogram", "MCP tools/call duration by tool.")
	times := m.toolTimes.snapshot()
	tkeys := make([]string, 0, len(times))
	for k := range times {
		tkeys = append(tkeys, k)
	}
	sort.Strings(tkeys)
	for _, k := range tkeys {
		writeHistogram(w, "aiwiki_mcp_tool_duration_seconds", []string{"tool", k}, times[k])
	}

	head("aiwiki_rate_limited_total", "counter", "Requests refused by a rate limiter.")
	for i, n := range rateLimiterNames {
		fmt.Fprintf(w, "aiwiki_rate_limited_total%s %d\n", labels("limiter", n), m.limited[i].n.Load())
	}

	head("aiwiki_auth_logins_total", "counter", "OIDC login callbacks by outcome.")
	for i, n := range loginOutcomes {
		fmt.Fprintf(w, "aiwiki_auth_logins_total%s %d\n", labels("outcome", n), m.logins[i].n.Load())
	}

	pools := make([]string, 0, len(src.pools))
	for p := range src.pools {
		pools = append(pools, p)
	}
	sort.Strings(pools)
	stats := map[string]sql.DBStats{}
	for _, p := range pools {
		stats[p] = src.pools[p].Stats()
	}
	poolMetric := func(name, typ, help string, f func(sql.DBStats) float64) {
		head(name, typ, help)
		for _, p := range pools {
			fmt.Fprintf(w, "%s%s %s\n", name, labels("pool", p), fmtFloat(f(stats[p])))
		}
	}
	poolMetric("aiwiki_db_open_connections", "gauge", "Open database connections.", func(s sql.DBStats) float64 { return float64(s.OpenConnections) })
	poolMetric("aiwiki_db_in_use_connections", "gauge", "Database connections in use.", func(s sql.DBStats) float64 { return float64(s.InUse) })
	poolMetric("aiwiki_db_idle_connections", "gauge", "Idle database connections.", func(s sql.DBStats) float64 { return float64(s.Idle) })
	poolMetric("aiwiki_db_wait_count_total", "counter", "Connections waited for.", func(s sql.DBStats) float64 { return float64(s.WaitCount) })
	poolMetric("aiwiki_db_wait_duration_seconds_total", "counter", "Time blocked waiting for a connection.", func(s sql.DBStats) float64 { return s.WaitDuration.Seconds() })

	head("aiwiki_ready", "gauge", "Last readiness result (1 ready, 0 not ready).")
	fmt.Fprintf(w, "aiwiki_ready %s\n", fmtFloat(src.ready))

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	head("go_goroutines", "gauge", "Number of goroutines.")
	fmt.Fprintf(w, "go_goroutines %d\n", runtime.NumGoroutine())
	head("go_memstats_heap_alloc_bytes", "gauge", "Bytes of allocated heap objects.")
	fmt.Fprintf(w, "go_memstats_heap_alloc_bytes %d\n", ms.HeapAlloc)
	head("process_start_time_seconds", "gauge", "Start time of the process since the Unix epoch.")
	fmt.Fprintf(w, "process_start_time_seconds %d\n", processStart.Unix())
}
