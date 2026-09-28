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

// ProcessCase - один ход по захваченному кейсу. claimAttempt - номер захвата
// из ClaimNextCase: по нему считается потолок попыток и ограждается откладывание.
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
	turn, err := w.agent.RunTurn(ctx, input)
	if err != nil {
		return w.failTurn(ctx, claimAttempt, input, turn, err)
	}
	// Технический исход хода - сбой модели, а не решение: ход переигрывается,
	// после потолка попыток кейс уходит к человеку обычным manual_review.
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
	// Реплика писалась под прежнее решение и могла обещать встречу, которую
	// пересчёт гейта сейчас отменит.
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
		// Окно заняли, пока шёл ход: бронь откатилась вместе с решением, лиду
		// уходят свежие окна.
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

// decisionRace гасит штатные гонки применения: новая реплика пришла во время
// хода, решение уже применено, кейс закрылся. Ход переиграется сам.
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

// outcome - обработчик исхода по терминалу хода. Каждый собирает решение и
// план целиком.
func (w *Worker) outcome(ctx context.Context, input qualify.DecisionInput, turn agent.Turn) (store.Decision, store.Plan, error) {
	switch turn.Terminal {
	case agent.TerminalComplete:
		return w.completeTurn(ctx, input, turn)
	case agent.TerminalOfferSlots, agent.TerminalBookSlot:
		// Рубеж записи перепроверяется до окон и брони: Turn недоверенный, и
		// заглушка или сбой агента не должны дать встречу мимо гейта.
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
	// Лид поправил факт после решения, а повторный complete модель не позвала:
	// гейт всё равно обязан пересмотреть решение.
	if gateNeedsReview(input, turn) {
		return w.completeTurn(ctx, input, turn)
	}
	return w.replyTurn(input, turn), store.Plan{Reply: &store.Reply{Purpose: "agent_text", Text: turn.FinalText}}, nil
}

// gateNeedsReview - ход без терминала изменил факты уже решённого кейса.
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

// completeTurn - исход собранных фактов: гейт решает, идёт лид на встречу или
// получает вежливый отказ.
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

// closeTurn - отказ лида либо просьба не писать: бронь снимается, очередь
// гасится до прощания, дальше бот первым не пишет.
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

// manualReview - передача человеку: готовый ответ лиду и задача менеджеру,
// кейс заморожен. Просьба перенести назначенную встречу - тот же выход со
// своим текстом: подбор нового времени ведёт менеджер.
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

// failTurn разбирает сбой хода: отмена уходит наверх, постоянный сбой и
// исчерпанный потолок попыток - к человеку, остальное откладывается.
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
	// Факты хода важнее снимка: ход мог собрать их и упасть после save_facts.
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

// rawResponse - аудит хода в decisions.raw_response: инструменты, база,
// нарушения guard и вердикты критика с цитатами. В лог это не идёт.
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
	return raw
}
