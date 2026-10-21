// Command prompteval runs the catalog golden fixtures through a live model n times each and
// prints a report: pass rate, divergence classes, unsafe outcomes. It does not write to the
// database, only outgoing calls to the model.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
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
	maxUSD := flag.Float64("max-usd", defaultMaxUSD, "spending ceiling in USD over all model calls, checked before every turn; exit code 3 when reached")
	flag.Parse()

	// Turn logs go to stderr only from warnings: the report is read on stdout.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	options := options{catalogDir: *catalogDir, dir: *dir, prompts: *prompts, runs: *runs,
		pattern: *pattern, out: *out, model: *model, maxUSD: *maxUSD}
	if err := run(ctx, options); err != nil {
		fmt.Fprintln(os.Stderr, "prompteval:", err)
		if errors.Is(err, errBudgetCeiling) {
			os.Exit(3)
		}
		os.Exit(1)
	}
}

type options struct {
	catalogDir, dir, prompts string
	runs                     int
	pattern, out, model      string
	maxUSD                   float64
}

// defaultMaxUSD - the default spending ceiling: a run of the whole catalog should cost cents,
// and a loop in a prompt change should not cost dollars.
const defaultMaxUSD = 2.0

// errBudgetCeiling - the run stopped on -max-usd; the report covers what was done.
var errBudgetCeiling = errors.New("spending ceiling reached")

func run(ctx context.Context, opts options) error {
	if opts.runs < 1 {
		return fmt.Errorf("-n must be at least 1")
	}
	if opts.maxUSD <= 0 || math.IsNaN(opts.maxUSD) || math.IsInf(opts.maxUSD, 0) {
		return fmt.Errorf("-max-usd must be a positive number")
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
	budgetStop := false
	for index, fixture := range fixtures {
		entry := fixtureRuns{caseID: fixture.CaseID, class: fixture.Class, action: fixture.ExpectedAction}
		for attempt := 1; attempt <= opts.runs; attempt++ {
			if client.Spent() >= opts.maxUSD {
				budgetStop = true
				break
			}
			turn, err := client.RunTurn(ctx, fixture.DecisionInput(int64(index+1)))
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				// The cause of a transport failure (429, 402, timeout) is visible only here:
				// in the report it is a single transport_error class.
				fmt.Fprintf(os.Stderr, "prompteval: transport error on %s run %d: %v\n", fixture.CaseID, attempt, err)
				entry.runs = append(entry.runs, runResult{transport: true})
				continue
			}
			entry.calls += turn.Trace.Calls
			issues := eval.Evaluate(fixture, turn)
			entry.runs = append(entry.runs, newRunResult(eval.Action(turn), issues))
		}
		if len(entry.runs) > 0 {
			results = append(results, entry)
		}
		if budgetStop {
			break
		}
	}
	report := aggregate(reportMeta{model: cfg.Model, prompts: promptsLabel(opts), runs: opts.runs,
		timestamp: time.Now().UTC().Format(time.RFC3339),
		costUSD:   client.Spent(), maxUSD: opts.maxUSD, budgetStop: budgetStop}, results)
	fmt.Print(renderTable(report))
	if opts.out != "" {
		if err := writeReport(opts.out, report); err != nil {
			return err
		}
	}
	if budgetStop {
		return fmt.Errorf("%w: $%.4f of $%.2f, %d of %d fixtures done", errBudgetCeiling,
			report.CostUSD, report.MaxUSD, len(results), len(fixtures))
	}
	// The "no technical failures" criterion: a run with a failed transport does not
	// measure the prompt, and CI must see that.
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

// filterFixtures selects fixtures by a comma-separated list of globs; an empty list
// passes everything, an empty result is an error.
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
