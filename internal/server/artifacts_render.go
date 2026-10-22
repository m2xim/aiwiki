package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// artifactCapTTL is the longest life of a render capability.
const artifactCapTTL = 15 * time.Minute

// artifactState is the process-wide state of the artifact renderer: the HMAC
// secret, the optional separate frame origin and the public-link rate limiter.
type artifactState struct {
	secret  []byte
	origin  string
	limiter *rateLimiter
	proxies []*net.IPNet // AIWIKI_TRUSTED_PROXIES: peers whose X-Forwarded-For is believed
}

// minArtifactSecret is the shortest accepted AIWIKI_ARTIFACT_SECRET.
const minArtifactSecret = 32

// parseTrustedProxies reads a comma or space separated list of CIDRs or IPs.
func parseTrustedProxies(list string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, item := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if !strings.Contains(item, "/") {
			ip := net.ParseIP(item)
			if ip == nil {
				return nil, fmt.Errorf("invalid address %q", item)
			}
			bits := 128
			if ip.To4() != nil {
				bits = 32
			}
			item += "/" + strconv.Itoa(bits)
		}
		_, n, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q", item)
		}
		out = append(out, n)
	}
	return out, nil
}

// newArtifactState reads AIWIKI_ARTIFACT_SECRET (required for several
// instances; otherwise a random per-process secret is used, so capabilities do
// not survive a restart) and AIWIKI_ARTIFACT_ORIGIN (optional hardening).
func newArtifactState() (*artifactState, error) {
	st := &artifactState{limiter: newRateLimiter()}
	if v := os.Getenv("AIWIKI_ARTIFACT_SECRET"); v != "" {
		if len(v) < minArtifactSecret {
			return nil, fmt.Errorf("AIWIKI_ARTIFACT_SECRET must be at least %d bytes (or unset for a random per-process secret)", minArtifactSecret)
		}
		st.secret = []byte(v)
	} else {
		st.secret = make([]byte, 32)
		if _, err := rand.Read(st.secret); err != nil {
			return nil, err
		}
	}
	if v := os.Getenv("AIWIKI_ARTIFACT_ORIGIN"); v != "" {
		origin, err := canonicalAllowedOrigin(v)
		if err != nil {
			return nil, fmt.Errorf("AIWIKI_ARTIFACT_ORIGIN: %w", err)
		}
		st.origin = origin
	}
	if v := os.Getenv("AIWIKI_TRUSTED_PROXIES"); v != "" {
		proxies, err := parseTrustedProxies(v)
		if err != nil {
			return nil, fmt.Errorf("AIWIKI_TRUSTED_PROXIES: %w", err)
		}
		st.proxies = proxies
	}
	return st, nil
}

// randomArtifactState is the state of servers assembled without Open (test
// fixtures): a random secret, never a constant, and no environment.
func randomArtifactState() *artifactState {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		panic("no randomness available: " + err.Error())
	}
	return &artifactState{secret: secret, limiter: newRateLimiter()}
}

var (
	fallbackArtifactOnce  sync.Once
	fallbackArtifactState *artifactState
)

// artifacts returns the server's artifact state (a process-wide random one for
// servers assembled without Open, such as test fixtures).
func (s *Server) artifacts() *artifactState {
	if s.art != nil {
		return s.art
	}
	fallbackArtifactOnce.Do(func() {
		if st, err := newArtifactState(); err == nil {
			fallbackArtifactState = st
		} else {
			fallbackArtifactState = randomArtifactState()
		}
	})
	return fallbackArtifactState
}

// ---- capability ----

// artifactCap is the payload of a render capability. Share is set for public
// capabilities only: it lets every frame request re-check that exact link.
type artifactCap struct {
	Artifact string `json:"a"`
	Revision int    `json:"r"`
	Expires  int64  `json:"e"`
	Kind     string `json:"k"`
	Share    string `json:"s,omitempty"`
}

func (st *artifactState) mac(payload []byte) []byte {
	m := hmac.New(sha256.New, st.secret)
	m.Write(payload)
	return m.Sum(nil)
}

// sign returns base64url(payload) "." base64url(HMAC-SHA256(secret, payload)).
func (st *artifactState) sign(c artifactCap) string {
	payload, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(st.mac(payload))
}

// verify checks the signature (in constant time), the shape and the expiry.
func (st *artifactState) verify(token string, now time.Time) (artifactCap, bool) {
	var c artifactCap
	if len(token) > 1024 {
		return c, false
	}
	p, m, ok := strings.Cut(token, ".")
	if !ok || strings.Contains(m, ".") {
		return c, false
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	sig, err2 := base64.RawURLEncoding.DecodeString(m)
	if err1 != nil || err2 != nil || !hmac.Equal(sig, st.mac(payload)) {
		return c, false
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if dec.Decode(&c) != nil || !validPathID(c.Artifact) || c.Revision < 1 {
		return artifactCap{}, false
	}
	switch c.Kind {
	case "user":
		if c.Share != "" {
			return artifactCap{}, false
		}
	case "public":
		if !validPathID(c.Share) {
			return artifactCap{}, false
		}
	default:
		return artifactCap{}, false
	}
	left := time.Unix(c.Expires, 0).Sub(now)
	if left <= 0 || left > artifactCapTTL+30*time.Second {
		return artifactCap{}, false
	}
	return c, true
}

// ---- headers ----

// artifactFrameCSP is the one place that builds the frame Content-Security-
// Policy. ancestors is the frame-ancestors source list ("'self'" by default).
func artifactFrameCSP(ancestors string) string {
	return "sandbox allow-scripts allow-modals; default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; media-src 'self' data: blob:; connect-src 'none'; form-action 'none'; base-uri 'none'; webrtc 'block'; frame-ancestors " + ancestors
}

const artifactPermissionsPolicy = "camera=(), microphone=(), geolocation=(), payment=(), usb=(), clipboard-read=()"

// artifactFrameAncestors is 'self', plus the application origin when frames are
// served from a separate origin (AIWIKI_ARTIFACT_ORIGIN).
func (s *Server) artifactFrameAncestors() string {
	if st := s.artifacts(); st.origin != "" {
		// Served from a separate origin: 'self' would be the frame host itself, so
		// only the application origin may embed it ('none' until it is known).
		if s.allowedOrigin != "" {
			return s.allowedOrigin
		}
		return "'none'"
	}
	return "'self'"
}

// setArtifactFrameHeaders sets the headers of a served frame file. CORP is
// cross-origin for every file: the sandboxed frame has an opaque origin, so a
// same-origin policy would block its own scripts, styles and images (verified in
// Chromium). The file is protected by the capability in the path instead.
func (s *Server) setArtifactFrameHeaders(h http.Header, mime string, left time.Duration) {
	h.Set("Content-Type", artifactContentType(mime))
	h.Set("Content-Security-Policy", artifactFrameCSP(s.artifactFrameAncestors()))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	h.Set("Permissions-Policy", artifactPermissionsPolicy)
	secs := int(left.Seconds())
	if secs < 0 {
		secs = 0
	}
	h.Set("Cache-Control", "private, max-age="+strconv.Itoa(secs))
}

func artifactNotFound(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, "not found\n")
}

// ---- rate limiting (in the process; not shared between instances) ----

type rateWindow struct {
	start time.Time
	n     int
}

type rateLimiter struct {
	mu  sync.Mutex
	m   map[string]*rateWindow
	now func() time.Time
}

func newRateLimiter() *rateLimiter { return &rateLimiter{m: map[string]*rateWindow{}, now: time.Now} }

// allow counts one request for key in a fixed window and reports whether it is
// within limit. The table is bounded: when it grows it drops expired windows,
// and as a last resort starts over (never growing without bound).
func (l *rateLimiter) allow(key string, limit int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.m) > 20000 {
		for k, w := range l.m {
			if now.Sub(w.start) >= window {
				delete(l.m, k)
			}
		}
		// Still too many live windows: evict arbitrary entries down to half.
		for k := range l.m {
			if len(l.m) <= 10000 {
				break
			}
			delete(l.m, k)
		}

	}
	w := l.m[key]
	if w == nil || now.Sub(w.start) >= window {
		w = &rateWindow{start: now}
		l.m[key] = w
	}
	w.n++
	return w.n <= limit
}

const (
	publicIPLimit    = 300  // /s/ requests per minute per client address
	publicTokenLimit = 600  // /s/ requests per minute per link
	frameIPLimit     = 1200 // /artifact-frame/ requests per minute per client address
)

func (st *artifactState) trustedProxy(ip net.IP) bool {
	for _, n := range st.proxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP is the peer address, unless the peer is a configured trusted proxy:
// then it is the last X-Forwarded-For address that is not itself a trusted
// proxy (the rightmost entries are the ones a trusted hop appended; anything to
// the left may be forged by the client). Without AIWIKI_TRUSTED_PROXIES every
// client behind a reverse proxy shares the proxy's address and one limit.
func (st *artifactState) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || len(st.proxies) == 0 || !st.trustedProxy(peer) {
		return host
	}
	var chain []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		chain = append(chain, strings.Split(v, ",")...)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(chain[i]))
		if ip == nil {
			return host // malformed chain: do not guess
		}
		if !st.trustedProxy(ip) {
			return ip.String()
		}
	}
	return host
}

// ---- request-scoped view database ----

// scopedView returns a database pinned to one connection whose session carries
// the given artifact-view settings (app.artifact_view, app.share_id,
// app.share_token_hash), which the artifact policies accept instead of a user.
// The settings are set only by the server after it verified a capability or
// looked up a public token. SQLite has no policies: there it is the plain pool.
func (d *database) scopedView(ctx context.Context, settings map[string]string) (*database, func(), error) {
	if !d.pg {
		return d, func() {}, nil
	}
	if d.conn != nil || d.pool == nil {
		return nil, nil, errors.New("database is already scoped")
	}
	conn, err := d.pool.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	keys := []string{"app.artifact_view", "app.share_id", "app.share_token_hash", "app.user_id", "app.mcp_projects", "app.mcp_mode"}
	apply := func(c context.Context, values map[string]string) error {
		for _, k := range keys {
			var got string
			if err := conn.QueryRowContext(c, `SELECT set_config($1,$2,false)`, k, values[k]).Scan(&got); err != nil {
				return err
			}
			if got != values[k] {
				return errors.New("session scope was not applied")
			}
		}
		return nil
	}
	if err := apply(ctx, settings); err != nil {
		discardConn(conn)
		return nil, nil, err
	}
	view := &database{pg: true, conn: conn, ctx: ctx}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, tx := range view.activeTx {
				_ = tx.Rollback()
			}
			if _, err := conn.ExecContext(cctx, "ROLLBACK"); err != nil {
				discardConn(conn)
				return
			}
			if err := apply(cctx, map[string]string{}); err != nil {
				discardConn(conn)
				return
			}
			_ = conn.Close()
		})
	}
	view.release = cleanup
	return view, cleanup, nil
}

// ---- /artifact-frame/<cap>/<path> ----

func (s *Server) artifactFrame(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	st := s.artifacts()
	if !st.limiter.allow("frame:"+st.clientIP(r), frameIPLimit, time.Minute) {
		defaultMetrics.rateLimited("artifact_frame")
		w.Header().Set("Retry-After", "60")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "Too many requests\n")
		return
	}
	c, ok := st.verify(r.PathValue("cap"), now)
	rel := r.PathValue("path")
	if rel == "" {
		rel = "index.html"
	}
	if !ok {
		artifactNotFound(w)
		return
	}
	if _, err := validArtifactPath(rel); err != nil {
		artifactNotFound(w)
		return
	}
	settings := map[string]string{"app.artifact_view": c.Artifact}
	if c.Kind == "public" {
		settings["app.share_id"] = c.Share
	}
	view, cleanup, err := s.DB.scopedView(r.Context(), settings)
	if err != nil {
		apiErr(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	defer cleanup()
	file, found, derr := s.artifactFrameLookup(view, c, rel)
	cleanup()
	if derr != nil {
		apiErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if !found {
		artifactNotFound(w)
		return
	}
	store, err := s.objectStorage()
	if err != nil {
		apiErr(w, http.StatusServiceUnavailable, "object storage unavailable")
		return
	}
	data, err := readArtifactBlob(r.Context(), store, file)
	if err != nil {
		if errors.Is(err, errObjectNotFound) {
			artifactNotFound(w)
			return
		}
		apiErr(w, http.StatusBadGateway, "could not read file")
		return
	}
	if rel == "index.html" && file.MIME == "text/html" {
		data = injectResizeScript(data)
	}
	s.setArtifactFrameHeaders(w.Header(), file.MIME, time.Unix(c.Expires, 0).Sub(now))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

// artifactFrameLookup re-checks, on every request, that the artifact is not
// deleted and, for a public capability, that the link is not revoked and
// public links are enabled; then it resolves the file of the pinned revision.
func (s *Server) artifactFrameLookup(db *database, c artifactCap, rel string) (artifactFile, bool, error) {
	var deleted sql.NullString
	var current int
	err := db.QueryRow(`SELECT revision,deleted_at FROM artifacts WHERE id=?`, c.Artifact).Scan(&current, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return artifactFile{}, false, nil
	}
	if err != nil {
		return artifactFile{}, false, err
	}
	if deleted.Valid || c.Revision > current {
		return artifactFile{}, false, nil
	}
	if c.Kind == "public" {
		if !s.publicLinksEnabled(db) {
			return artifactFile{}, false, nil
		}
		var revoked sql.NullString
		var shareArtifact, creator string
		err := db.QueryRow(`SELECT artifact_id,created_by,revoked_at FROM artifact_shares WHERE id=?`, c.Share).Scan(&shareArtifact, &creator, &revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return artifactFile{}, false, nil
		}
		if err != nil {
			return artifactFile{}, false, err
		}
		if revoked.Valid || shareArtifact != c.Artifact || !s.artifactLinkCreatorValid(db, c.Artifact, creator) {
			return artifactFile{}, false, nil
		}
	}
	files, found, err := loadManifest(db, c.Artifact, c.Revision)
	if err != nil || !found {
		return artifactFile{}, false, err
	}
	for _, f := range files {
		if f.Path == rel {
			return f, true, nil
		}
	}
	return artifactFile{}, false, nil
}

// ---- /artifact-runtime/* ----

var runtimeTypes = map[string]string{
	".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8", ".json": "application/json; charset=utf-8",
	".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2", ".woff": "font/woff",
	".map": "application/json; charset=utf-8", ".txt": "text/plain; charset=utf-8",
}

// artifactRuntime serves the artifact kit from <web dir>/artifact-runtime/. It
// answers 404 when the directory is absent. The kit is public static code:
// Cache-Control revalidates on every use (the file names are not content-
// hashed), and Cross-Origin-Resource-Policy is cross-origin because the script
// is requested by a document with an opaque origin (the sandboxed frame), which
// a same-origin policy would block (verified in Chromium).
func (s *Server) artifactRuntime(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("file")
	if rel == "resize.js" {
		h := w.Header()
		h.Set("Content-Type", "text/javascript; charset=utf-8")
		h.Set("Cache-Control", "public, no-cache")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cross-Origin-Resource-Policy", "cross-origin")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(artifactResizeJS))
		return
	}
	if s.WebDir == "" || rel == "" || strings.ContainsAny(rel, "\\\x00") || strings.HasPrefix(rel, "/") {
		artifactNotFound(w)
		return
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, ".") {
			artifactNotFound(w)
			return
		}
	}
	ctype, ok := runtimeTypes[strings.ToLower(filepath.Ext(rel))]
	if !ok {
		artifactNotFound(w)
		return
	}
	root := filepath.Join(s.WebDir, "artifact-runtime")
	full := filepath.Join(root, filepath.FromSlash(rel))
	if within, err := filepath.Rel(root, full); err != nil || strings.HasPrefix(within, "..") {
		artifactNotFound(w)
		return
	}
	f, err := os.Open(full)
	if err != nil {
		artifactNotFound(w)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		artifactNotFound(w)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "public, no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// ---- /s/<token> ----

func publicTokenPattern(t string) bool {
	if len(t) != 43 {
		return false
	}
	for _, c := range t {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func hashShareToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func publicPageHeaders(h http.Header) {
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

// writeArtifactUnavailable is the one neutral page for every reason a public
// link does not work (unknown, revoked, deleted artifact, links disabled,
// malformed): the response must not reveal which.
func writeArtifactUnavailable(w http.ResponseWriter) {
	publicPageHeaders(w.Header())
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `<!doctype html><html lang="ru"><head><meta charset="utf-8"><meta name="robots" content="noindex, nofollow"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Ссылка недоступна</title><style>body{font:16px/1.5 system-ui,sans-serif;margin:0;min-height:100vh;display:grid;place-items:center;color:#333;background:#fafafa}main{text-align:center;padding:24px}</style></head><body><main><h1>Ссылка недоступна</h1></main></body></html>`)
}

func (s *Server) publicArtifact(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	st := s.artifacts()
	// Both counters are charged on every request so a guesser cannot probe
	// without being limited, and the limit answer does not depend on the token.
	ipOK := st.limiter.allow("ip:"+st.clientIP(r), publicIPLimit, time.Minute)

	tokenOK := st.limiter.allow("tok:"+hashShareToken(token), publicTokenLimit, time.Minute)
	if !ipOK || !tokenOK {
		if !ipOK {
			defaultMetrics.rateLimited("artifact_public")
		}
		if !tokenOK {
			defaultMetrics.rateLimited("artifact_token")
		}
		publicPageHeaders(w.Header())
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "Too many requests\n")
		return
	}
	if !publicTokenPattern(token) {
		writeArtifactUnavailable(w)
		return
	}
	hash := hashShareToken(token)
	view, cleanup, err := s.DB.scopedView(r.Context(), map[string]string{"app.share_token_hash": hash})
	if err != nil {
		apiErr(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	defer cleanup()
	var shareID, artifactID, creator string
	var pinned sql.NullInt64
	var revoked sql.NullString
	err = view.QueryRow(`SELECT id,artifact_id,created_by,revision,revoked_at FROM artifact_shares WHERE token_hash=?`, hash).Scan(&shareID, &artifactID, &creator, &pinned, &revoked)
	if errors.Is(err, sql.ErrNoRows) || err == nil && revoked.Valid {
		writeArtifactUnavailable(w)
		return
	}
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if view.pg {
		var got string
		if err = view.QueryRow(`SELECT set_config('app.artifact_view',?,false)`, artifactID).Scan(&got); err != nil {
			apiErr(w, http.StatusInternalServerError, "database error")
			return
		}
	}
	var title string
	var current int
	var deleted sql.NullString
	err = view.QueryRow(`SELECT title,revision,deleted_at FROM artifacts WHERE id=?`, artifactID).Scan(&title, &current, &deleted)
	enabled := s.publicLinksEnabled(view) && s.artifactLinkCreatorValid(view, artifactID, creator)
	cleanup()
	if errors.Is(err, sql.ErrNoRows) || err == nil && (deleted.Valid || !enabled) {
		writeArtifactUnavailable(w)
		return
	}
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "database error")
		return
	}
	rev := current
	if pinned.Valid {
		rev = int(pinned.Int64)
	}
	if rev < 1 || rev > current {
		writeArtifactUnavailable(w)
		return
	}
	cap := st.sign(artifactCap{Artifact: artifactID, Revision: rev, Expires: time.Now().Add(artifactCapTTL).Unix(), Kind: "public", Share: shareID})
	s.writePublicPage(w, title, s.artifactFramePrefix()+"/artifact-frame/"+cap+"/index.html")
}

// artifactFramePrefix is the separate frame origin, or empty for the same origin.
func (s *Server) artifactFramePrefix() string { return s.artifacts().origin }

func (s *Server) writePublicPage(w http.ResponseWriter, title, frameURL string) {
	nonceBytes := make([]byte, 16)
	_, _ = rand.Read(nonceBytes)
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	frameSrc := "'self'"
	if o := s.artifacts().origin; o != "" {
		frameSrc += " " + o
	}
	publicPageHeaders(w.Header())
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+"'; frame-src "+frameSrc+"; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	esc := html.EscapeString(title)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<!doctype html><html lang="ru"><head><meta charset="utf-8"><meta name="robots" content="noindex, nofollow"><meta name="viewport" content="width=device-width, initial-scale=1"><title>%[1]s</title><style nonce="%[2]s">html,body{margin:0;background:#fff;color:#222;font:16px/1.4 system-ui,sans-serif}h1{font-size:18px;margin:0;padding:12px 16px;border-bottom:1px solid #e3e3e3}iframe{display:block;width:100%%;height:640px;border:0}</style></head><body><h1>%[1]s</h1><iframe id="f" sandbox="allow-scripts allow-modals" referrerpolicy="no-referrer" src="%[3]s" title="%[1]s"></iframe><script nonce="%[2]s">var f=document.getElementById('f');window.addEventListener('message',function(e){if(e.source!==f.contentWindow)return;var d=e.data;if(!d||d.type!=='aiwiki:resize'||typeof d.height!=='number'||!isFinite(d.height))return;f.style.height=Math.max(120,Math.min(4000,Math.round(d.height)))+'px'});</script></body></html>`, esc, nonce, html.EscapeString(frameURL))
}

func isArtifactAssetPath(p string) bool {
	return strings.HasPrefix(p, "/artifact-frame/") || strings.HasPrefix(p, "/artifact-runtime/")
}

func isArtifactAssetRequest(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) && isArtifactAssetPath(r.URL.Path)
}

// withArtifactOrigin implements the optional hardening AIWIKI_ARTIFACT_ORIGIN:
// requests whose Host is that origin are answered with the frame and runtime
// routes only (they never reach the application API); everything else follows
// the normal Host and Origin rules. Without the setting it is a no-op.
func (s *Server) withArtifactOrigin(app http.Handler) http.Handler {
	origin := s.artifacts().origin
	if origin == "" {
		return app
	}
	trusted := newTrustedHost(origin)
	mux := http.NewServeMux()
	s.registerArtifactAssetRoutesFrameOnly(mux)
	tracked := trackMux(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authority, ok := parseHostAuthority(r.Host); ok && trusted.matches(authority) {
			if !isArtifactAssetRequest(r) {
				artifactNotFound(w)
				return
			}
			if r.Header.Get("Origin") == "null" {
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Vary", "Origin")
			}
			tracked.ServeHTTP(w, r)
			return
		}
		if isArtifactAssetPath(r.URL.Path) {
			// Frames and the runtime exist only on their own origin.
			artifactNotFound(w)
			return
		}
		app.ServeHTTP(w, r)

	})
}

func (s *Server) registerArtifactAssetRoutesFrameOnly(mux *http.ServeMux) {
	mux.HandleFunc("GET /artifact-frame/{cap}/{path...}", s.artifactFrame)
	mux.HandleFunc("GET /artifact-runtime/{file...}", s.artifactRuntime)
}

//go:embed artifact_resize.js
var artifactResizeJS []byte

const resizeScriptTag = `<script src="/artifact-runtime/resize.js"></script>`

// injectResizeScript adds the height reporter to a served entry document: before
// the last </body>, or at the end when there is none. Stored bytes are untouched.
func injectResizeScript(doc []byte) []byte {
	i := lastIndexFold(doc, "</body")
	if i < 0 {
		i = len(doc)
	}
	out := make([]byte, 0, len(doc)+len(resizeScriptTag))
	out = append(out, doc[:i]...)
	out = append(out, resizeScriptTag...)
	return append(out, doc[i:]...)
}

// lastIndexFold finds the last ASCII-case-insensitive occurrence of needle
// (lower case) without altering byte offsets, which a Unicode fold could.
func lastIndexFold(b []byte, needle string) int {
	for i := len(b) - len(needle); i >= 0; i-- {
		ok := true
		for j := 0; j < len(needle); j++ {
			c := b[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}
