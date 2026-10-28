package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"paygate/internal/app"
	"paygate/internal/platform/config"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "keygen" {
		keygen()
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	a, err := app.Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	if err := a.Start(ctx); err != nil {
		a.Stop(context.Background())
		return err
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("paygate started", "addr", cfg.HTTPAddr, "modules", a.ModuleNames(), "bus", cfg.Bus)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	a.Stop(shutdownCtx)
	return nil
}

// keygen prints a new Ed25519 key pair for access tokens:
//
//	go run ./cmd/paygate keygen
//
// Put AUTH_SIGNING_KEY only in the auth service's secret store. Give
// AUTH_PUBLIC_KEYS to every service.
func keygen() {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		panic(err)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	fmt.Printf("AUTH_SIGNING_KEY=%s\nAUTH_PUBLIC_KEYS=%s\n",
		base64.StdEncoding.EncodeToString(seed), base64.StdEncoding.EncodeToString(pub))
}
