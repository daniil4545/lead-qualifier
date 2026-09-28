// Команда prompteval прогоняет эталоны каталога через живую модель по n раз и
// печатает отчёт: доля прошедших, классы расхождений, опасные исходы. В базу
// не пишет, только исходящие вызовы к модели.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/daniil4545/lead-qualifier/internal/agent"
	"github.com/daniil4545/lead-qualifier/internal/config"
	"github.com/daniil4545/lead-qualifier/internal/eval"
)

func main() {
	catalogDir := flag.String("catalog", "data/example", "catalog directory")
	dir := flag.String("dir", "", "fixtures directory; empty - <catalog>/golden")
	prompts := flag.String("prompts", "", "candidate prompts directory; empty - <catalog>/prompts")
	runs := flag.Int("n", 3, "runs per fixture")
	pattern := flag.String("fixtures", "", "comma-separated globs over case_id")
	out := flag.String("out", "", "path for the JSON report")
	model := flag.String("model", "", "model override; empty - OPENROUTER_MODEL or catalog model")
	flag.Parse()

	// Логи хода уходят в stderr только от предупреждений: отчёт читается в stdout.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	options := options{catalogDir: *catalogDir, dir: *dir, prompts: *prompts, runs: *runs,
		pattern: *pattern, out: *out, model: *model}
	if err := run(ctx, options); err != nil {
		fmt.Fprintln(os.Stderr, "prompteval:", err)
		os.Exit(1)
	}
}

type options struct {
	catalogDir, dir, prompts string
	runs                     int
	pattern, out, model      string
}

func run(ctx context.Context, opts options) error {
	if opts.runs < 1 {
		return fmt.Errorf("-n must be at least 1")
	}
	catalog, err := config.LoadCatalog(opts.catalogDir)
	if err != nil {
		return err
	}
	cfg, err := config.LoadOpenRouter(catalog)
	if err != nil {
		return err
	}
	if opts.model != "" {
		cfg.Model = opts.model
	}
	client, err := agent.FromCatalog(opts.catalogDir, opts.prompts, cfg, catalog, &http.Client{})
	if err != nil {
		return err
	}
	if opts.dir == "" {
		opts.dir = filepath.Join(opts.catalogDir, "golden")
	}
	fixtures, err := eval.LoadFixtures(opts.dir)
	if err != nil {
		return err
	}
	if fixtures, err = filterFixtures(fixtures, opts.pattern); err != nil {
		return err
	}
	results := make([]fixtureRuns, 0, len(fixtures))
	for index, fixture := range fixtures {
		entry := fixtureRuns{caseID: fixture.CaseID, class: fixture.Class, action: fixture.ExpectedAction}
		for attempt := 1; attempt <= opts.runs; attempt++ {
			turn, err := client.RunTurn(ctx, fixture.DecisionInput(int64(index+1)))
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				// Причину транспортного сбоя (429, 402, таймаут) видно только здесь:
				// в отчёте это один класс transport_error.
				fmt.Fprintf(os.Stderr, "prompteval: transport error on %s run %d: %v\n", fixture.CaseID, attempt, err)
				entry.runs = append(entry.runs, runResult{transport: true})
				continue
			}
			entry.calls += turn.Trace.Calls
			issues := eval.Evaluate(fixture, turn)
			entry.runs = append(entry.runs, newRunResult(eval.Action(turn), issues))
		}
		results = append(results, entry)
	}
	report := aggregate(reportMeta{model: cfg.Model, prompts: promptsLabel(opts), runs: opts.runs,
		timestamp: time.Now().UTC().Format(time.RFC3339)}, results)
	fmt.Print(renderTable(report))
	if opts.out != "" {
		if err := writeReport(opts.out, report); err != nil {
			return err
		}
	}
	// Критерий «без технических сбоев»: прогон с упавшим транспортом не
	// меряет промпт, и CI обязан это увидеть.
	if report.TransportErrors > 0 {
		return fmt.Errorf("%d runs failed on transport", report.TransportErrors)
	}
	return nil
}

func promptsLabel(opts options) string {
	if opts.prompts != "" {
		return opts.prompts
	}
	return filepath.Join(opts.catalogDir, "prompts")
}

// filterFixtures отбирает эталоны по списку glob через запятую; пустой список
// пропускает всё, пустой результат - ошибка.
func filterFixtures(all []eval.Fixture, pattern string) ([]eval.Fixture, error) {
	var patterns []string
	for _, part := range strings.Split(pattern, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			patterns = append(patterns, trimmed)
		}
	}
	if len(patterns) == 0 {
		return all, nil
	}
	var selected []eval.Fixture
	for _, fixture := range all {
		for _, glob := range patterns {
			matched, err := filepath.Match(glob, fixture.CaseID)
			if err != nil {
				return nil, fmt.Errorf("bad -fixtures pattern %q: %w", glob, err)
			}
			if matched {
				selected = append(selected, fixture)
				break
			}
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("no fixtures match %q", pattern)
	}
	return selected, nil
}
