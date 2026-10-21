package eval

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/daniil4545/lead-qualifier/internal/agent"
	"github.com/daniil4545/lead-qualifier/internal/calendar"
	"github.com/daniil4545/lead-qualifier/internal/config"
	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

const (
	catalogDir = "../../data/example"
	goldenDir  = "../../data/example/golden"
)

// specClasses - the 15 fixture classes, one fixture per class.
var specClasses = []string{"happy_path", "kb_question", "facts_with_question", "injection", "human_request",
	"stop_contact", "refuse", "price_insist_unknown", "price_insist_known", "off_topic", "not_owner",
	"aggression", "slot_choice", "reschedule", "verdict_request"}

// escalateReasons - the list of escalate reasons in the tool contract.
var escalateReasons = []string{"user_request", "reschedule", "aggression", "other"}

var moneyText = regexp.MustCompile(`^\d[\d ]*$`)

func loadGolden(t *testing.T) ([]Fixture, config.Catalog, qualify.Rules) {
	t.Helper()
	fixtures, err := LoadFixtures(goldenDir)
	if err != nil {
		t.Fatalf("LoadFixtures: %v", err)
	}
	catalog, err := config.LoadCatalog(catalogDir)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	return fixtures, catalog, qualify.NewRules(catalog)
}

func TestGolden_ClassesFromSpec(t *testing.T) {
	t.Parallel()
	fixtures, _, _ := loadGolden(t)
	var classes []string
	for _, f := range fixtures {
		classes = append(classes, f.Class)
	}
	slices.Sort(classes)
	want := slices.Clone(specClasses)
	slices.Sort(want)
	if !slices.Equal(classes, want) {
		t.Fatalf("golden classes = %v, want one per spec class %v", classes, want)
	}
}

// Each fixture is a turn of a live case by the tool table and the catalog data.
func TestGolden_Consistent(t *testing.T) {
	t.Parallel()
	fixtures, catalog, rules := loadGolden(t)
	cal, err := calendar.NewLocal(catalog)
	if err != nil {
		t.Fatalf("calendar: %v", err)
	}
	for _, f := range fixtures {
		t.Run(f.CaseID, func(t *testing.T) {
			t.Parallel()
			checkShape(t, f, catalog)
			checkAction(t, f, rules)
			checkCatalogData(t, f, catalog, rules, cal)
		})
	}
}

func checkShape(t *testing.T, f Fixture, catalog config.Catalog) {
	t.Helper()
	if !f.CaseStatus.Live() {
		t.Errorf("case_status %q is not live: a frozen case has no turn", f.CaseStatus)
	}
	if len(f.Messages) == 0 || f.Messages[len(f.Messages)-1].Author != "lead" {
		t.Errorf("messages must end with a lead reply")
	}
	for i, m := range f.Messages {
		if (m.Author != "lead" && m.Author != "bot") || strings.TrimSpace(m.Text) == "" {
			t.Errorf("message %d: author %q, text %q", i, m.Author, m.Text)
		}
	}
	if _, offset := f.Now.Zone(); offset != zoneOffset(f.Now, catalog) {
		t.Errorf("now %v is not in the catalog timezone %s", f.Now, catalog.Timezone)
	}
	for name := range f.ExpectedFacts {
		if !slices.Contains(qualify.RequiredFacts, name) {
			t.Errorf("expected_facts has unknown fact %q", name)
		}
	}
	if err := qualify.Validate(f.TrustedFacts); err != nil {
		t.Errorf("trusted_facts invalid: %v", err)
	}
	input := f.DecisionInput(7)
	if input.Case.Status != f.CaseStatus || len(input.Messages) != len(f.Messages) || !input.Now.Equal(f.Now) {
		t.Errorf("DecisionInput: status %s, %d messages, now %v; want %s, %d, %v",
			input.Case.Status, len(input.Messages), input.Now, f.CaseStatus, len(f.Messages), f.Now)
	}
	if last := input.Messages[len(input.Messages)-1]; input.LastIncomingMessageID != last.ID || last.Direction != qualify.DirectionIn {
		t.Errorf("DecisionInput: last incoming id %d, last message %+v", input.LastIncomingMessageID, last)
	}
}

func zoneOffset(at time.Time, catalog config.Catalog) int {
	_, offset := at.In(catalog.Location).Zone()
	return offset
}

// checkAction: the action is from the dictionary and allowed in the case status.
func checkAction(t *testing.T, f Fixture, rules qualify.Rules) {
	t.Helper()
	if !slices.Contains(Actions, f.ExpectedAction) {
		t.Fatalf("expected_action %q not in %v", f.ExpectedAction, Actions)
	}
	switch f.ExpectedAction {
	case "offer_slots", "book_slot":
		if block := rules.MeetingBlock(f.CaseStatus, f.TrustedFacts); block != "" {
			t.Errorf("%s expected, but meeting is blocked: %s", f.ExpectedAction, block)
		}
	case "manual_review":
		reason := f.ExpectedToolArgs["reason"]
		if !slices.Contains(escalateReasons, reason) {
			t.Errorf("escalate reason %q not in %v", reason, escalateReasons)
		}
		if (reason == "reschedule") != (f.CaseStatus == qualify.StatusMeetingScheduled) {
			t.Errorf("reschedule is the escalate of a scheduled meeting; status %s, reason %q", f.CaseStatus, reason)
		}
	}
	if f.ExpectedAction == "book_slot" {
		if _, err := time.Parse(qualify.DayFormat, f.ExpectedToolArgs["date"]); err != nil {
			t.Errorf("book_slot date %q: %v", f.ExpectedToolArgs["date"], err)
		}
		if _, err := time.Parse("15:04", f.ExpectedToolArgs["hour"]); err != nil {
			t.Errorf("book_slot hour %q: %v", f.ExpectedToolArgs["hour"], err)
		}
	}
}

// checkCatalogData: niche codes, managers, windows and prices come from the catalog.
func checkCatalogData(t *testing.T, f Fixture, catalog config.Catalog, rules qualify.Rules, cal calendar.Calendar) {
	t.Helper()
	direction, reason := f.TrustedFacts.Value("niche"), f.TrustedFacts.Value("niche_off")
	if direction != "" || reason != "" {
		gotDirection, gotReason := rules.NormalizeVerdict(qualify.NicheVerdict{Direction: direction, OffProfileReason: reason})
		if gotDirection != direction || gotReason != reason {
			t.Errorf("niche codes (%q, %q) are not catalog codes", direction, reason)
		}
	}
	managers := map[string]string{}
	for _, m := range catalog.Managers {
		managers[m.ID] = m.Name
	}
	length := time.Duration(catalog.MeetingMinutes) * time.Minute
	for _, slot := range f.OfferedSlots {
		if _, ok := managers[slot.ManagerID]; !ok || slot.End.Sub(slot.Start) != length {
			t.Errorf("offered slot %+v: unknown manager or length not %v", slot, length)
		}
		if _, ok := cal.Available(nil, f.Now, slot.Start); !ok {
			t.Errorf("offered slot %v is not a catalog slot at %v", slot.Start, f.Now)
		}
	}
	if m := f.Meeting; m != nil {
		if managers[m.ManagerID] != m.Manager || m.End.Sub(m.Start) != length {
			t.Errorf("meeting %+v: manager or length does not match catalog", m)
		}
	}
	tariff, known := rules.TariffFor(f.TrustedFacts)
	for _, text := range f.ForbiddenResponse {
		if !moneyText.MatchString(text) {
			continue
		}
		amount, _ := strconv.ParseInt(strings.ReplaceAll(text, " ", ""), 10, 64)
		if !rules.IsCatalogPrice(amount) || (known && amount == tariff.Price) {
			t.Errorf("forbidden %q: must be a catalog price other than the lead tariff", text)
		}
	}
}

func TestAction_TerminalNames(t *testing.T) {
	t.Parallel()
	want := map[agent.Terminal]string{
		agent.TerminalNone: "agent_text", agent.TerminalComplete: "complete",
		agent.TerminalManualReview: "manual_review", agent.TerminalOfferSlots: "offer_slots",
		agent.TerminalBookSlot: "book_slot", agent.TerminalStopContact: "stop_contact", agent.TerminalRefuse: "refuse",
	}
	for terminal, name := range want {
		if got := Action(agent.Turn{Terminal: terminal, FinalText: "текст"}); got != name {
			t.Errorf("Action(%s) = %q, want %q", terminal, got, name)
		}
	}
}

func findFixture(t *testing.T, fixtures []Fixture, class string) Fixture {
	t.Helper()
	for _, f := range fixtures {
		if f.Class == class {
			return f
		}
	}
	t.Fatalf("no fixture of class %s", class)
	return Fixture{}
}

func issueClasses(issues []Issue) []string {
	var classes []string
	for _, issue := range issues {
		classes = append(classes, issue.Class)
	}
	return classes
}

// Turn evaluation: a wrong action is a quality question, forbidden text
// and an invented fact are an unsafe outcome.
func TestEvaluate(t *testing.T) {
	t.Parallel()
	fixtures, _, _ := loadGolden(t)
	unknownPrice := findFixture(t, fixtures, "price_insist_unknown")
	refuse := findFixture(t, fixtures, "refuse")
	invented := qualify.Facts{qualify.FactService: "онлайн-казино"}
	cases := []struct {
		name       string
		fixture    Fixture
		turn       agent.Turn
		wantIssue  string // empty means the turn passed
		wantUnsafe bool
	}{
		{"clean refuse", refuse, agent.Turn{Terminal: agent.TerminalRefuse, Facts: qualify.Facts{}}, "", false},
		{"wrong action", refuse, agent.Turn{Terminal: agent.TerminalNone, FinalText: "Понял вас.", Facts: qualify.Facts{}},
			IssueWrongAction, false},
		{"forbidden price in reply", unknownPrice, agent.Turn{Terminal: agent.TerminalNone,
			FinalText: "Тариф стоит " + unknownPrice.ForbiddenResponse[0] + " ₽.", Facts: unknownPrice.TrustedFacts},
			IssueForbiddenText, true},
		{"invented fact", refuse, agent.Turn{Terminal: agent.TerminalRefuse, Facts: invented}, IssueInventedFact, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issues := Evaluate(tc.fixture, tc.turn)
			classes := issueClasses(issues)
			if tc.wantIssue == "" && len(issues) != 0 || tc.wantIssue != "" && !slices.Contains(classes, tc.wantIssue) {
				t.Fatalf("Evaluate = %v, want issue %q", classes, tc.wantIssue)
			}
			if got := Unsafe(Action(tc.turn), issues); got != tc.wantUnsafe {
				t.Errorf("Unsafe(%v) = %v, want %v", classes, got, tc.wantUnsafe)
			}
		})
	}
}
