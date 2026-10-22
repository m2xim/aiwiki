package main

import (
	"aiwiki/internal/server"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "check-storage" {
		os.Exit(runCheckStorage(os.Args[2:]))
	}
	addr := flag.String("addr", "127.0.0.1:8090", "HTTP listen address")
	data := flag.String("data", "data", "SQLite and attachment directory")
	web := flag.String("web", "web/dist", "Built frontend directory")
	allowedOrigin := flag.String("allowed-origin", "", "trusted browser origin and remote Host authority (for example http://192.0.2.10:8090)")
	flag.Parse()
	if *allowedOrigin != "" {
		if err := server.ValidateAllowedOrigin(*allowedOrigin); err != nil {
			return err
		}
	}
	accessLog, err := server.ParseAccessLogMode(os.Getenv("AIWIKI_ACCESS_LOG"))
	if err != nil {
		return err
	}
	metricsAddr, metricsToken := os.Getenv("AIWIKI_METRICS_ADDR"), os.Getenv("AIWIKI_METRICS_TOKEN")
	if err := server.ValidateMetricsConfig(metricsAddr, metricsToken); err != nil {
		return err
	}
	var s *server.Server
	databaseURL := os.Getenv("AIWIKI_DATABASE_URL")
	issuer := os.Getenv("AIWIKI_OIDC_ISSUER")
	corporate := databaseURL != "" || issuer != "" || os.Getenv("AIWIKI_OIDC_CLIENT_ID") != "" || os.Getenv("AIWIKI_OIDC_CLIENT_SECRET") != "" || os.Getenv("AIWIKI_MIGRATION_DATABASE_URL") != ""
	for _, setting := range os.Environ() {
		if strings.HasPrefix(setting, "AIWIKI_OIDC_") || strings.HasPrefix(setting, "AIWIKI_S3_") || strings.HasPrefix(setting, "AIWIKI_KEYCLOAK_") || strings.HasPrefix(setting, "AIWIKI_MCP_RESOURCE_URL=") || strings.HasPrefix(setting, "AIWIKI_MCP_INTROSPECTION_") {
			corporate = true
		}
	}
	if corporate {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s, err = server.OpenCorporate(ctx, server.CorporateConfig{
			MCPOAuth:    server.MCPOAuthConfig{ResourceURL: os.Getenv("AIWIKI_MCP_RESOURCE_URL"), IntrospectionClientID: os.Getenv("AIWIKI_MCP_INTROSPECTION_CLIENT_ID"), IntrospectionClientSecret: os.Getenv("AIWIKI_MCP_INTROSPECTION_CLIENT_SECRET")},
			Admission:   server.KeycloakAdmissionConfig{AdminURL: os.Getenv("AIWIKI_KEYCLOAK_ADMIN_URL"), ClientID: os.Getenv("AIWIKI_KEYCLOAK_ADMISSION_CLIENT_ID"), ClientSecret: os.Getenv("AIWIKI_KEYCLOAK_ADMISSION_CLIENT_SECRET")},
			S3:          s3ConfigFromEnv(),
			DatabaseURL: databaseURL, MigrationDatabaseURL: os.Getenv("AIWIKI_MIGRATION_DATABASE_URL"), RuntimeRole: os.Getenv("AIWIKI_DATABASE_ROLE"),
			IssuerURL: issuer, ClientID: os.Getenv("AIWIKI_OIDC_CLIENT_ID"), ClientSecret: os.Getenv("AIWIKI_OIDC_CLIENT_SECRET"), RedirectURL: os.Getenv("AIWIKI_OIDC_REDIRECT_URL"),
			AccessRole: os.Getenv("AIWIKI_OIDC_ACCESS_ROLE"), AdminRole: os.Getenv("AIWIKI_OIDC_ADMIN_ROLE"), RolesClaim: os.Getenv("AIWIKI_OIDC_ROLES_CLAIM"),
			AllowInsecureHTTP: os.Getenv("AIWIKI_ALLOW_INSECURE_LOOPBACK") == "1",
		}, *data, *web)
	} else {
		s, err = server.Open(*data, *web)
	}
	if err != nil {
		return err
	}
	defer s.Close()
	if *allowedOrigin != "" {
		if err := s.SetAllowedOrigin(*allowedOrigin); err != nil {
			return err
		}
	}
	if accessLog {
		s.SetAccessLog(os.Stderr)
	}
	log.Printf("AI Wiki: http://%s", *addr)
	srv := &http.Server{Addr: *addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 5 * time.Minute, IdleTimeout: 90 * time.Second}
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stopped)
	var metricsSrv *http.Server
	if metricsAddr != "" {
		// Bind first: a taken port fails startup cleanly, before the main server runs.
		ln, err := net.Listen("tcp", metricsAddr)
		if err != nil {
			return fmt.Errorf("metrics listener: %w", err)
		}
		log.Printf("AI Wiki metrics: http://%s/metrics", ln.Addr())
		metricsSrv = &http.Server{Handler: s.MetricsHandler(metricsToken), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second}
		// Metrics are non-essential: a later failure is logged, the wiki keeps serving.
		go func() {
			if err := metricsSrv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				log.Printf("metrics listener stopped: %v", err)
			}
		}()
	}
	failure := make(chan error, 1)
	go func() { failure <- srv.ListenAndServe() }()
	select {
	case err := <-failure:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-stopped:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if metricsSrv != nil {
			_ = metricsSrv.Shutdown(ctx)
		}
		if err := srv.Shutdown(ctx); err != nil {
			return err
		}
	}
	return nil
}
