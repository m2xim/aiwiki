package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	readyCacheTTL     = 5 * time.Second
	readyCheckTimeout = 2 * time.Second
	readyLogInterval  = 30 * time.Second
	readyGrace        = 500 * time.Millisecond
	// readyProbeKey is the object whose HEAD proves the bucket answers. It need
	// not exist: "not found" is an answer, any other error is not.
	readyProbeKey = "readiness-probe"
)

// readiness caches the result of the dependency checks for readyCacheTTL and
// shares one running check between concurrent callers.
type readiness struct {
	checks map[string]func(context.Context) error // by dependency name
	now    func() time.Time

	mu      sync.Mutex
	have    bool
	result  bool
	at      time.Time
	flight  *readyFlight
	lastLog map[string]time.Time
}

type readyFlight struct {
	done   chan struct{}
	result bool
}

func newReadiness(checks map[string]func(context.Context) error) *readiness {
	return &readiness{checks: checks, now: time.Now, lastLog: map[string]time.Time{}}
}

// ok returns the cached result or runs one shared check. A caller whose own
// context ends first gets false without cancelling the shared check.
func (rd *readiness) ok(ctx context.Context) bool {
	rd.mu.Lock()
	if rd.have && rd.now().Sub(rd.at) < readyCacheTTL {
		res := rd.result
		rd.mu.Unlock()
		return res
	}
	f := rd.flight
	if f == nil {
		f = &readyFlight{done: make(chan struct{})}
		rd.flight = f
		go rd.run(f)
	}
	rd.mu.Unlock()
	select {
	case <-f.done:
		return f.result
	case <-ctx.Done():
		return false
	}
}

func (rd *readiness) run(f *readyFlight) {
	type outcome struct {
		name string
		err  error
	}
	results := make(chan outcome, len(rd.checks))
	for name, check := range rd.checks {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), readyCheckTimeout)
			defer cancel()
			var err error
			func() {
				defer func() {
					if p := recover(); p != nil {
						err = errors.New("check panicked")
					}
				}()
				err = check(ctx)
			}()
			results <- outcome{name, err}
		}()
	}
	good := true
	pending := map[string]bool{}
	for name := range rd.checks {
		pending[name] = true
	}
	// A check that ignores its context must not keep the flight open forever.
	deadline := time.NewTimer(readyCheckTimeout + readyGrace)
	defer deadline.Stop()
	for len(pending) > 0 {
		select {
		case o := <-results:
			delete(pending, o.name)
			if o.err != nil {
				good = false
				rd.logFailure(o.name, o.err)
			}
		case <-deadline.C:
			good = false
			for name := range pending {
				rd.logFailure(name, errors.New("check did not return in time"))
			}
			pending = nil
		}
	}
	rd.mu.Lock()
	rd.have, rd.result, rd.at = true, good, rd.now()
	rd.flight = nil
	rd.mu.Unlock()
	f.result = good
	close(f.done)
}

// logFailure writes the reason to the server log, at most once per
// readyLogInterval per dependency.
func (rd *readiness) logFailure(name string, err error) {
	rd.mu.Lock()
	now := rd.now()
	last, seen := rd.lastLog[name]
	if seen && now.Sub(last) < readyLogInterval {
		rd.mu.Unlock()
		return
	}
	rd.lastLog[name] = now
	rd.mu.Unlock()
	log.Printf("readiness: %s check failed: %v", name, err)
}

// readiness returns the server's readiness checker, creating a private,
// uncached-across-requests one when Handler has not set it up (direct callers
// of routes in tests).
func (s *Server) readiness() *readiness {
	if s.ready != nil {
		return s.ready
	}
	return s.newReadiness()
}

func (s *Server) newReadiness() *readiness {
	checks := map[string]func(context.Context) error{}
	if s.DB != nil && s.DB.pool != nil {
		pool := s.DB.pool
		checks["database"] = func(ctx context.Context) error { return pool.PingContext(ctx) }
	}
	if (s.auth != nil || s.personal) && s.objects != nil {
		store := s.objects
		checks["storage"] = func(ctx context.Context) error {
			_, err := store.Stat(ctx, readyProbeKey)
			if err != nil && !errors.Is(err, errObjectNotFound) {
				return err
			}
			return nil
		}
	}
	return newReadiness(checks)
}

// readyHandler serves GET /api/ready. The body never names a dependency.
func (s *Server) readyHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.readiness().ok(r.Context()) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]bool{"ok": false})
}
