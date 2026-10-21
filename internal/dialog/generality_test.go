package dialog

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/daniil4545/lead-qualifier/internal/agent"
	"github.com/daniil4545/lead-qualifier/internal/config"
	"github.com/daniil4545/lead-qualifier/internal/knowledge"
	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

// A second catalog without code changes: one scenario over both catalogs, all
// thresholds, prices, time zone and meeting duration come from the loaded catalog.
func TestCatalogGenerality(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{exampleCatalog, "../../testdata/catalog-alt"} {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			t.Parallel()
			catalog, err := config.LoadCatalog(dir)
			if err != nil {
				t.Fatalf("load catalog %s: %v", dir, err)
			}
			t.Run("tariffs", func(t *testing.T) { t.Parallel(); checkTariffs(t, catalog) })
			t.Run("knowledge", func(t *testing.T) { t.Parallel(); checkKnowledge(t, dir, catalog) })
			t.Run("gate", func(t *testing.T) { t.Parallel(); checkGate(t, dir, catalog) })
			t.Run("booking", func(t *testing.T) { t.Parallel(); checkBooking(t, dir, catalog) })
		})
	}
}

func minTariffThreshold(catalog config.Catalog) int64 {
	least := catalog.Tariffs[0].Threshold
	for _, tariff := range catalog.Tariffs {
		least = min(least, tariff.Threshold)
	}
	return least
}

func maxTariffThreshold(catalog config.Catalog) int64 {
	most := catalog.Tariffs[0].Threshold
	for _, tariff := range catalog.Tariffs {
		most = max(most, tariff.Threshold)
	}
	return most
}

// catalogFacts - a full set of the lead's facts with revenue and the niche verdict.
func catalogFacts(rules qualify.Rules, revenue int64, verdict qualify.NicheVerdict) qualify.Facts {
	s := func(v string) *string { return &v }
	owner := true
	facts := qualify.Merge(qualify.Facts{}, qualify.FactDelta{Service: s("чем занимается лид"),
		Revenue: s(strconv.FormatInt(revenue, 10)), Role: s("директор"), IsOwner: &owner, City: s("Город")})
	rules.ApplyVerdict(facts, verdict)
	return facts
}

// Tariff by revenue: the threshold is inclusive, below it is the previous tariff or none.
func checkTariffs(t *testing.T, catalog config.Catalog) {
	rules := qualify.NewRules(catalog)
	tariffs := slices.Clone(catalog.Tariffs)
	slices.SortFunc(tariffs, func(a, b config.Tariff) int { return int(a.Threshold - b.Threshold) })
	verdict := qualify.NicheVerdict{Direction: catalog.Segments[0].Code}
	for i, tariff := range tariffs {
		below := ""
		if i > 0 {
			below = tariffs[i-1].Name
		}
		for _, row := range []struct {
			revenue int64
			want    string
		}{{tariff.Threshold - 1, below}, {tariff.Threshold, tariff.Name}, {tariff.Threshold + 1, tariff.Name}} {
			got, ok := rules.TariffFor(catalogFacts(rules, row.revenue, verdict))
			if !ok {
				got.Name = ""
			}
			if got.Name != row.want {
				t.Errorf("TariffFor(revenue=%d) = %q, want %q", row.revenue, got.Name, row.want)
			}
		}
	}
}

// The catalog base passes lint on its prices, links and stop dictionary, the prompts
// are built with its company.
func checkKnowledge(t *testing.T, dir string, catalog config.Catalog) {
	kb, err := knowledge.Load(dir)
	if err != nil {
		t.Fatalf("load knowledge: %v", err)
	}
	stopNames, err := knowledge.LoadStopNames(dir)
	if err != nil {
		t.Fatalf("load stop names: %v", err)
	}
	if problems := knowledge.Lint(kb, catalog, stopNames); len(problems) != 0 {
		t.Errorf("Lint = %v, want clean", problems)
	}
	prompts, err := agent.LoadPrompts(dir, catalog, kb)
	if err != nil {
		t.Fatalf("load prompts: %v", err)
	}
	for name, text := range map[string]string{"system": prompts.System, "niche": prompts.Niche} {
		if !strings.Contains(text, catalog.Company) || strings.Contains(text, "{company}") {
			t.Errorf("%s prompt: company %q not substituted", name, catalog.Company)
		}
	}
}

type gateRow struct {
	name       string
	revenue    int64
	verdict    qualify.NicheVerdict
	wantStatus qualify.Status
	wantText   string
}

// The gate at the threshold of each segment: the segment threshold or, without it, the minimum
// tariff threshold; the threshold is inclusive. A refusal reason closes the case at any revenue.
func checkGate(t *testing.T, dir string, catalog config.Catalog) {
	var rows []gateRow
	for _, segment := range catalog.Segments {
		threshold := segment.Threshold
		if threshold == 0 {
			threshold = minTariffThreshold(catalog)
		}
		verdict := qualify.NicheVerdict{Direction: segment.Code}
		rows = append(rows,
			gateRow{segment.Code + " threshold-1", threshold - 1, verdict, qualify.StatusRejected, catalog.Texts.TooEarly},
			gateRow{segment.Code + " threshold", threshold, verdict, qualify.StatusReadyToDecide, catalog.Texts.SlotsFooter},
			gateRow{segment.Code + " threshold+1", threshold + 1, verdict, qualify.StatusReadyToDecide, catalog.Texts.SlotsFooter})
	}
	for _, reason := range catalog.RefusalReasons {
		rows = append(rows, gateRow{"refusal " + reason.Code, maxTariffThreshold(catalog) * 10,
			qualify.NicheVerdict{OffProfileReason: reason.Code}, qualify.StatusRejected, catalog.Texts.OffProfile})
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			h := newHarnessFor(t, dir)
			caseID := h.start("lead")
			sentBefore := h.sender.count()
			h.say("lead", "факты", complete(catalogFacts(h.rules, row.revenue, row.verdict)))
			if got := h.status(caseID); got != row.wantStatus {
				t.Errorf("revenue %d, verdict %+v: status = %s, want %s", row.revenue, row.verdict, got, row.wantStatus)
			}
			want := fragment(row.wantText)
			if sent := h.sender.since(sentBefore); len(sent) != 1 || !strings.Contains(sent[0].text, want) {
				t.Errorf("sent %q, want one message containing %q", sent, want)
			}
		})
	}
}

// tuesdayBounds - the earliest start and the latest end of Tuesday hours:
// common and the managers' own.
func tuesdayBounds(t *testing.T, catalog config.Catalog) (earliest, latest time.Time) {
	t.Helper()
	schedules := []config.WorkHours{catalog.WorkHours}
	for _, manager := range catalog.Managers {
		if len(manager.WorkHours) > 0 {
			schedules = append(schedules, manager.WorkHours)
		}
	}
	for _, hours := range schedules {
		open, closing, ok := tuesdayWindow(t, catalog, hours)
		if !ok {
			continue
		}
		if earliest.IsZero() || open.Before(earliest) {
			earliest = open
		}
		if closing.After(latest) {
			latest = closing
		}
	}
	if earliest.IsZero() {
		t.Fatal("catalog has no Tuesday hours")
	}
	return earliest, latest
}

func tuesdayWindow(t *testing.T, catalog config.Catalog, hours config.WorkHours) (open, closing time.Time, ok bool) {
	t.Helper()
	windows, err := config.ParseWindows(hours)
	if err != nil {
		t.Fatalf("parse work hours: %v", err)
	}
	window, ok := windows[time.Tuesday]
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	open = time.Date(2026, time.October, 6, window.StartHour, window.StartMinute, 0, 0, catalog.Location)
	closing = time.Date(2026, time.October, 6, window.EndHour, window.EndMinute, 0, 0, catalog.Location)
	return open, closing, true
}

// Booking in the catalog time zone and working hours for the catalog meeting duration.
// Window grid (owner decision): the first window is the start of the working day, the step is
// the meeting duration, the last ends no later than the end of the day.
func checkBooking(t *testing.T, dir string, catalog config.Catalog) {
	open, closing, ok := tuesdayWindow(t, catalog, catalog.WorkHours)
	if !ok {
		t.Fatal("catalog has no common Tuesday hours")
	}
	earliest, latest := tuesdayBounds(t, catalog)
	duration := time.Duration(catalog.MeetingMinutes) * time.Minute
	for _, row := range []struct {
		name   string
		start  time.Time
		booked bool
	}{
		{"opening of work day", open, true},
		{"second grid step", open.Add(duration), true},
		{"last slot ends at closing", closing.Add(-duration), true},
		{"half step off grid", open.Add(duration / 2), false},
		{"step before any hours", earliest.Add(-duration), false},
		{"slot past any closing", latest, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			h := newHarnessFor(t, dir)
			caseID := h.start("lead")
			facts := catalogFacts(h.rules, maxTariffThreshold(catalog), qualify.NicheVerdict{Direction: catalog.Segments[0].Code})
			h.say("lead", "факты", complete(facts))
			sentBefore := h.sender.count()
			h.say("lead", "вторник", bookAt(facts, row.start))

			meeting := h.meetingOf(caseID)
			sent := h.sender.since(sentBefore)
			if !row.booked {
				if meeting.CaseID != 0 || h.status(caseID) != qualify.StatusReadyToDecide {
					t.Errorf("start %s: meeting %+v, status %s, want no booking",
						row.start.Format("15:04"), meeting, h.status(caseID))
				}
				return
			}
			if !meeting.Start.Equal(row.start) || meeting.End.Sub(meeting.Start) != duration {
				t.Fatalf("start %s: meeting %v-%v, want booked for %v; sent %q",
					row.start.Format("15:04"), meeting.Start, meeting.End, duration, sent)
			}
			wantText := []string{fragment(catalog.Texts.MeetingConfirmed), strconv.Itoa(catalog.MeetingMinutes),
				row.start.In(catalog.Location).Format("15:04")}
			for _, want := range wantText {
				if len(sent) != 1 || !strings.Contains(sent[0].text, want) {
					t.Errorf("sent %q, want one confirmation containing %q", sent, want)
				}
			}
		})
	}
}
