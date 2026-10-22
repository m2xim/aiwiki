package server

import (
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	// Requests per OAuth subject per window; same value as persistent keys.
	mcpOAuthRateLimit  = mcpPersistentRateLimit
	mcpOAuthRateWindow = time.Minute
	// Hard bound on distinct tracked subjects. When full, expired windows are
	// swept first; if the table is still full, a NEW subject is refused (429)
	// rather than evicting a live window, so an attacker cannot reset a victim's
	// counter by flooding fresh keys. Only subjects with a valid signed token
	// reach the limiter, so the table cannot be filled anonymously.
	mcpOAuthRateMaxKeys = 10000
)

type mcpOAuthWindow struct {
	start time.Time
	count int
}

// mcpOAuthLimiter is a per-process fixed-window limiter keyed by
// issuer + subject. It is not shared between processes.
type mcpOAuthLimiter struct {
	mu        sync.Mutex
	windows   map[string]*mcpOAuthWindow
	nextSweep time.Time
	limit     int
	window    time.Duration
	maxKeys   int
}

func newMCPOAuthLimiter() *mcpOAuthLimiter {
	return &mcpOAuthLimiter{windows: map[string]*mcpOAuthWindow{}, limit: mcpOAuthRateLimit, window: mcpOAuthRateWindow, maxKeys: mcpOAuthRateMaxKeys}
}

func (l *mcpOAuthLimiter) sweepLocked(now time.Time) {
	for k, w := range l.windows {
		if !now.Before(w.start.Add(l.window)) {
			delete(l.windows, k)
		}
	}
	l.nextSweep = now.Add(l.window)
}

// allow takes one slot; false means the subject is over its limit (or the
// table is full of live windows and the subject is new).
func (l *mcpOAuthLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !now.Before(l.nextSweep) {
		l.sweepLocked(now)
	}
	w := l.windows[key]
	// A clock that moved backwards also starts a fresh window.
	if w != nil && (!now.Before(w.start.Add(l.window)) || now.Before(w.start)) {
		delete(l.windows, key)
		w = nil
	}
	if w == nil {
		if len(l.windows) >= l.maxKeys {
			l.sweepLocked(now)
			if len(l.windows) >= l.maxKeys {
				return false
			}
		}
		l.windows[key] = &mcpOAuthWindow{start: now, count: 1}
		return true
	}
	if w.count >= l.limit {
		return false
	}
	w.count++
	return true
}

// mcpOAuthClientID returns the first non-empty azp/client_id from the claims
// (signed token first, then introspection), reduced to at most 100 printable
// runes; "unknown" when nothing usable is present.
func mcpOAuthClientID(sources ...map[string]any) string {
	for _, claims := range sources {
		for _, name := range []string{"azp", "client_id"} {
			raw, _ := claims[name].(string)
			var b strings.Builder
			n := 0
			for _, r := range raw {
				if !unicode.IsPrint(r) {
					continue
				}
				if n == 100 {
					break
				}
				b.WriteRune(r)
				n++
			}
			if v := strings.TrimSpace(b.String()); v != "" {
				return v
			}
		}
	}
	return "unknown"
}
