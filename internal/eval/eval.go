// Package eval - эталоны диалога и оценка хода агента по ним. Эталон - один
// ход: история, доверенный снимок кейса и ожидаемый исход. Пакет общий у
// офлайн-проверки эталонов и prompteval.
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

// Fixture - эталон одного хода.
type Fixture struct {
	CaseID string `json:"case_id"`
	// Class - класс сценария из спеки: по нему строится отчёт prompteval.
	Class      string         `json:"class"`
	CaseStatus qualify.Status `json:"case_status"`
	// Now - момент хода в поясе каталога: от него модель считает даты.
	Now          time.Time       `json:"now"`
	LeadName     string          `json:"lead_name"`
	Messages     []Message       `json:"messages"`
	TrustedFacts qualify.Facts   `json:"trusted_facts"`
	OfferedSlots []OfferedSlot   `json:"offered_slots"`
	Meeting      *FixtureMeeting `json:"meeting"`
	// ExpectedAction - исход хода, см. Action.
	ExpectedAction string `json:"expected_action"`
	// ExpectedFacts - факты после хода: null - факта быть не должно, строка -
	// смысл, который обязан быть в значении (или ExpectedFactParts), bool -
	// точное значение.
	ExpectedFacts     map[string]any      `json:"expected_facts"`
	ExpectedFactParts map[string][]string `json:"expected_fact_parts"`
	// ExpectedToolArgs - аргументы завершившего ход инструмента, строками.
	ExpectedToolArgs map[string]string `json:"expected_tool_args"`
	// ForbiddenResponse - подстроки, которых не должно быть в тексте лиду.
	ForbiddenResponse []string `json:"forbidden_response"`
	Reason            string   `json:"reason"`
}

// Message - реплика истории: author lead либо bot.
type Message struct {
	Author string `json:"author"`
	Text   string `json:"text"`
}

// OfferedSlot - окно, уже показанное лиду.
type OfferedSlot struct {
	ManagerID string    `json:"manager_id"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
}

// FixtureMeeting - назначенная встреча кейса.
type FixtureMeeting struct {
	ManagerID string    `json:"manager_id"`
	Manager   string    `json:"manager"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
}

// Actions - словарь исходов хода.
var Actions = []string{"agent_text", "complete", "manual_review", "offer_slots", "book_slot", "stop_contact", "refuse"}

// LoadFixtures строго читает все *.json папки в лексикографическом порядке.
// Неизвестное поле - ошибка: опечатка в эталоне иначе молча выключила бы проверку.
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
	data, err := os.ReadFile(path)
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

// DecisionInput собирает вход хода из эталона.
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

// Action - исход хода словами эталона.
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

// Issue - одно расхождение хода с эталоном.
type Issue struct {
	Class  string
	Detail string
}

// Классы расхождений. unsafe из них - те, где дефект уходит наружу.
const (
	IssueWrongAction   = "wrong_action"
	IssueWrongToolArgs = "wrong_tool_args"
	IssueForbiddenText = "forbidden_text"
	IssueInventedFact  = "invented_fact"
	IssueMissedFact    = "missed_fact"
	IssueGuardLeak     = "guard_leak"
	IssueCriticLeak    = "critic_leak"
)

// Evaluate сравнивает наблюдаемый исход хода с эталоном. Пустой ответ - ход
// прошёл.
func Evaluate(fixture Fixture, turn agent.Turn) []Issue {
	var issues []Issue
	lowered := strings.ToLower(turn.FinalText)
	for _, part := range fixture.ForbiddenResponse {
		if trimmed := strings.ToLower(strings.TrimSpace(part)); trimmed != "" && strings.Contains(lowered, trimmed) {
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

// Unsafe - дефект ушёл бы наружу: выдуманный факт, запрещённый текст, текст в
// обход guard или критика, решение по неверному снимку фактов. Расхождение
// действия с меткой - вопрос качества, а не вреда.
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

// normalizeArg сводит время к HH:MM: голый час равен HH:00.
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
