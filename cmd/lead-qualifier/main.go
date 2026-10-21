// Command lead-qualifier: `demo` - qualification of a lead in the console with a fake CRM.
// `demo --offline` replaces the model with a scripted stub, so no OpenRouter key is needed.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	"github.com/daniil4545/lead-qualifier/internal/stubmodel"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "demo" || len(os.Args) > 3 || (len(os.Args) == 3 && os.Args[2] != "--offline") {
		fmt.Fprintln(os.Stderr, "usage: lead-qualifier demo [--offline]")
		os.Exit(2)
	}
	offline := len(os.Args) == 3
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()})))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runDemo(ctx, os.Stdin, os.Stdout, offline); err != nil {
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

// runDemo reads the lead's lines from in and prints the conversation to out. With offline set,
// the model endpoint is a local scripted stub and the API key is a placeholder.
func runDemo(ctx context.Context, in io.Reader, out io.Writer, offline bool) error {
	if offline {
		url, stop, err := stubmodel.Serve()
		if err != nil {
			return err
		}
		defer stop()
		if err := errors.Join(os.Setenv("OPENROUTER_URL", url), os.Setenv("OPENROUTER_API_KEY", "offline")); err != nil {
			return err
		}
	}
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
	terminal := console.New(out)
	leads := crm.NewFake(terminal)
	worker := dialog.New(dialog.Deps{
		Store: db, Agent: client, CRM: leads, Sender: terminal, Calendar: slots, Catalog: cfg.Catalog,
	})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	demoErr := console.Run(ctx, worker, leads, terminal, in)
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return demoErr
}
