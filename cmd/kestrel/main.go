// Command kestrel watches dead-man's switches and raises the alarm in Discord.
//
// Jobs ping a secret URL when they succeed. Cloud Scheduler calls the sweep,
// which announces every check that has gone quiet past its deadline.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dmikalova/kestrel/internal/config"
	"github.com/dmikalova/kestrel/internal/discord"
	"github.com/dmikalova/kestrel/internal/server"
	"github.com/dmikalova/kestrel/internal/store"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 20 * time.Second
	startupTimeout    = 30 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("kestrel stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	secretsPath := flag.String(
		"secrets",
		config.DefaultSecretsPath,
		"path to the mounted config secret",
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	cfg, err := config.Load(*secretsPath)
	if err != nil {
		return fmt.Errorf("load the configuration: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()

	db, err := store.Open(startCtx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	// Terraform owns the check list, so the table is made to match it on every
	// boot. A check removed from Terraform is removed here.
	declared := make([]store.Declared, 0, len(cfg.Checks))
	for _, c := range cfg.Checks {
		declared = append(declared, store.Declared{
			Name:          c.Name,
			Token:         c.Token,
			PeriodSeconds: c.PeriodSeconds,
			GraceSeconds:  c.GraceSeconds,
		})
	}

	if err := db.Reconcile(startCtx, declared); err != nil {
		return err
	}

	log.Info("reconciled the checks", "count", len(declared))

	webhook, err := discord.New(cfg.DiscordWebhook)
	if err != nil {
		return fmt.Errorf("configure Discord: %w", err)
	}

	srv := &http.Server{
		Addr: ":" + cfg.Port,
		Handler: server.New(server.Options{
			Store:          db,
			Discord:        webhook,
			Logger:         log,
			Audience:       cfg.Audience,
			ServiceAccount: cfg.ServiceAccount,
		}).Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	errs := make(chan error, 1)

	go func() {
		log.Info("listening", "port", cfg.Port)

		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down: %w", err)
	}

	return nil
}
