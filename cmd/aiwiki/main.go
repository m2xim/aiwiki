package main

import (
	"aiwiki/internal/server"
	"context"
	"errors"
	"flag"
	"log"
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
	var s *server.Server
	var err error
	databaseURL := os.Getenv("AIWIKI_DATABASE_URL")
	issuer := os.Getenv("AIWIKI_OIDC_ISSUER")
	corporate := databaseURL != "" || issuer != "" || os.Getenv("AIWIKI_OIDC_CLIENT_ID") != "" || os.Getenv("AIWIKI_OIDC_CLIENT_SECRET") != "" || os.Getenv("AIWIKI_MIGRATION_DATABASE_URL") != ""
	for _, setting := range os.Environ() {
		if strings.HasPrefix(setting, "AIWIKI_OIDC_") || strings.HasPrefix(setting, "AIWIKI_S3_") || strings.HasPrefix(setting, "AIWIKI_KEYCLOAK_") {
			corporate = true
		}
	}
	if corporate {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s, err = server.OpenCorporate(ctx, server.CorporateConfig{
			Admission:   server.KeycloakAdmissionConfig{AdminURL: os.Getenv("AIWIKI_KEYCLOAK_ADMIN_URL"), ClientID: os.Getenv("AIWIKI_KEYCLOAK_ADMISSION_CLIENT_ID"), ClientSecret: os.Getenv("AIWIKI_KEYCLOAK_ADMISSION_CLIENT_SECRET")},
			S3:          server.S3Config{Bucket: os.Getenv("AIWIKI_S3_BUCKET"), Region: os.Getenv("AIWIKI_S3_REGION"), Endpoint: os.Getenv("AIWIKI_S3_ENDPOINT"), Prefix: os.Getenv("AIWIKI_S3_PREFIX"), PathStyle: os.Getenv("AIWIKI_S3_PATH_STYLE") == "1", AllowInsecureLoopback: os.Getenv("AIWIKI_ALLOW_INSECURE_LOOPBACK") == "1"},
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
	log.Printf("AI Wiki: http://%s", *addr)
	srv := &http.Server{Addr: *addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 5 * time.Minute, IdleTimeout: 90 * time.Second}
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stopped)
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
		if err := srv.Shutdown(ctx); err != nil {
			return err
		}
	}
	return nil
}
