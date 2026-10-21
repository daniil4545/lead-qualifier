package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

// uniqueViolation - the Postgres code for a unique index violation; slotIndex is
// the manager window index. The second booking index (one per case) is not a taken
// window: that is a code error, not a race between leads.
const (
	uniqueViolation = "23505"
	slotIndex       = "meetings_slot_idx"
)

// ApplyDecision applies a turn decision in one transaction: the block's messages
// are marked processed, the decision and case projection are written, the booking and
// outward actions are put into the outbox. lastIncomingID is the last message
// of the block the turn answered.
func (s *Store) ApplyDecision(ctx context.Context, caseID, lastIncomingID int64, decision Decision, plan Plan) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin apply decision: %w", err)
	}
	defer rollback(ctx, tx)
	var oldStatus qualify.Status
	if err := tx.QueryRow(ctx, `SELECT status FROM cases WHERE id = $1 FOR UPDATE`, caseID).Scan(&oldStatus); err != nil {
		return fmt.Errorf("lock case %d: %w", caseID, err)
	}
	if oldStatus.Frozen() {
		return fmt.Errorf("%w: case %d is %s", ErrFrozenCase, caseID, oldStatus)
	}
	if err := markBlockProcessed(ctx, tx, caseID, lastIncomingID); err != nil {
		return err
	}
	decisionID, err := insertDecision(ctx, tx, caseID, decision)
	if err != nil {
		return err
	}
	facts, err := json.Marshal(decision.Facts)
	if err != nil {
		return fmt.Errorf("encode case facts: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE cases SET status = $2, facts = $3, decision_reason = $4, attempts = 0,
		       last_error_kind = '', last_error = '', updated_at = now()
		WHERE id = $1
	`, caseID, decision.Status, facts, decision.Reason); err != nil {
		return fmt.Errorf("update case after decision: %w", err)
	}
	if err := addEvent(ctx, tx, caseID, "decision_done", map[string]any{"decision_id": decisionID,
		"next_action": decision.NextAction, "reason": decision.Reason}); err != nil {
		return err
	}
	if err := changeStatus(ctx, tx, caseID, oldStatus, decision.Status, "decision"); err != nil {
		return err
	}
	if err := applyPlan(ctx, tx, caseID, fmt.Sprintf("message:%d", lastIncomingID), plan); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit apply decision: %w", err)
	}
	slog.Info("decision applied", "case_id", caseID, "decision_id", decisionID, "source", decision.Source,
		"action", decision.NextAction, "reason", decision.Reason, "status", string(decision.Status))
	return nil
}

// markBlockProcessed marks the block of messages up to lastIncomingID. A message newer than the
// block means the turn did not answer everything: the decision is rejected.
func markBlockProcessed(ctx context.Context, tx pgx.Tx, caseID, lastIncomingID int64) error {
	var newer int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(min(id), 0) FROM messages
		WHERE case_id = $1 AND direction = 'in' AND processed_at IS NULL AND id > $2
	`, caseID, lastIncomingID).Scan(&newer); err != nil {
		return fmt.Errorf("check newer incoming: %w", err)
	}
	if newer != 0 {
		return fmt.Errorf("%w: case %d answered %d, newer %d", ErrStaleDecision, caseID, lastIncomingID, newer)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE messages SET processed_at = now()
		WHERE case_id = $1 AND direction = 'in' AND processed_at IS NULL AND id <= $2
	`, caseID, lastIncomingID)
	if err != nil {
		return fmt.Errorf("mark block processed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: case %d message %d", ErrAlreadyApplied, caseID, lastIncomingID)
	}
	return nil
}

func insertDecision(ctx context.Context, tx pgx.Tx, caseID int64, decision Decision) (int64, error) {
	facts, err := json.Marshal(decision.Facts)
	if err != nil {
		return 0, fmt.Errorf("encode decision facts: %w", err)
	}
	raw := decision.RawResponse
	if raw == nil {
		raw = map[string]any{}
	}
	rawJSON, err := json.Marshal(raw)
	if err != nil {
		return 0, fmt.Errorf("encode decision raw response: %w", err)
	}
	var id int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO decisions (case_id, source, status, next_action, reason, facts, message, raw_response)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id
	`, caseID, decision.Source, decision.Status, decision.NextAction, decision.Reason, facts,
		decision.Message, rawJSON).Scan(&id); err != nil {
		return 0, fmt.Errorf("insert decision: %w", err)
	}
	return id, nil
}

// applyPlan writes the decision effects. Order matters: first the queue is cleared and
// the booking cancelled, then the new one is put in - the farewell goes out, what stood before it does not.
func applyPlan(ctx context.Context, tx pgx.Tx, caseID int64, causeKey string, plan Plan) error {
	if plan.CancelOutbox {
		tag, err := tx.Exec(ctx, `
			UPDATE outbox SET status = 'cancelled' WHERE case_id = $1 AND kind = 'message' AND status IN ('pending', 'sending')
		`, caseID)
		if err != nil {
			return fmt.Errorf("cancel case outbox: %w", err)
		}
		slog.Info("case outbox cancelled", "case_id", caseID, "cancelled", tag.RowsAffected())
	}
	if plan.CancelMeeting {
		if err := cancelMeeting(ctx, tx, caseID); err != nil {
			return err
		}
	}
	if plan.DropSlots || len(plan.Slots) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM offered_slots WHERE case_id = $1`, caseID); err != nil {
			return fmt.Errorf("drop offered slots: %w", err)
		}
	}
	for _, slot := range plan.Slots {
		if _, err := tx.Exec(ctx, `
			INSERT INTO offered_slots (case_id, manager_id, starts_at, ends_at, expires_at) VALUES ($1, $2, $3, $4, $5)
		`, caseID, slot.ManagerID, slot.Start, slot.End, plan.SlotsTTL); err != nil {
			return fmt.Errorf("save offered slot: %w", err)
		}
	}
	if plan.Booking != nil {
		if err := insertBooking(ctx, tx, caseID, *plan.Booking); err != nil {
			return err
		}
	}
	if plan.Reply != nil {
		if err := insertOutbox(ctx, tx, caseID, "message", plan.Reply.Purpose, causeKey, plan.Reply.Text, ""); err != nil {
			return err
		}
	}
	if plan.CRMNote != "" {
		if err := insertOutbox(ctx, tx, caseID, "crm_note", "note", causeKey, plan.CRMNote, ""); err != nil {
			return err
		}
	}
	if plan.CRMTask != nil {
		if err := insertOutbox(ctx, tx, caseID, "crm_task", "task", causeKey, plan.CRMTask.Text, plan.CRMTask.ManagerID); err != nil {
			return err
		}
	}
	return nil
}

// insertBooking writes the booking. A busy manager window is rejected by the unique index,
// and the whole decision transaction rolls back.
func insertBooking(ctx context.Context, tx pgx.Tx, caseID int64, booking qualify.Meeting) error {
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO meetings (case_id, manager_id, starts_at, ends_at, status) VALUES ($1, $2, $3, $4, 'scheduled')
		RETURNING id
	`, caseID, booking.ManagerID, booking.Start, booking.End).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == slotIndex {
		return fmt.Errorf("%w: manager %s at %s", ErrSlotTaken, booking.ManagerID, booking.Start)
	}
	if err != nil {
		return fmt.Errorf("insert meeting: %w", err)
	}
	if err := addEvent(ctx, tx, caseID, "meeting_booked", map[string]any{"meeting_id": id,
		"manager_id": booking.ManagerID, "starts_at": booking.Start}); err != nil {
		return err
	}
	slog.Info("meeting booked", "case_id", caseID, "meeting_id", id, "manager_id", booking.ManagerID)
	return nil
}

func cancelMeeting(ctx context.Context, tx pgx.Tx, caseID int64) error {
	var id int64
	err := tx.QueryRow(ctx, `
		UPDATE meetings SET status = 'cancelled', updated_at = now()
		WHERE case_id = $1 AND status = 'scheduled' RETURNING id
	`, caseID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cancel meeting: %w", err)
	}
	if err := addEvent(ctx, tx, caseID, "meeting_cancelled", map[string]any{"meeting_id": id}); err != nil {
		return err
	}
	slog.Info("meeting cancelled", "case_id", caseID, "meeting_id", id)
	return nil
}
