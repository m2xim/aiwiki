package server

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// Server serves the local wiki API and its built frontend.
type Server struct {
	authDB          *database
	objects         objectStore
	DB              *database
	DataDir, WebDir string
	allowedOrigin   string
	auth            *AuthService
	principal       *Principal
	art             *artifactState
	// spaceQuotaDefault is AIWIKI_SPACE_QUOTA_BYTES: the storage limit of a space
	// without an override (0 = unlimited). Corporate mode only.
	spaceQuotaDefault int64
	accessLog         *slog.Logger
	ready             *readiness
}

// SetAllowedOrigin enables one explicitly trusted, canonicalized browser origin.
func (s *Server) SetAllowedOrigin(origin string) error {
	canonical, err := canonicalAllowedOrigin(origin)
	if err != nil {
		return err
	}
	s.allowedOrigin = canonical
	return nil
}

// ValidateAllowedOrigin checks that the value is a serialized HTTP(S) origin.
func ValidateAllowedOrigin(origin string) error {
	_, err := canonicalAllowedOrigin(origin)
	return err
}

func canonicalAllowedOrigin(origin string) (string, error) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Opaque != "" || (!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return "", fmt.Errorf("invalid allowed origin %q (expected an http or https origin without user info, path, query, or fragment)", origin)
	}
	authority, ok := parseHostAuthority(parsed.Host)
	if !ok {
		return "", fmt.Errorf("invalid allowed origin %q (invalid host or port)", origin)
	}
	host := authority.host
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	defaultPort := "80"
	if strings.EqualFold(parsed.Scheme, "https") {
		defaultPort = "443"
	}
	canonical := strings.ToLower(parsed.Scheme) + "://" + host
	if authority.port != "" && authority.port != defaultPort {
		canonical += ":" + authority.port
	}
	return canonical, nil
}

func Open(dataDir, webDir string) (*Server, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "files"), 0755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "wiki.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;"); err != nil {
		db.Close()
		return nil, err
	}
	s := &Server{DB: &database{pool: db}, DataDir: dataDir, WebDir: webDir}
	if s.art, err = newArtifactState(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.seed(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Server) Close() error {
	if s.authDB != nil {
		_ = s.authDB.Close()
	}
	return s.DB.Close()
}

// Handler returns the routes behind the Host and Origin checks of corsLocal.
func (s *Server) Handler() http.Handler {
	var app http.Handler
	if s.auth != nil {
		app = corsLocal(s.corporateHandler(), s.allowedOrigin)
	} else {
		app = corsLocal(s.routes(), s.allowedOrigin)
	}
	if s.ready == nil {
		s.ready = s.newReadiness()
	}
	return s.observe(s.withArtifactOrigin(app))
}

// routes is the bare router. In-process callers that have already passed the
// Host check on the outer request may use it directly.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	// Corporate metadata is served by the outer auth router. In trusted mode,
	// discovery must return 404 rather than the SPA document.
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", http.NotFound)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", http.NotFound)
	if s.DB.pg {
		s.corporateRoutes(mux)
	} else {
		mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"mode": "trusted"}) })
		mux.HandleFunc("GET /api/catalog", s.trustedCatalog)
	}
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /api/ready", s.readyHandler)
	mux.HandleFunc("/api/spaces", s.spaces)
	mux.HandleFunc("/api/spaces/{id}", s.space)
	mux.HandleFunc("GET /api/spaces/{id}/audit", s.spaceAudit)
	mux.HandleFunc("GET /api/spaces/{id}/storage", s.spaceStorage)
	mux.HandleFunc("GET /api/tags", s.tags)
	mux.HandleFunc("/api/articles", s.articles)
	mux.HandleFunc("/api/articles/", s.articleRoute)
	mux.HandleFunc("/api/comments/", s.commentRoute)
	mux.HandleFunc("GET /api/bootstrap", s.bootstrap)
	mux.HandleFunc("GET /api/export", s.export)
	mux.HandleFunc("/api/favorites", s.favorites)
	mux.HandleFunc("/api/favorites/{id}", s.favorite)
	mux.HandleFunc("/api/search", s.searchHTTP)
	mux.HandleFunc("/files/", s.fileRoute)
	mux.HandleFunc("/mcp", s.mcp)
	s.registerArtifactRoutes(mux)
	if s.WebDir != "" {
		mux.Handle("/", webCache(http.FileServer(http.Dir(s.WebDir))))
	}
	return trackMux(mux)
}

// webCache lets browsers keep content-hashed build assets forever and makes them
// revalidate everything else (index.html), so a deploy is picked up on reload.
func webCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

// corsLocal rejects requests whose Host is neither loopback nor the configured
// allowed origin's host and port (a DNS-rebinding defence: a page on a foreign
// name resolving to this server sends no Origin on same-origin reads), then
// applies the Origin rules. X-Forwarded-Host is deliberately not consulted.
func corsLocal(next http.Handler, allowedOrigin string) http.Handler {
	trusted := newTrustedHost(allowedOrigin)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authority, validHost := parseHostAuthority(r.Host)
		hostLoopback := validHost && isLoopbackHost(authority.host)
		if !hostLoopback && !(validHost && trusted.matches(authority)) {
			apiErr(w, http.StatusForbidden, "host not allowed")
			return
		}
		if origin := r.Header.Get("Origin"); origin == "null" && isArtifactAssetRequest(r) {
			// The sandboxed artifact frame has an opaque origin; it only loads its own
			// files and the public kit (module scripts and fonts send Origin: null).
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Vary", "Origin")
		} else if origin != "" {
			parsed, parseErr := url.Parse(origin)
			if parseErr != nil || (!allowedLocalOrigin(parsed, r, hostLoopback) && origin != allowedOrigin) {
				apiErr(w, http.StatusForbidden, "origin not allowed")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowedLocalOrigin accepts loopback origins, and only when the request's own
// Host is loopback, so a remote allowed host never trusts generic dev ports.
func allowedLocalOrigin(origin *url.URL, r *http.Request, hostLoopback bool) bool {
	if !hostLoopback || origin.User != nil || origin.Host == "" || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || !isLoopbackHost(origin.Hostname()) {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if strings.EqualFold(origin.Scheme, scheme) && strings.EqualFold(origin.Host, r.Host) {
		return true
	}
	if origin.Scheme != "http" {
		return false
	}
	port := origin.Port()
	return port == "8090" || port == "5173" || port == "4173" || port == "18088"
}

// hostAuthority is a request Host split into a lower-cased name or IP literal
// (no brackets) and a normalized numeric port ("" when absent).
type hostAuthority struct{ host, port string }

// parseHostAuthority strictly parses a Host header value. Anything that is not
// host or host:port (user info, paths, zones, bare IPv6, bad ports) is invalid.
func parseHostAuthority(value string) (hostAuthority, bool) {
	if value == "" {
		return hostAuthority{}, false
	}
	for _, c := range value {
		if c <= ' ' || c == 0x7f || strings.ContainsRune(`/\@?#%`, c) {
			return hostAuthority{}, false
		}
	}
	var host, port string
	hasPort := false
	if strings.HasPrefix(value, "[") {
		end := strings.IndexByte(value, ']')
		if end < 0 {
			return hostAuthority{}, false
		}
		host = value[1:end]
		if !strings.Contains(host, ":") || net.ParseIP(host) == nil {
			return hostAuthority{}, false
		}
		if rest := value[end+1:]; rest != "" {
			if rest[0] != ':' {
				return hostAuthority{}, false
			}
			port, hasPort = rest[1:], true
		}
	} else {
		host = value
		if i := strings.IndexByte(value, ':'); i >= 0 {
			host, port, hasPort = value[:i], value[i+1:], true
		}
		if host == "" {
			return hostAuthority{}, false
		}
		for _, c := range host {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_') {
				return hostAuthority{}, false
			}
		}
	}
	if hasPort {
		normalized, ok := normalizePort(port)
		if !ok {
			return hostAuthority{}, false
		}
		port = normalized
	}
	return hostAuthority{host: strings.ToLower(host), port: port}, true
}

// normalizePort accepts only 1..65535 written in digits and drops leading zeros.
func normalizePort(port string) (string, bool) {
	if port == "" || len(port) > 5 {
		return "", false
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", false
	}
	return strconv.Itoa(n), true
}

// trustedHost is the host and effective port of the explicitly allowed origin.
type trustedHost struct {
	host, port, defaultPort string
	ok                      bool
}

func newTrustedHost(origin string) trustedHost {
	if origin == "" {
		return trustedHost{}
	}
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" {
		return trustedHost{}
	}
	def := "80"
	if strings.EqualFold(u.Scheme, "https") {
		def = "443"
	}
	port := def
	if u.Port() != "" {
		var ok bool
		if port, ok = normalizePort(u.Port()); !ok {
			return trustedHost{}
		}
	}
	return trustedHost{host: strings.ToLower(u.Hostname()), port: port, defaultPort: def, ok: true}
}

// matches reports whether the request authority is exactly the configured host
// and effective port. A Host without a port means the scheme's default port.
func (t trustedHost) matches(a hostAuthority) bool {
	if !t.ok {
		return false
	}
	port := a.port
	if port == "" {
		port = t.defaultPort
	}
	if port != t.port {
		return false
	}
	if x, y := net.ParseIP(a.host), net.ParseIP(t.host); x != nil && y != nil {
		return x.Equal(y)
	}
	return a.host == t.host
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
