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
	s, err := server.Open(*data, *web)
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
	srv := &http.Server{Addr: *addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
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
