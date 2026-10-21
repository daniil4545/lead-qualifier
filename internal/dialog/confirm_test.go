package dialog

import (
	"context"
	"testing"

	"github.com/daniil4545/lead-qualifier/internal/agent"
)

func TestIsConfirmationBlock(t *testing.T) {
	t.Parallel()
	tests := []struct {
		text string
		want bool
	}{
		{"ок", true},
		{"Ок!", true},
		{"Спасибо)", true},
		{"всё ок", true},
		{"Все хорошо, спасибо", true},
		{"ок, до встречи", true},
		{"👍", false},
		{"", false},
		{"ок, а можно перенести?", false},
		{"спасибо, а во сколько?", false},
		{"все время занят", false},
		{"нет", false},
	}
	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			t.Parallel()
			if got := isConfirmationBlock(tc.text); got != tc.want {
				t.Fatalf("isConfirmationBlock(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// With a live booking a bare "ок" is answered by the catalog text and the model is not called
// (the stub agent fails on an unexpected turn); a repeat stays silent; a real question goes to
// the model as usual.
func TestProcessCase_BareConfirmationOfLiveBooking(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	caseID := h.start("lead")
	h.say("lead", "факты", complete(h.facts("3000000", retail)))
	h.say("lead", "вторник 12:00", bookAt(h.facts("3000000", retail), tuesdayNoon))
	if h.meetingOf(caseID).CaseID == 0 {
		t.Fatal("setup: no booking")
	}
	ctx := context.Background()

	sentBefore := h.sender.count()
	if err := h.worker.Ingest(ctx, "lead", "Спасибо!", ""); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	h.turnOf(caseID)
	sent := h.sender.since(sentBefore)
	if len(sent) != 1 || sent[0].text != h.catalog.Texts.MeetingAck {
		t.Fatalf("sent %q, want the one catalog text %q", sent, h.catalog.Texts.MeetingAck)
	}

	sentBefore = h.sender.count()
	if err := h.worker.Ingest(ctx, "lead", "ок", ""); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	h.turnOf(caseID)
	if sent := h.sender.since(sentBefore); len(sent) != 0 {
		t.Fatalf("repeat sent %q, want silence", sent)
	}
	if got := h.meetingOf(caseID); got.CaseID == 0 {
		t.Fatal("the booking must stay")
	}

	// A message with content is a model turn: the stub has no queued result and fails the turn.
	if err := h.worker.Ingest(ctx, "lead", "ок, а можно на час позже?", ""); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	h.agent.queue(caseID, result{turn: agent.Turn{Facts: h.facts("3000000", retail), FinalText: "Подскажите, на какое время хотите перенести?"}})
	h.turnOf(caseID)
	if h.agent.calls[caseID] != 3 {
		t.Fatalf("model turns = %d, want 3 (two before, one for the question)", h.agent.calls[caseID])
	}
}
