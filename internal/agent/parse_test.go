package agent

import (
	"context"
	"testing"
	"time"

	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

const textReply = "Хорошо, спасибо за ответ."

// Parsing of recorded OpenRouter answers. A broken answer is a model failure: an error
// or a technical reason, no text for the lead, dialog replays the turn. A one-off
// empty answer and an unknown tool the model fixes within the same turn.
func TestRunTurn_RecordedResponses(t *testing.T) {
	t.Parallel()
	fx := loadFixture(t)
	serviceOnly := qualify.Merge(qualify.Facts{}, qualify.FactDelta{Service: strPtr("сеть магазинов")})
	input := turnInput(qualify.StatusCollecting, serviceOnly, "Сколько длится подключение?")
	cases := []struct {
		name      string
		script    []string
		fallback  string
		technical bool // model failure: a turn error or a reason from TechnicalReasons
	}{
		{"broken json", []string{"broken_json.json"}, "", true},
		{"empty choices", []string{"empty_choices.json"}, "", true},
		{"empty content once", []string{"empty_content.json"}, "", false},
		{"empty content always", nil, "empty_content.json", true},
		{"unknown tool once", []string{"unknown_tool.json"}, "", false},
		{"unknown tool always", nil, "unknown_tool.json", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := fx.clientWith(t, &fakeOpenRouter{script: tc.script, fallback: tc.fallback})
			turn, err := client.RunTurn(context.Background(), input)
			technical := err != nil || TechnicalReasons[turn.Reason]
			if technical != tc.technical {
				t.Fatalf("err = %v, reason = %q: technical failure %v, want %v", err, turn.Reason, technical, tc.technical)
			}
			if tc.technical && turn.FinalText != "" {
				t.Errorf("failed turn carries text %q for the lead", turn.FinalText)
			}
			if !tc.technical && (turn.Terminal != TerminalNone || turn.FinalText != textReply) {
				t.Errorf("terminal %s, text %q, want the model reply %q", turn.Terminal, turn.FinalText, textReply)
			}
		})
	}
}

// The book_slot time: HH:MM or a bare hour, in the catalog time zone; anything else is not a booking.
func TestParseRequestedAt(t *testing.T) {
	t.Parallel()
	location := time.FixedZone("UTC+2", 2*3600)
	cases := []struct {
		date, clock string
		want        time.Time // zero means not recognized
	}{
		{"2026-10-06", "9", time.Date(2026, time.October, 6, 9, 0, 0, 0, location)},
		{"2026-10-06", "09:30", time.Date(2026, time.October, 6, 9, 30, 0, 0, location)},
		{"2026-10-06", "17:00", time.Date(2026, time.October, 6, 17, 0, 0, 0, location)},
		{"2026-10-06", "9:5", time.Time{}},
		{"2026-10-06", "24:00", time.Time{}},
		{"2026-10-06", "09:60", time.Time{}},
		{"2026-10-06", "09:30:00", time.Time{}},
		{"2026-10-06", "в полдень", time.Time{}},
		{"2026-10-06", "", time.Time{}},
		{"завтра", "09:30", time.Time{}},
		{"", "09:30", time.Time{}},
	}
	for _, tc := range cases {
		got, ok := parseRequestedAt(tc.date, tc.clock, location)
		if ok != !tc.want.IsZero() || ok && !got.Equal(tc.want) {
			t.Errorf("parseRequestedAt(%q, %q) = %v, %v; want %v", tc.date, tc.clock, got, ok, tc.want)
		}
	}
}
