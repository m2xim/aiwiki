package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ---- route holder ----
//
// ServeMux.ServeHTTP writes the matched pattern into the *Request it received,
// and every WithContext in between makes a copy, so the outermost middleware
// cannot read it. It puts a holder into the context instead; every mux is
// wrapped by trackMux, which stores its pattern into the holder after its
// ServeHTTP returns, only when the holder is still empty. The innermost mux
// returns first, so the most specific pattern wins.

type routeHolder struct{ pattern string }

type routeHolderKey struct{}

func routeHolderFrom(ctx context.Context) *routeHolder {
	h, _ := ctx.Value(routeHolderKey{}).(*routeHolder)
	return h
}

// trackMux serves mux and records the pattern that matched.
func trackMux(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deferred, so a panicking handler is still labelled with its real route.
		defer func() {
			if h := routeHolderFrom(r.Context()); h != nil && h.pattern == "" {
				h.pattern = r.Pattern
			}
		}()
		mux.ServeHTTP(w, r)
	})
}

// routeLabel turns a recorded pattern into the bounded metric/log label. The
// catch-all "/" serves the static frontend, except below the API prefixes where
// it only produces a 404 (or an authentication refusal): that is "other".
func routeLabel(pattern, path string) string {
	switch pattern {
	case "":
		return "other"
	case "/":
		if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/files/") || authIsMCPPath(path) {
			return "other"
		}
		return "static"
	}
	return pattern
}

// ---- response writer ----

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(code) // interim response, the final one follows
		return
	}
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ReadFrom keeps the sendfile fast path of the underlying writer (FileServer,
// ServeContent) while counting the bytes.
func (w *statusWriter) ReadFrom(src io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	var n int64
	var err error
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err = rf.ReadFrom(src)
	} else {
		n, err = io.Copy(struct{ io.Writer }{w.ResponseWriter}, src)
	}
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ---- request id ----

var upstreamRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// ---- middleware ----

// SetAccessLog enables the JSON access log (one slog line per request) on w.
// A nil writer disables it.
func (s *Server) SetAccessLog(w io.Writer) {
	if w == nil {
		s.accessLog = nil
		return
	}
	s.accessLog = slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// ParseAccessLogMode validates AIWIKI_ACCESS_LOG ("", "off" or "json") and
// reports whether the JSON log is requested.
func ParseAccessLogMode(v string) (bool, error) {
	switch v {
	case "", "off":
		return false, nil
	case "json":
		return true, nil
	}
	return false, fmt.Errorf("invalid AIWIKI_ACCESS_LOG %q (expected off or json)", v)
}

// observe is the outermost middleware: request id, route holder, metrics and
// access log. It never reads the query string, headers (except the validated
// upstream id), cookies, bodies or the client address.
func (s *Server) observe(next http.Handler) http.Handler { return s.observeWith(defaultMetrics, next) }

func (s *Server) observeWith(m *metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := newRequestID()
		w.Header().Set("X-Request-Id", id)
		upstream := r.Header.Get("X-Request-Id")
		holder := &routeHolder{}
		sw := &statusWriter{ResponseWriter: w}
		m.inFlight.Add(1)
		finished := false
		defer func() {
			m.inFlight.Add(-1)
			status := sw.status
			if !finished && status == 0 {
				status = http.StatusInternalServerError // handler panicked before answering
			} else if status == 0 {
				status = http.StatusOK
			}
			route := routeLabel(holder.pattern, r.URL.Path)
			elapsed := time.Since(start)
			method := normalizeMethod(r.Method)
			m.observeRequest(route, method, status, elapsed)
			if logger := s.accessLog; logger != nil {
				attrs := []slog.Attr{
					slog.String("request_id", id),
					slog.String("method", method),
					slog.String("route", route),
					slog.Int("status", status),
					slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
					slog.Int64("bytes", sw.bytes),
				}
				if upstreamRequestID.MatchString(upstream) {
					attrs = append(attrs, slog.String("upstream_request_id", upstream))
				}
				logger.LogAttrs(context.Background(), slog.LevelInfo, "request", attrs...)
			}
		}()
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), routeHolderKey{}, holder)))
		finished = true
	})
}

// ---- metrics listener ----

// ValidateMetricsConfig checks AIWIKI_METRICS_ADDR and AIWIKI_METRICS_TOKEN.
// An empty address disables the listener. A non-loopback address requires a
// token of at least 32 characters.
func ValidateMetricsConfig(addr, token string) error {
	if addr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return fmt.Errorf("invalid AIWIKI_METRICS_ADDR %q (expected host:port)", addr)
	}
	if !literalLoopbackIP(host) && len(token) < 32 {
		return errors.New("AIWIKI_METRICS_ADDR is not a loopback address: AIWIKI_METRICS_TOKEN of at least 32 characters is required")
	}
	return nil
}

// literalLoopbackIP is true only for 127.0.0.0/8 and ::1 written as IP literals;
// names (even "localhost") resolve outside our control, so they need the token.
func literalLoopbackIP(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func bearerMatches(header, token string) bool {
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(header[len(prefix):])))
	want := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// MetricsHandler serves only GET /metrics and GET /api/ready. When token is
// not empty, both require "Authorization: Bearer <token>".
func (s *Server) MetricsHandler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		ready := 0.0
		if s.readiness().ok(r.Context()) {
			ready = 1
		}
		src := metricsSource{mode: "trusted", pools: map[string]*sql.DB{}, ready: ready}
		if s.personal {
			src.mode = "personal"
		} else if s.auth != nil {
			src.mode = "corporate"
		}
		if s.DB != nil && s.DB.pool != nil {
			src.pools["app"] = s.DB.pool
		}
		if s.authDB != nil && s.authDB.pool != nil {
			src.pools["auth"] = s.authDB.pool
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		defaultMetrics.write(w, src)
	})
	mux.HandleFunc("GET /api/ready", s.readyHandler)
	if token == "" {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !bearerMatches(r.Header.Get("Authorization"), token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// observedCallTool runs callTool and records the call in the MCP metrics.
func (s *Server) observedCallTool(params map[string]json.RawMessage) any {
	var name string
	_ = json.Unmarshal(params["name"], &name)
	start := time.Now()
	result := s.callTool(params)
	isErr := false
	if m, ok := result.(map[string]any); ok {
		isErr, _ = m["isError"].(bool)
	}
	defaultMetrics.observeTool(name, isErr, time.Since(start))
	return result
}
