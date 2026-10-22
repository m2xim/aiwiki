package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Personal mode (docs/PERSONAL.md): the corporate PostgreSQL and S3 data model
// for one built-in owner, without an identity provider.
const (
	// PersonalOwnerID is the id of the only principal. The import command writes
	// the same identity rows.
	PersonalOwnerID     = "owner"
	PersonalIssuer      = "aiwiki:personal"
	PersonalDefaultName = "Владелец"
)

// PersonalConfig configures OpenPersonal.
type PersonalConfig struct {
	S3                   S3Config
	DatabaseURL          string
	MigrationDatabaseURL string
	RuntimeRole          string
	// Name is the display name of the owner; empty means PersonalDefaultName.
	Name string
	// SpaceQuotaBytes is the default storage limit per space; 0 means unlimited.
	SpaceQuotaBytes int64
}

// OpenPersonal opens the corporate data model for the built-in owner. It runs
// the same migrations, grants and startup guards as OpenCorporate, requires S3,
// and then ensures the owner identity rows (see ensurePersonalOwner).
func OpenPersonal(ctx context.Context, cfg PersonalConfig, dataDir, webDir string) (*Server, error) {
	if cfg.S3.Bucket == "" {
		return nil, errors.New("personal mode requires an S3 bucket")
	}
	if cfg.DatabaseURL == "" {
		return nil, errors.New("personal mode requires a database")
	}
	if cfg.SpaceQuotaBytes < 0 {
		return nil, errors.New("space storage quota must not be negative")
	}
	name, err := personalName(cfg.Name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "files"), 0755); err != nil {
		return nil, err
	}
	if err := migrateAndGrant(ctx, cfg.MigrationDatabaseURL, cfg.RuntimeRole, dataDir, webDir); err != nil {
		return nil, err
	}
	db, err := openPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	s := &Server{DB: db, DataDir: dataDir, WebDir: webDir, spaceQuotaDefault: cfg.SpaceQuotaBytes, personal: true, personalName: name}
	fail := func(err error) (*Server, error) {
		db.Close()
		return nil, err
	}
	if s.art, err = newArtifactState(); err != nil {
		return fail(err)
	}
	if err = db.validateRuntimeSchema(); err != nil {
		return fail(err)
	}
	db.warnMovedArticleMismatch()
	db.warnStrayArtifacts()
	if s.objects, err = newS3Store(ctx, cfg.S3); err != nil {
		return fail(err)
	}
	if err = ensurePersonalOwner(ctx, db, name); err != nil {
		return fail(fmt.Errorf("personal owner setup failed: %w", err))
	}
	return s, nil
}

func personalName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return PersonalDefaultName, nil
	}
	if utf8.RuneCountInString(name) > 160 || !utf8.ValidString(name) || strings.ContainsAny(name, "\x00\r\n") {
		return "", errors.New("AIWIKI_PERSONAL_NAME must be at most 160 characters on one line")
	}
	return name, nil
}

// ensurePersonalOwner creates the owner identity idempotently, in one
// transaction on a connection scoped to the owner, using the RUNTIME role:
//
//   - users and memberships have row-level security whose policies apply to the
//     runtime role (it is not the table owner); their only policies are the
//     permissive "no persistent MCP scope" ones, and this connection has no MCP
//     scope, so the writes pass them. A restrictive policy added there later
//     applies to this code too; nothing here is exempt from RLS.
//   - spaces has FORCE RLS. The UPDATE of spaces.owner_id is allowed by the
//     ordinary update policy (role >= manager) only because the owner membership
//     was inserted first in the same transaction; no policy is bypassed.
//   - spaces cannot be enumerated before the memberships exist, so they are
//     read from corporate_space_state, which a trigger keeps in step with
//     spaces (one row per space).
//
// Nothing is granted to any role and nothing in the schema changes. The server
// writes an audit row only when it changed something.
func ensurePersonalOwner(ctx context.Context, db *database, name string) error {
	scoped, done, err := db.scopedMCP(ctx, PersonalOwnerID, nil, "")
	if err != nil {
		return err
	}
	defer done()
	tx, err := scoped.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Two instances starting together must not both create the row.
	if _, err = tx.Exec(`SELECT pg_advisory_xact_lock(742109334)`); err != nil {
		return err
	}
	var issuer, subject, current string
	var admin, disabled int
	created, renamed := false, false
	err = tx.QueryRow(`SELECT issuer,subject,name,admin,disabled FROM users WHERE id=?`, PersonalOwnerID).Scan(&issuer, &subject, &current, &admin, &disabled)
	switch {
	case err == nil:
		if issuer != PersonalIssuer || subject != PersonalOwnerID || admin != 1 || disabled != 0 {
			return errors.New("the user \"owner\" already exists with a different identity, role or state; refusing to take it over")
		}
		if current != name {
			if _, err = tx.Exec(`UPDATE users SET name=? WHERE id=?`, name, PersonalOwnerID); err != nil {
				return err
			}
			renamed = true
		}
	case errors.Is(err, sql.ErrNoRows):
		if _, err = tx.Exec(`INSERT INTO users(id,issuer,subject,name,admin,disabled) VALUES(?,?,?,?,1,0)`, PersonalOwnerID, PersonalIssuer, PersonalOwnerID, name); err != nil {
			return err
		}
		created = true
	default:
		return err
	}
	res, err := tx.Exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role)
SELECT 'space',space_id,'user',?,'owner' FROM corporate_space_state
ON CONFLICT (resource_type,resource_id,subject_type,subject_id) DO UPDATE SET role='owner' WHERE memberships.role<>'owner'`, PersonalOwnerID)
	if err != nil {
		return err
	}
	memberships, _ := res.RowsAffected()
	res, err = tx.Exec(`UPDATE spaces SET owner_id=? WHERE owner_id<>?`, PersonalOwnerID, PersonalOwnerID)
	if err != nil {
		return err
	}
	spaces, _ := res.RowsAffected()
	var invisible int
	if err = tx.QueryRow(`SELECT (SELECT count(*) FROM corporate_space_state)-(SELECT count(*) FROM spaces WHERE owner_id=?)`, PersonalOwnerID).Scan(&invisible); err != nil {
		return err
	}
	if invisible != 0 {
		return fmt.Errorf("%d spaces are not visible to the owner after setup; the database does not match the personal-mode contract", invisible)
	}
	if created || renamed || memberships > 0 || spaces > 0 {
		if err = corporateAudit(tx, PersonalOwnerID, "personal.owner.ensure", "user", PersonalOwnerID, map[string]any{"created": created, "renamed": renamed, "memberships": memberships, "spaces": spaces}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Server) personalPrincipal() Principal {
	return Principal{ID: PersonalOwnerID, Issuer: PersonalIssuer, Subject: PersonalOwnerID, Name: s.personalName, Admin: true}
}

// personalHandler serves every request that passed the Host/Origin checks as the
// owner, on a connection scoped to the owner exactly like an authenticated
// corporate request, so row-level security applies to every query.
func (s *Server) personalHandler() http.Handler {
	public := http.NewServeMux()
	// No login flow, no MCP OAuth discovery, no keys or delegations in v1.
	for _, prefix := range []string{"/auth/", "/.well-known/", "/api/mcp/"} {
		public.HandleFunc(prefix, personalNotFound)
	}
	public.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	public.HandleFunc("GET /api/ready", s.readyHandler)
	public.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p := s.personalPrincipal()
		writeJSON(w, 200, map[string]any{"mode": "personal", "user": map[string]any{"id": p.ID, "name": p.Name, "admin": p.Admin}})
	})
	public.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protected := strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/files/") || authIsMCPPath(r.URL.Path)
		if !protected {
			s.routes().ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		p := s.personalPrincipal()
		scoped, cleanup, err := s.DB.scopedMCP(r.Context(), p.ID, nil, "")
		if err != nil {
			apiErr(w, 503, "database unavailable")
			return
		}
		defer cleanup()
		requestServer := *s
		requestServer.DB = scoped
		requestServer.principal = &p
		requestServer.routes().ServeHTTP(w, r)
	}))
	inner := trackMux(public)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A header that is silently ignored would suggest protection that does
		// not exist: personal mode has no keys.
		if _, present := r.Header["Authorization"]; present {
			w.Header().Set("Cache-Control", "no-store")
			apiErr(w, http.StatusUnauthorized, "authorization headers are not supported in personal mode")
			return
		}
		inner.ServeHTTP(w, r)
	})
}

func personalNotFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	apiErr(w, http.StatusNotFound, "not found")
}
