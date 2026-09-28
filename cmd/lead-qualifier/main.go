// Команда lead-qualifier: `demo` - квалификация лида в консоли с фейковой CRM.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/daniil4545/lead-qualifier/internal/agent"
	"github.com/daniil4545/lead-qualifier/internal/calendar"
	"github.com/daniil4545/lead-qualifier/internal/config"
	"github.com/daniil4545/lead-qualifier/internal/console"
	"github.com/daniil4545/lead-qualifier/internal/crm"
	"github.com/daniil4545/lead-qualifier/internal/dialog"
	"github.com/daniil4545/lead-qualifier/internal/store"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: lead-qualifier demo")
		os.Exit(2)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()})))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runDemo(ctx); err != nil {
		slog.Error("demo failed", "error", err)
		fmt.Fprintln(os.Stderr, "demo failed:", err)
		os.Exit(1)
	}
}

func logLevel() slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.TrimSpace(os.Getenv("LOG_LEVEL")))); err != nil {
		return slog.LevelInfo
	}
	return level
}

func runDemo(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		return err
	}
	client, err := agent.FromCatalog(cfg.CatalogDir, "", cfg.OpenRouter, cfg.Catalog, &http.Client{})
	if err != nil {
		return err
	}
	slots, err := calendar.NewLocal(cfg.Catalog)
	if err != nil {
		return err
	}
	terminal := console.New(os.Stdout)
	leads := crm.NewFake(terminal)
	worker := dialog.New(dialog.Deps{
		Store: db, Agent: client, CRM: leads, Sender: terminal, Calendar: slots, Catalog: cfg.Catalog,
	})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	demoErr := console.Run(ctx, worker, leads, terminal, os.Stdin)
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return demoErr
}
