package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/daniil4545/lead-qualifier/internal/eval"
)

// ClassTransport - the run failed on transport: the model did not answer. The criterion
// "no technical failures" is zero such runs.
const ClassTransport = "transport_error"

// runResult - the outcome of one run of one fixture.
type runResult struct {
	transport bool
	classes   []string // divergence classes; empty means the run passed
	detail    string   // the first cause to look at
	unsafe    bool
}

func newRunResult(action string, issues []eval.Issue) runResult {
	result := runResult{unsafe: eval.Unsafe(action, issues)}
	for _, issue := range issues {
		result.classes = append(result.classes, issue.Class)
	}
	if len(issues) > 0 {
		result.detail = issues[0].Class + ": " + issues[0].Detail
	}
	return result
}

type fixtureRuns struct {
	caseID, class, action string
	runs                  []runResult
	calls                 int
}

type reportMeta struct {
	model, prompts, timestamp string
	runs                      int
	costUSD, maxUSD           float64
	budgetStop                bool
}

// FixtureStat - the summary of one fixture.
type FixtureStat struct {
	CaseID         string         `json:"case_id"`
	Class          string         `json:"class"`
	Action         string         `json:"action"`
	PassFraction   float64        `json:"pass_fraction"`
	UnsafeFraction float64        `json:"unsafe_fraction"`
	FailMode       string         `json:"fail_mode,omitempty"`
	ClassCounts    map[string]int `json:"class_counts"`
	Sample         string         `json:"sample,omitempty"`
}

// Report - the summary of a run.
type Report struct {
	Model       string  `json:"model"`
	Prompts     string  `json:"prompts"`
	Timestamp   string  `json:"timestamp"`
	Runs        int     `json:"runs"`
	Fixtures    int     `json:"fixtures"`
	OverallPass float64 `json:"overall_pass"`
	// UnsafeRate - the share of fixtures with an unsafe outcome in half of the runs or more.
	UnsafeRate   float64 `json:"unsafe_rate"`
	AlwaysPass   int     `json:"always_pass"`
	AlwaysFail   int     `json:"always_fail"`
	Flaky        int     `json:"flaky"`
	CallsPerTurn float64 `json:"calls_per_turn"`
	// TransportErrors - the number of runs of class transport_error.
	TransportErrors int `json:"transport_errors"`
	// CostUSD - the cost of all model calls of the run; MaxUSD - the ceiling. BudgetStop - the run
	// ended on the ceiling, and the numbers cover only the turns made before it.
	CostUSD     float64        `json:"cost_usd"`
	MaxUSD      float64        `json:"max_usd"`
	BudgetStop  bool           `json:"budget_stop,omitempty"`
	ClassCounts map[string]int `json:"class_counts"`
	Stats       []FixtureStat  `json:"stats"`
}

func aggregate(meta reportMeta, data []fixtureRuns) Report {
	report := Report{Model: meta.model, Prompts: meta.prompts, Timestamp: meta.timestamp, Runs: meta.runs,
		Fixtures: len(data), ClassCounts: map[string]int{},
		CostUSD: meta.costUSD, MaxUSD: meta.maxUSD, BudgetStop: meta.budgetStop}
	passSum, unsafeFixtures, calls, turns := 0.0, 0, 0, 0
	for _, fixture := range data {
		stat := fixtureStat(fixture)
		report.Stats = append(report.Stats, stat)
		passSum += stat.PassFraction
		if stat.UnsafeFraction >= 0.5 {
			unsafeFixtures++
		}
		switch stat.PassFraction {
		case 1:
			report.AlwaysPass++
		case 0:
			report.AlwaysFail++
		default:
			report.Flaky++
		}
		for class, count := range stat.ClassCounts {
			report.ClassCounts[class] += count
		}
		calls += fixture.calls
		for _, run := range fixture.runs {
			if !run.transport {
				turns++
			}
		}
	}
	report.TransportErrors = report.ClassCounts[ClassTransport]
	if len(data) > 0 {
		report.OverallPass = passSum / float64(len(data))
		report.UnsafeRate = float64(unsafeFixtures) / float64(len(data))
	}
	if turns > 0 {
		report.CallsPerTurn = float64(calls) / float64(turns)
	}
	return report
}

func fixtureStat(fixture fixtureRuns) FixtureStat {
	stat := FixtureStat{CaseID: fixture.caseID, Class: fixture.class, Action: fixture.action, ClassCounts: map[string]int{}}
	passed, unsafe := 0, 0
	for _, run := range fixture.runs {
		switch {
		case run.transport:
			stat.ClassCounts[ClassTransport]++
		case len(run.classes) == 0:
			passed++
		}
		if run.unsafe {
			unsafe++
		}
		for _, class := range run.classes {
			stat.ClassCounts[class]++
		}
		if stat.Sample == "" {
			stat.Sample = run.detail
		}
	}
	if total := len(fixture.runs); total > 0 {
		stat.PassFraction = float64(passed) / float64(total)
		stat.UnsafeFraction = float64(unsafe) / float64(total)
	}
	if stat.PassFraction < 1 {
		stat.FailMode = dominantClass(stat.ClassCounts)
	}
	return stat
}

// dominantClass - the most frequent class; on a tie the lexicographically smaller.
func dominantClass(counts map[string]int) string {
	best, bestCount := "", 0
	for class, count := range counts {
		if count > bestCount || (count == bestCount && class < best) {
			best, bestCount = class, count
		}
	}
	return best
}

func renderTable(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "model:            %s\n", r.Model)
	fmt.Fprintf(&b, "prompts:          %s\n", r.Prompts)
	fmt.Fprintf(&b, "fixtures x runs:  %d x %d\n", r.Fixtures, r.Runs)
	fmt.Fprintf(&b, "overall pass:     %.2f\n", r.OverallPass)
	fmt.Fprintf(&b, "unsafe rate:      %.2f\n", r.UnsafeRate)
	fmt.Fprintf(&b, "always/flaky/never: %d/%d/%d\n", r.AlwaysPass, r.Flaky, r.AlwaysFail)
	fmt.Fprintf(&b, "calls per turn:   %.1f\n", r.CallsPerTurn)
	fmt.Fprintf(&b, "%s runs: %d\n", ClassTransport, r.TransportErrors)
	fmt.Fprintf(&b, "cost:             $%.4f of $%.2f ceiling\n", r.CostUSD, r.MaxUSD)
	if r.BudgetStop {
		b.WriteString("STOPPED on the spending ceiling: the numbers cover the turns made so far only\n")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "%-28s %-22s %-14s %5s %6s  %s\n", "case", "class", "action", "pass", "unsafe", "fail mode / sample")
	for _, stat := range r.Stats {
		note := stat.Sample
		if note == "" {
			note = stat.FailMode
		}
		fmt.Fprintf(&b, "%-28s %-22s %-14s %5.2f %6.2f  %s\n", stat.CaseID, stat.Class, stat.Action,
			stat.PassFraction, stat.UnsafeFraction, note)
	}
	if len(r.ClassCounts) > 0 {
		classes := make([]string, 0, len(r.ClassCounts))
		for class := range r.ClassCounts {
			classes = append(classes, class)
		}
		slices.Sort(classes)
		b.WriteString("\nissue classes:\n")
		for _, class := range classes {
			fmt.Fprintf(&b, "  %-18s %d\n", class, r.ClassCounts[class])
		}
	}
	return b.String()
}

func writeReport(path string, report Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}
