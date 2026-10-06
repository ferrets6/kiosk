// SPDX-License-Identifier: MIT

// Command kiosk is the broker: it gives each person their own throwaway
// Chromium on the NAS and proxies them to it.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	// The image is distroless: no zoneinfo, so TZ needs the embedded copy.
	_ "time/tzdata"

	"github.com/ferrets6/kiosk/internal/broker"
)

var version = "dev"

func main() {
	cfg, err := broker.Load(os.Getenv)
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	store, err := broker.OpenStore(cfg.DataDir)
	if err != nil {
		slog.Error("open state", "err", err)
		os.Exit(1)
	}
	b := broker.NewServer(cfg, store)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           b.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go b.Reap(ctx, time.Minute)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	slog.Info("kiosk listening", "addr", cfg.Addr, "version", version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}
