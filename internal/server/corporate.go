package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// OpenCorporate never falls back to trusted-network mode on configuration errors.
// Migrations use a separate owner connection; runtime must own no protected tables.
func OpenCorporate(ctx context.Context, cfg CorporateConfig, dataDir, webDir string) (*Server, error) {
	if cfg.S3.Bucket == "" {
		return nil, errors.New("corporate mode requires an S3 bucket")
	}
	if cfg.DatabaseURL == "" || cfg.IssuerURL == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RedirectURL == "" {
		return nil, errors.New("corporate mode requires database and complete OIDC configuration")
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "files"), 0755); err != nil {
		return nil, err
	}
	if cfg.MigrationDatabaseURL != "" {
		if !validPathID(cfg.RuntimeRole) {
			return nil, errors.New("runtime database role is required for migration")
		}
		migration, err := openPostgres(ctx, cfg.MigrationDatabaseURL)
		if err != nil {
			return nil, err
		}
		owner := &Server{DB: migration, DataDir: dataDir, WebDir: webDir}
		err = owner.migrate()
		if err == nil {
			err = owner.migrateCorporate()
		}
		if err == nil {
			role := `"` + cfg.RuntimeRole + `"`
			_, err = migration.Exec(`GRANT USAGE ON SCHEMA public TO ` + role + `; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO ` + role + `; GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + role + `; GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA public TO ` + role + `; REVOKE TRUNCATE,TRIGGER,REFERENCES ON ALL TABLES IN SCHEMA public FROM ` + role + `; REVOKE UPDATE,DELETE ON security_audit FROM ` + role + `; REVOKE ALL ON schema_migrations FROM ` + role + `; REVOKE EXECUTE ON FUNCTION corporate_owner_effective_role(text,text,text) FROM ` + role)
		}
		migration.Close()
		if err != nil {
			return nil, fmt.Errorf("corporate migration failed: %w", err)
		}
	}
	db, err := openPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	s := &Server{DB: db, DataDir: dataDir, WebDir: webDir}
	fail := func(err error) (*Server, error) {
		if s.authDB != nil {
			s.authDB.Close()
		}
		db.Close()
		return nil, err
	}
	if err = db.validateRuntimeSchema(); err != nil {
		return fail(err)
	}
	db.warnMovedArticleMismatch()
	s.objects, err = newS3Store(ctx, cfg.S3)
	if err != nil {
		return fail(err)
	}
	s.authDB, err = openPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return fail(err)
	}
	s.authDB.pool.SetMaxOpenConns(4)
	s.auth, err = newAuthService(ctx, s.authDB, cfg)
	if err != nil {
		return fail(err)
	}
	redirect, err := url.Parse(cfg.RedirectURL)
	if err != nil {
		return fail(errors.New("invalid OIDC redirect URL"))
	}
	if err = s.SetAllowedOrigin(redirect.Scheme + "://" + redirect.Host); err != nil {
		return fail(err)
	}
	return s, nil
}

func (s *Server) corporateHandler() http.Handler {
	public := http.NewServeMux()
	s.auth.routes(public)
	public.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	public.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protected := strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/files/") || authIsMCPPath(r.URL.Path)
		if !protected {
			s.routes().ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		p, ok := s.auth.authenticate(w, r)
		if !ok {
			return
		}
		mode := p.MCPMode
		if p.MCPProjects != nil && mode == "" {
			mode = "read"
		}
		scoped, cleanup, err := s.DB.scopedMCP(r.Context(), p.ID, p.MCPProjects, mode)
		if err != nil {
			apiErr(w, 503, "database unavailable")
			return
		}
		defer cleanup()
		requestServer := *s
		requestServer.DB = scoped
		requestServer.principal = &p
		// A request owns its scope all the way through the in-process MCP proxy.
		requestServer.routes().ServeHTTP(w, r)
	}))
	return public
}

func (s *Server) changeAuthor(proposed string) string {
	if s.principal != nil {
		if s.principal.MCPActorID != "" {
			return s.principal.MCPActorID
		}
		return s.principal.ID
	}
	return proposed
}

// effectiveRole is evaluated on the request-bound connection, never cached.
func (s *Server) effectiveRole(space, project string) (int, error) {
	if s.principal == nil {
		return 0, sql.ErrNoRows
	}
	var role int
	err := s.DB.QueryRow(`SELECT corporate_role_rank(corporate_effective_role(?,?,?))`, s.principal.ID, space, project).Scan(&role)
	return role, err
}
