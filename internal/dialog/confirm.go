package dialog

import (
	"context"
	"log/slog"
	"strings"
	"unicode"

	"github.com/daniil4545/lead-qualifier/internal/qualify"
	"github.com/daniil4545/lead-qualifier/internal/store"
)

// confirmWords - the whitelist of confirmation words. A block with any other word is not a
// confirmation: "ок, а можно перенести?" must reach the model.
var confirmWords = map[string]bool{
	"ок": true, "окей": true, "хорошо": true, "отлично": true, "понятно": true,
	"договорились": true, "спасибо": true, "ok": true, "okay": true,
}

// confirmPhrases - multi-word confirmations in normal form: case and "ё" are folded in
// isConfirmationBlock, so the list has no variants.
var confirmPhrases = [][]string{{"до", "встречи"}}

// isConfirmationBlock: the block of incoming messages consists of whitelist words only. Case,
// punctuation and emoji are ignored; a block with no word at all (emoji only, empty text) is
// not a confirmation.
func isConfirmationBlock(block string) bool {
	tokens := strings.FieldsFunc(strings.ReplaceAll(strings.ToLower(block), "ё", "е"),
		func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(tokens) == 0 {
		return false
	}
	for i := 0; i < len(tokens); {
		// "все" is an optional prefix of a list word: "всё ок". Alone or before a foreign
		// word ("все время") it does not pass.
		if tokens[i] == "все" && i+1 < len(tokens) && confirmWords[tokens[i+1]] {
			i++
			continue
		}
		if confirmWords[tokens[i]] {
			i++
			continue
		}
		matched := false
		for _, phrase := range confirmPhrases {
			if i+len(phrase) <= len(tokens) && strings.Join(tokens[i:i+len(phrase)], " ") == strings.Join(phrase, " ") {
				i += len(phrase)
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// acknowledgeMeeting answers a bare confirmation from a lead with a live booking in code, without
// a model call: "ок" or "спасибо" after the booking text needs no reasoning, and a model turn on it
// tends to offer other windows or promise things. The answer is the catalog text. A repeat after
// that text stays silent, so two "спасибо" do not make a conversation of two constants. It
// reports whether the turn was handled.
func (w *Worker) acknowledgeMeeting(ctx context.Context, input qualify.DecisionInput) (bool, error) {
	meeting := input.Meeting
	if meeting == nil || !meeting.End.After(input.Now) || len(input.OfferedSlots) > 0 ||
		!isConfirmationBlock(input.LeadBlock()) {
		return false, nil
	}
	decision := store.Decision{Source: "rule", Status: input.Case.Status, NextAction: "agent_text",
		Reason: "meeting_ack", Facts: input.Case.Facts}
	var plan store.Plan
	if w.lastOutgoing(input) == w.catalog.Texts.MeetingAck {
		decision.Reason = "meeting_ack_repeat"
	} else {
		decision.Message = w.catalog.Texts.MeetingAck
		plan.Reply = &store.Reply{Purpose: "agent_text", Text: decision.Message}
	}
	slog.Info("meeting confirmation answered by code", "case_id", input.Case.ID, "reason", decision.Reason)
	err := w.store.ApplyDecision(ctx, input.Case.ID, input.LastIncomingMessageID, decision, plan)
	return true, w.decisionRace(input.Case.ID, err)
}

// lastOutgoing - the text of our last message to the lead.
func (w *Worker) lastOutgoing(input qualify.DecisionInput) string {
	for index := len(input.Messages) - 1; index >= 0; index-- {
		if input.Messages[index].Direction == qualify.DirectionOut {
			return strings.TrimSpace(input.Messages[index].Text)
		}
	}
	return ""
}
