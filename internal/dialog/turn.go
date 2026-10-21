package dialog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/daniil4545/lead-qualifier/internal/agent"
	"github.com/daniil4545/lead-qualifier/internal/qualify"
	"github.com/daniil4545/lead-qualifier/internal/store"
)

// ProcessCase - one turn on a claimed case. claimAttempt is the claim number
// from ClaimNextCase: the attempt ceiling is counted by it and deferral is fenced by it.
func (w *Worker) ProcessCase(ctx context.Context, caseID int64, claimAttempt int) error {
	input, err := w.store.LoadDecisionInput(ctx, caseID, w.now())
	if err != nil {
		return fmt.Errorf("load decision input: %w", err)
	}
	if input.LastIncomingMessageID == 0 || !input.Case.Status.Live() {
		slog.Debug("case skipped", "case_id", caseID, "status", string(input.Case.Status))
		return nil
	}
	if input.Meeting != nil {
		input.ManagerName = w.managerName(input.Meeting.ManagerID)
	}
	if handled, err := w.acknowledgeMeeting(ctx, input); handled || err != nil {
		return err
	}
	turn, err := w.agent.RunTurn(ctx, input)
	if err != nil {
		return w.failTurn(ctx, claimAttempt, input, turn, err)
	}
	// A technical turn outcome is a model failure, not a decision: the turn is replayed,
	// and after the attempt ceiling the case goes to a human as an ordinary manual_review.
	if turn.Terminal == agent.TerminalManualReview && agent.TechnicalReasons[turn.Reason] {
		if claimAttempt <= agentMaxAttempts {
			return w.store.FailCase(ctx, caseID, claimAttempt, turn.Reason, "технический исход хода")
		}
		slog.Warn("turn retry ceiling reached", "case_id", caseID, "kind", turn.Reason, "attempts", claimAttempt)
	}
	turnInput := input
	turnInput.Case.Facts = turn.Facts
	if turnInput.Case.Facts == nil {
		turnInput.Case.Facts = qualify.Facts{}
	}
	// The reply was written for the previous decision and could promise a meeting that
	// the gate recomputation is about to cancel.
	if gateNeedsReview(input, turn) {
		turn.FinalText = ""
	}
	decision, plan, err := w.outcome(ctx, turnInput, turn)
	if err != nil {
		return w.failTurn(ctx, claimAttempt, input, turn, err)
	}
	decision.RawResponse = rawResponse(turn)
	err = w.store.ApplyDecision(ctx, caseID, input.LastIncomingMessageID, decision, plan)
	if errors.Is(err, store.ErrSlotTaken) {
		// The window was taken while the turn ran: the booking rolled back together with the decision, and the lead
		// gets fresh windows.
		slog.Warn("meeting slot taken", "case_id", caseID)
		decision, plan, err = w.offerSlots(ctx, turnInput, "", w.catalog.Texts.SlotTaken, zeroTime, zeroTime)
		if err != nil {
			return w.failTurn(ctx, claimAttempt, input, turn, err)
		}
		decision.RawResponse = rawResponse(turn)
		err = w.store.ApplyDecision(ctx, caseID, input.LastIncomingMessageID, decision, plan)
	}
	return w.decisionRace(caseID, err)
}

// decisionRace absorbs ordinary races on apply: a new message arrived during the
// turn, the decision is already applied, the case is closed. The turn replays itself.
func (w *Worker) decisionRace(caseID int64, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrStaleDecision), errors.Is(err, store.ErrAlreadyApplied), errors.Is(err, store.ErrFrozenCase):
		slog.Info("decision race", "case_id", caseID, "reason", err.Error())
		w.Wake()
		return nil
	}
	return fmt.Errorf("apply decision: %w", err)
}

// outcome - the handler of the turn outcome by terminal. Each one builds the decision and
// the whole plan.
func (w *Worker) outcome(ctx context.Context, input qualify.DecisionInput, turn agent.Turn) (store.Decision, store.Plan, error) {
	switch turn.Terminal {
	case agent.TerminalComplete:
		return w.completeTurn(ctx, input, turn)
	case agent.TerminalOfferSlots, agent.TerminalBookSlot:
		// The booking boundary is rechecked before windows and booking: Turn is untrusted, and
		// an agent stub or failure must not yield a meeting that bypasses the gate.
		if block := w.rules.MeetingBlock(input.Case.Status, input.Case.Facts); block != "" {
			slog.Warn("meeting blocked", "case_id", input.Case.ID, "terminal", turn.Terminal.String(), "block", block)
			return w.manualReview(input, "meeting_blocked:"+block, "rule")
		}
		if turn.Terminal == agent.TerminalBookSlot {
			return w.bookSlot(ctx, input, turn.RequestedAt)
		}
		return w.offerSlots(ctx, input, turn.FinalText, "", turn.NotEarlierThan, turn.NotLaterThan)
	case agent.TerminalStopContact:
		return w.closeTurn(input, turn, qualify.StatusStopped, w.catalog.Texts.Stopped, "лид попросил больше не писать")
	case agent.TerminalRefuse:
		return w.closeTurn(input, turn, qualify.StatusRefused, w.catalog.Texts.Refused, "лид отказался")
	case agent.TerminalManualReview:
		return w.manualReview(input, turn.Reason, "llm")
	}
	// The lead corrected a fact after the decision and the model did not call complete again:
	// the gate must still review the decision.
	if gateNeedsReview(input, turn) {
		return w.completeTurn(ctx, input, turn)
	}
	return w.replyTurn(input, turn), store.Plan{Reply: &store.Reply{Purpose: "agent_text", Text: turn.FinalText}}, nil
}

// gateNeedsReview - a turn without a terminal changed the facts of an already decided case.
func gateNeedsReview(input qualify.DecisionInput, turn agent.Turn) bool {
	return turn.Terminal == agent.TerminalNone && input.Case.Status == qualify.StatusReadyToDecide &&
		turn.FactsChanged && len(turn.Facts.Missing()) == 0
}

func (w *Worker) replyTurn(input qualify.DecisionInput, turn agent.Turn) store.Decision {
	return store.Decision{
		Source: "llm", Status: input.Case.Status, NextAction: "agent_text", Reason: "ответ ассистента",
		Facts: input.Case.Facts, Message: turn.FinalText,
	}
}

// completeTurn - the outcome of the collected facts: the gate decides whether the lead goes to a meeting or
// gets a polite refusal.
func (w *Worker) completeTurn(ctx context.Context, input qualify.DecisionInput, turn agent.Turn) (store.Decision, store.Plan, error) {
	facts := input.Case.Facts
	if err := agent.EnsureNiche(ctx, w.agent, w.rules, input.Case.ID, facts, input.LeadBlock()); err != nil {
		return store.Decision{}, store.Plan{}, err
	}
	if qualify.CompleteIsIdempotent(input.Case.Status, turn.FactsChanged) {
		decision := w.replyTurn(input, turn)
		if turn.FinalText == "" {
			return decision, store.Plan{}, nil
		}
		return decision, store.Plan{Reply: &store.Reply{Purpose: "agent_text", Text: turn.FinalText}}, nil
	}
	gate := w.rules.Gate(facts)
	segment, reason := w.rules.Segment(facts)
	slog.Info("meeting gate decided", "case_id", input.Case.ID, "gate", string(gate),
		"segment", segment, "niche_off", reason, "threshold", w.rules.Threshold(segment))
	switch gate {
	case qualify.GateOffProfile:
		return w.rejectTurn(input, turn, string(gate), w.catalog.Texts.OffProfile,
			"отказ: ниша вне профиля ("+w.rules.ReasonName(reason)+")")
	case qualify.GateTooEarly:
		text := render(w.catalog.Texts.TooEarly, "{threshold}", qualify.Rubles(w.rules.Threshold(segment)))
		return w.rejectTurn(input, turn, string(gate), text, "отказ: выручка ниже порога сегмента")
	}
	return w.offerSlots(ctx, input, turn.FinalText, "", zeroTime, zeroTime)
}

func (w *Worker) rejectTurn(input qualify.DecisionInput, turn agent.Turn, reason, text, note string) (store.Decision, store.Plan, error) {
	decision := store.Decision{Source: "llm", Status: qualify.StatusRejected, NextAction: "reject",
		Reason: reason, Facts: input.Case.Facts}
	return decision, store.Plan{
		Reply:     &store.Reply{Purpose: "rejected", Text: withReply(turn.FinalText, text)},
		CRMNote:   w.leadNote(input, note),
		DropSlots: true,
	}, nil
}

// closeTurn - a refusal from the lead or a request not to write: the booking is cancelled, the queue
// is cleared before the farewell, and after that the bot does not write first.
func (w *Worker) closeTurn(input qualify.DecisionInput, turn agent.Turn, status qualify.Status, text, note string) (store.Decision, store.Plan, error) {
	decision := store.Decision{Source: "llm", Status: status, NextAction: turn.Terminal.String(),
		Reason: turn.Reason, Facts: input.Case.Facts}
	plan := store.Plan{
		Reply:         &store.Reply{Purpose: "farewell", Text: text},
		CRMNote:       w.leadNote(input, note),
		CancelOutbox:  true,
		CancelMeeting: true,
		DropSlots:     true,
	}
	if input.Meeting != nil {
		plan.CRMTask = &store.Task{ManagerID: input.Meeting.ManagerID,
			Text: "Встреча " + w.dateTime(input.Meeting.Start) + " отменена: " + note}
	}
	return decision, plan, nil
}

// manualReview - handoff to a human: a ready reply to the lead and a task for the manager,
// the case is frozen. A request to reschedule an appointed meeting is the same exit with
// its own text: the manager picks the new time.
func (w *Worker) manualReview(input qualify.DecisionInput, reason, source string) (store.Decision, store.Plan, error) {
	text := w.catalog.Texts.Handoff
	task := &store.Task{Text: "Лид передан человеку, причина: " + reason + ". " + w.factsSummary(input.Case.Facts)}
	if reason == "escalate:reschedule" && input.Meeting != nil {
		text = w.catalog.Texts.Reschedule
		task = &store.Task{ManagerID: input.Meeting.ManagerID,
			Text: "Перенести встречу " + w.dateTime(input.Meeting.Start) + ": лид просит другое время"}
	}
	decision := store.Decision{Source: source, Status: qualify.StatusManualReview, NextAction: "manual_review",
		Reason: reason, Facts: input.Case.Facts}
	return decision, store.Plan{Reply: &store.Reply{Purpose: "handoff", Text: text}, CRMTask: task}, nil
}

// failTurn handles a turn failure: cancellation goes up, a permanent failure and
// an exhausted attempt ceiling go to a human, the rest is deferred.
func (w *Worker) failTurn(ctx context.Context, claimAttempt int, input qualify.DecisionInput, turn agent.Turn, cause error) error {
	if errors.Is(cause, context.Canceled) || ctx.Err() != nil {
		return cause
	}
	failure := &agent.ExternalFailure{Kind: "turn_error", Message: cause.Error()}
	var external *agent.ExternalFailure
	if errors.As(cause, &external) {
		failure = external
	}
	if !failure.Permanent && claimAttempt <= agentMaxAttempts {
		return w.store.FailCase(ctx, input.Case.ID, claimAttempt, failure.Kind, failure.Message)
	}
	slog.Warn("turn retry ceiling reached", "case_id", input.Case.ID, "kind", failure.Kind,
		"permanent", failure.Permanent, "attempts", claimAttempt)
	// Turn facts matter more than the snapshot: the turn may have collected them and failed after save_facts.
	if len(turn.Facts) > 0 {
		input.Case.Facts = turn.Facts
	}
	decision, plan, _ := w.manualReview(input, failure.Kind, "rule")
	raw := rawResponse(turn)
	raw["error_kind"] = failure.Kind
	raw["error"] = failure.Message
	raw["attempts"] = claimAttempt
	decision.RawResponse = raw
	return w.decisionRace(input.Case.ID, w.store.ApplyDecision(ctx, input.Case.ID, input.LastIncomingMessageID, decision, plan))
}

// rawResponse - the turn audit in decisions.raw_response: tools, knowledge base,
// guard violations and critic verdicts with quotes. It does not go to the log.
func rawResponse(turn agent.Turn) map[string]any {
	raw := map[string]any{
		"terminal": turn.Terminal.String(),
		"reason":   turn.Reason,
		"kb_files": turn.Trace.KBFiles,
		"calls":    turn.Trace.Calls,
	}
	rounds := make([]map[string]any, 0, len(turn.Trace.Rounds))
	for _, round := range turn.Trace.Rounds {
		rounds = append(rounds, map[string]any{"tool": round.Tool, "arguments": round.Arguments, "result": round.Result})
	}
	raw["rounds"] = rounds
	if len(turn.Trace.Guard) > 0 {
		raw["guard"] = turn.Trace.Guard
	}
	if len(turn.Trace.Critic) > 0 {
		raw["critic"] = turn.Trace.Critic
	}
	if turn.Trace.CriticRaw != "" {
		raw["critic_raw"] = turn.Trace.CriticRaw
	}
	if len(turn.Trace.Claims) > 0 {
		raw["claims"] = turn.Trace.Claims
	}
	return raw
}
