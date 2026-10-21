// Package eval - dialog golden fixtures and evaluation of an agent turn against them. A fixture is one
// turn: history, a trusted case snapshot and the expected outcome. The package is shared by
// the offline fixture check and prompteval.
package eval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/daniil4545/lead-qualifier/internal/agent"
	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

// Fixture - a golden fixture of one turn.
type Fixture struct {
	CaseID string `json:"case_id"`
	// Class - the scenario class from the spec: the prompteval report is built by it.
	Class      string         `json:"class"`
	CaseStatus qualify.Status `json:"case_status"`
	// Now - the turn moment in the catalog time zone: the model counts dates from it.
	Now          time.Time       `json:"now"`
	LeadName     string          `json:"lead_name"`
	Messages     []Message       `json:"messages"`
	TrustedFacts qualify.Facts   `json:"trusted_facts"`
	OfferedSlots []OfferedSlot   `json:"offered_slots"`
	Meeting      *FixtureMeeting `json:"meeting"`
	// ExpectedAction - the turn outcome, see Action.
	ExpectedAction string `json:"expected_action"`
	// ExpectedFacts - facts after the turn: null means the fact must not exist, a string is
	// the meaning that must be in the value (or ExpectedFactParts), bool is
	// the exact value.
	ExpectedFacts     map[string]any      `json:"expected_facts"`
	ExpectedFactParts map[string][]string `json:"expected_fact_parts"`
	// ExpectedToolArgs - arguments of the tool that ended the turn, as strings.
	ExpectedToolArgs map[string]string `json:"expected_tool_args"`
	// ForbiddenResponse - substrings that must not be in the text to the lead.
	ForbiddenResponse []string `json:"forbidden_response"`
	Reason            string   `json:"reason"`
}

// Message - a history message: author lead or bot.
type Message struct {
	Author string `json:"author"`
	Text   string `json:"text"`
}

// OfferedSlot - a window already shown to the lead.
type OfferedSlot struct {
	ManagerID string    `json:"manager_id"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
}

// FixtureMeeting - the case's appointed meeting.
type FixtureMeeting struct {
	ManagerID string    `json:"manager_id"`
	Manager   string    `json:"manager"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
}

// Actions - the dictionary of turn outcomes.
var Actions = []string{"agent_text", "complete", "manual_review", "offer_slots", "book_slot", "stop_contact", "refuse"}

// LoadFixtures strictly reads all *.json of the folder in lexicographic order.
// An unknown field is an error: a typo in a fixture would otherwise silently disable a check.
func LoadFixtures(dir string) ([]Fixture, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("list fixtures: %w", err)
	}
	var fixtures []Fixture
	for _, path := range paths {
		fixture, err := loadFixture(path)
		if err != nil {
			return nil, err
		}
		fixtures = append(fixtures, fixture)
	}
	if len(fixtures) == 0 {
		return nil, fmt.Errorf("no fixtures in %s", dir)
	}
	return fixtures, nil
}

func loadFixture(path string) (Fixture, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: a fixture file of the operator's catalog
	if err != nil {
		return Fixture{}, fmt.Errorf("read fixture %s: %w", path, err)
	}
	var fixture Fixture
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		return Fixture{}, fmt.Errorf("decode fixture %s: %w", path, err)
	}
	if !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
		return Fixture{}, fmt.Errorf("decode fixture %s: trailing data", path)
	}
	if !slices.Contains(Actions, fixture.ExpectedAction) {
		return Fixture{}, fmt.Errorf("fixture %s: unknown expected_action %q", path, fixture.ExpectedAction)
	}
	return fixture, nil
}

// DecisionInput builds the turn input from a fixture.
func (f Fixture) DecisionInput(caseID int64) qualify.DecisionInput {
	status := f.CaseStatus
	if status == "" {
		status = qualify.StatusCollecting
	}
	facts := f.TrustedFacts
	if facts == nil {
		facts = qualify.Facts{}
	}
	input := qualify.DecisionInput{
		Case: qualify.Case{ID: caseID, Status: status, Facts: facts},
		Lead: qualify.Lead{Name: f.LeadName},
		Now:  f.Now,
	}
	for index, message := range f.Messages {
		direction := qualify.DirectionOut
		if message.Author == "lead" {
			direction = qualify.DirectionIn
		}
		id := int64(index + 1)
		input.Messages = append(input.Messages, qualify.Message{ID: id, CaseID: caseID, Direction: direction, Text: message.Text})
		if direction == qualify.DirectionIn {
			input.LastIncomingMessageID = id
		}
	}
	for _, slot := range f.OfferedSlots {
		input.OfferedSlots = append(input.OfferedSlots, qualify.OfferedSlot{ManagerID: slot.ManagerID, Start: slot.Start, End: slot.End})
	}
	if f.Meeting != nil {
		input.Meeting = &qualify.Meeting{CaseID: caseID, ManagerID: f.Meeting.ManagerID,
			Start: f.Meeting.Start, End: f.Meeting.End, Status: qualify.MeetingScheduled}
		input.ManagerName = f.Meeting.Manager
	}
	return input
}

// Action - the turn outcome in the fixture's words.
func Action(turn agent.Turn) string {
	switch turn.Terminal {
	case agent.TerminalComplete:
		return "complete"
	case agent.TerminalManualReview:
		return "manual_review"
	case agent.TerminalOfferSlots:
		return "offer_slots"
	case agent.TerminalBookSlot:
		return "book_slot"
	case agent.TerminalStopContact:
		return "stop_contact"
	case agent.TerminalRefuse:
		return "refuse"
	}
	return "agent_text"
}

// Issue - one divergence of a turn from the fixture.
type Issue struct {
	Class  string
	Detail string
}

// Divergence classes. The unsafe ones are those where the defect goes outward.
const (
	IssueWrongAction   = "wrong_action"
	IssueWrongToolArgs = "wrong_tool_args"
	IssueForbiddenText = "forbidden_text"
	IssueInventedFact  = "invented_fact"
	IssueMissedFact    = "missed_fact"
	IssueGuardLeak     = "guard_leak"
	IssueCriticLeak    = "critic_leak"
)

// containsForbidden reports whether text contains the forbidden part. A part that starts with
// a digit is not taken from the middle of a number: "12 000" does not lie in "112 000".
func containsForbidden(text, part string) bool {
	if part[0] < '0' || part[0] > '9' {
		return strings.Contains(text, part)
	}
	for from := 0; ; {
		at := strings.Index(text[from:], part)
		if at < 0 {
			return false
		}
		at += from
		if at == 0 || text[at-1] < '0' || text[at-1] > '9' {
			return true
		}
		from = at + 1
	}
}

// Evaluate compares the observed turn outcome with the fixture. An empty answer means the turn
// passed.
func Evaluate(fixture Fixture, turn agent.Turn) []Issue {
	var issues []Issue
	lowered := strings.ToLower(turn.FinalText)
	for _, part := range fixture.ForbiddenResponse {
		if trimmed := strings.ToLower(strings.TrimSpace(part)); trimmed != "" && containsForbidden(lowered, trimmed) {
			issues = append(issues, Issue{IssueForbiddenText, fmt.Sprintf("final text contains %q", part)})
		}
	}
	if turn.FinalText != "" {
		if len(turn.Trace.Guard) > 0 {
			issues = append(issues, Issue{IssueGuardLeak, "final text set despite guard violations"})
		}
		if last := len(turn.Trace.Critic); last > 0 && !turn.Trace.Critic[last-1].OK {
			issues = append(issues, Issue{IssueCriticLeak, "final text set despite critic rejection"})
		}
	}
	if action := Action(turn); action != fixture.ExpectedAction {
		issues = append(issues, Issue{IssueWrongAction,
			fmt.Sprintf("action %q, want %q (reason %q)", action, fixture.ExpectedAction, turn.Reason)})
	}
	issues = append(issues, evaluateToolArgs(fixture, turn)...)
	return append(issues, evaluateFacts(fixture, turn.Facts)...)
}

// Unsafe - the defect would go outward: an invented fact, forbidden text, text
// bypassing the guard or critic, a decision on a wrong facts snapshot. An action
// divergence from the label is a quality question, not harm.
func Unsafe(action string, issues []Issue) bool {
	snapshotDefect := false
	for _, issue := range issues {
		switch issue.Class {
		case IssueInventedFact, IssueForbiddenText, IssueGuardLeak, IssueCriticLeak:
			return true
		case IssueMissedFact:
			snapshotDefect = true
		}
	}
	return snapshotDefect && (action == "complete" || action == "agent_text")
}

func evaluateToolArgs(fixture Fixture, turn agent.Turn) []Issue {
	if len(fixture.ExpectedToolArgs) == 0 {
		return nil
	}
	var args map[string]any
	if rounds := turn.Trace.Rounds; len(rounds) > 0 {
		args = rounds[len(rounds)-1].Arguments
	}
	var issues []Issue
	names := make([]string, 0, len(fixture.ExpectedToolArgs))
	for name := range fixture.ExpectedToolArgs {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		want := fixture.ExpectedToolArgs[name]
		got, ok := args[name]
		if !ok {
			issues = append(issues, Issue{IssueWrongToolArgs, fmt.Sprintf("tool arg %s is missing, want %q", name, want)})
			continue
		}
		if text := normalizeArg(fmt.Sprint(got)); text != normalizeArg(want) {
			issues = append(issues, Issue{IssueWrongToolArgs, fmt.Sprintf("tool arg %s = %q, want %q", name, text, want)})
		}
	}
	return issues
}

// normalizeArg reduces a time to HH:MM: a bare hour equals HH:00.
func normalizeArg(value string) string {
	value = strings.TrimSpace(value)
	hour, minute, ok := agent.ParseClock(value)
	if !ok {
		return value
	}
	return fmt.Sprintf("%02d:%02d", hour, minute)
}

func evaluateFacts(fixture Fixture, facts qualify.Facts) []Issue {
	var issues []Issue
	for _, name := range qualify.RequiredFacts {
		expected := fixture.ExpectedFacts[name]
		got, exists := facts[name]
		empty := !exists || got == nil || strings.TrimSpace(fmt.Sprint(got)) == ""
		switch want := expected.(type) {
		case nil:
			if !empty {
				issues = append(issues, Issue{IssueInventedFact, fmt.Sprintf("fact %s = %#v, want none", name, got)})
			}
		case bool:
			if value, ok := got.(bool); !ok || value != want {
				issues = append(issues, Issue{IssueMissedFact, fmt.Sprintf("fact %s = %#v, want %v", name, got, want)})
			}
		case string:
			if empty {
				issues = append(issues, Issue{IssueMissedFact, fmt.Sprintf("fact %s is empty, want %q", name, want)})
				continue
			}
			value := strings.ToLower(fmt.Sprint(got))
			parts := fixture.ExpectedFactParts[name]
			if len(parts) == 0 {
				parts = []string{want}
			}
			for _, part := range parts {
				if !strings.Contains(value, strings.ToLower(part)) {
					issues = append(issues, Issue{IssueMissedFact, fmt.Sprintf("fact %s = %q, want meaning %q", name, value, part)})
				}
			}
		}
	}
	return issues
}
