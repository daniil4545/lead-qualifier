package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

// CreateCase creates a lead from a CRM request, a case in collection and a greeting in the outbox.
// The bot writes first: the lead's reply continues this same case.
func (s *Store) CreateCase(ctx context.Context, lead qualify.Lead, greeting string) (qualify.Case, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return qualify.Case{}, fmt.Errorf("begin create case: %w", err)
	}
	defer rollback(ctx, tx)
	var leadID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO leads (name, peer_ref, crm_ref) VALUES ($1, $2, $3) RETURNING id
	`, lead.Name, lead.PeerRef, lead.CRMRef).Scan(&leadID); err != nil {
		return qualify.Case{}, fmt.Errorf("insert lead: %w", err)
	}
	created, err := scanCase(tx.QueryRow(ctx, `
		INSERT INTO cases (lead_id, status) VALUES ($1, $2) RETURNING `+caseColumns,
		leadID, qualify.StatusCollecting))
	if err != nil {
		return qualify.Case{}, fmt.Errorf("insert case: %w", err)
	}
	if err := addEvent(ctx, tx, created.ID, "case_created", map[string]any{"lead_id": leadID, "crm_ref": lead.CRMRef}); err != nil {
		return qualify.Case{}, err
	}
	if err := insertOutbox(ctx, tx, created.ID, "message", "greeting", "application", greeting, ""); err != nil {
		return qualify.Case{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return qualify.Case{}, fmt.Errorf("commit create case: %w", err)
	}
	slog.Info("case created", "case_id", created.ID, "lead_id", leadID, "crm_ref", lead.CRMRef)
	return created, nil
}

// Ingest saves a lead's message. A live case is queued for a turn, a refusal
// returns to collection with the previous facts, a closed case accumulates the message silently.
// Returns the case and a flag that it is waiting for a turn.
func (s *Store) Ingest(ctx context.Context, peerRef, text, externalID string) (int64, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("begin ingest: %w", err)
	}
	defer rollback(ctx, tx)
	current, err := scanCase(tx.QueryRow(ctx, `
		SELECT c.id, c.lead_id, c.status, c.facts, c.decision_reason, c.attempts, c.created_at, c.updated_at
		FROM cases c JOIN leads l ON l.id = c.lead_id
		WHERE l.peer_ref = $1
		FOR UPDATE OF c
	`, peerRef))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("%w: %s", ErrUnknownPeer, peerRef)
	}
	if err != nil {
		return 0, false, fmt.Errorf("load case of peer: %w", err)
	}
	var messageID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO messages (case_id, direction, text, external_id) VALUES ($1, 'in', $2, $3)
		ON CONFLICT (case_id, direction, external_id) WHERE external_id <> '' DO NOTHING
		RETURNING id
	`, current.ID, text, externalID).Scan(&messageID)
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Debug("inbound duplicate", "case_id", current.ID)
		return current.ID, false, tx.Commit(ctx)
	}
	if err != nil {
		return 0, false, fmt.Errorf("insert inbound message: %w", err)
	}
	if err := addEvent(ctx, tx, current.ID, "message_saved", map[string]any{"message_id": messageID}); err != nil {
		return 0, false, err
	}
	if current.Status.Frozen() {
		if err := tx.Commit(ctx); err != nil {
			return 0, false, fmt.Errorf("commit inbound to frozen case: %w", err)
		}
		slog.Info("inbound to frozen case", "case_id", current.ID, "status", string(current.Status), "msg_id", messageID)
		return current.ID, false, nil
	}
	next := current.Status
	if current.Status == qualify.StatusRefused {
		next = qualify.StatusCollecting
	}
	if _, err := tx.Exec(ctx, `
		UPDATE cases SET status = $2, next_action_at = now(), updated_at = now() WHERE id = $1
	`, current.ID, next); err != nil {
		return 0, false, fmt.Errorf("wake case: %w", err)
	}
	if err := changeStatus(ctx, tx, current.ID, current.Status, next, "inbound"); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("commit ingest: %w", err)
	}
	slog.Info("inbound accepted", "case_id", current.ID, "msg_id", messageID, "status", string(next))
	return current.ID, true, nil
}

// ClaimNextCase takes a case with an unprocessed lead message. An undelivered
// reply holds the case: a second turn on top of the first would give two replies to one
// incoming message. attempts grows with each claim and is reset by a decision.
func (s *Store) ClaimNextCase(ctx context.Context, lease time.Duration) (qualify.Case, bool, error) {
	claimed, err := scanCase(s.pool.QueryRow(ctx, `
		UPDATE cases SET next_action_at = now() + $1::interval, attempts = attempts + 1, updated_at = now()
		WHERE id = (
			SELECT c.id FROM cases c
			WHERE c.status IN ('collecting', 'ready_to_decide', 'meeting_scheduled')
			  AND c.next_action_at <= now()
			  AND EXISTS (SELECT 1 FROM messages m
			              WHERE m.case_id = c.id AND m.direction = 'in' AND m.processed_at IS NULL)
			  AND NOT EXISTS (SELECT 1 FROM outbox o
			                  WHERE o.case_id = c.id AND o.kind = 'message' AND o.status IN ('pending', 'sending'))
			ORDER BY c.next_action_at, c.id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING `+caseColumns, lease.String()))
	if errors.Is(err, pgx.ErrNoRows) {
		return qualify.Case{}, false, nil
	}
	if err != nil {
		return qualify.Case{}, false, fmt.Errorf("claim case: %w", err)
	}
	slog.Debug("case claimed", "case_id", claimed.ID, "attempts", claimed.Attempts)
	return claimed, true, nil
}

// LoadDecisionInput - the case snapshot for a turn. now cuts off a stale set of windows.
func (s *Store) LoadDecisionInput(ctx context.Context, caseID int64, now time.Time) (qualify.DecisionInput, error) {
	input := qualify.DecisionInput{Now: now}
	var err error
	input.Case, err = scanCase(s.pool.QueryRow(ctx, `SELECT `+caseColumns+` FROM cases WHERE id = $1`, caseID))
	if err != nil {
		return qualify.DecisionInput{}, fmt.Errorf("load case %d: %w", caseID, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT id, name, peer_ref, crm_ref FROM leads WHERE id = $1`,
		input.Case.LeadID).Scan(&input.Lead.ID, &input.Lead.Name, &input.Lead.PeerRef, &input.Lead.CRMRef); err != nil {
		return qualify.DecisionInput{}, fmt.Errorf("load lead of case %d: %w", caseID, err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, case_id, direction, text, created_at FROM messages WHERE case_id = $1 ORDER BY id
	`, caseID)
	if err != nil {
		return qualify.DecisionInput{}, fmt.Errorf("load messages: %w", err)
	}
	input.Messages, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (qualify.Message, error) {
		var message qualify.Message
		err := row.Scan(&message.ID, &message.CaseID, &message.Direction, &message.Text, &message.CreatedAt)
		return message, err
	})
	if err != nil {
		return qualify.DecisionInput{}, fmt.Errorf("scan messages: %w", err)
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(max(id), 0) FROM messages WHERE case_id = $1 AND direction = 'in' AND processed_at IS NULL
	`, caseID).Scan(&input.LastIncomingMessageID); err != nil {
		return qualify.DecisionInput{}, fmt.Errorf("load last incoming: %w", err)
	}
	slotRows, err := s.pool.Query(ctx, `
		SELECT manager_id, starts_at, ends_at FROM offered_slots
		WHERE case_id = $1 AND expires_at > $2 ORDER BY starts_at
	`, caseID, now)
	if err != nil {
		return qualify.DecisionInput{}, fmt.Errorf("load offered slots: %w", err)
	}
	input.OfferedSlots, err = pgx.CollectRows(slotRows, func(row pgx.CollectableRow) (qualify.OfferedSlot, error) {
		var slot qualify.OfferedSlot
		err := row.Scan(&slot.ManagerID, &slot.Start, &slot.End)
		return slot, err
	})
	if err != nil {
		return qualify.DecisionInput{}, fmt.Errorf("scan offered slots: %w", err)
	}
	meeting, err := scanMeeting(s.pool.QueryRow(ctx, `
		SELECT `+meetingColumns+` FROM meetings WHERE case_id = $1 AND status = 'scheduled'
	`, caseID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return qualify.DecisionInput{}, fmt.Errorf("load meeting: %w", err)
	default:
		input.Meeting = &meeting
	}
	return input, nil
}

const meetingColumns = `id, case_id, manager_id, starts_at, ends_at, status`

func scanMeeting(row rowScanner) (qualify.Meeting, error) {
	var meeting qualify.Meeting
	err := row.Scan(&meeting.ID, &meeting.CaseID, &meeting.ManagerID, &meeting.Start, &meeting.End, &meeting.Status)
	return meeting, err
}

// BusyMeetings - active bookings overlapping [from, to): occupancy for selection.
func (s *Store) BusyMeetings(ctx context.Context, from, to time.Time) ([]qualify.Meeting, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+meetingColumns+` FROM meetings
		WHERE status = 'scheduled' AND starts_at < $2 AND ends_at > $1
	`, from, to)
	if err != nil {
		return nil, fmt.Errorf("load busy meetings: %w", err)
	}
	meetings, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (qualify.Meeting, error) {
		return scanMeeting(row)
	})
	if err != nil {
		return nil, fmt.Errorf("scan busy meetings: %w", err)
	}
	return meetings, nil
}

// FailCase defers a turn after a model failure: the status does not change, the input
// stays unprocessed. claimAttempt guards against a taken-over claim.
func (s *Store) FailCase(ctx context.Context, caseID int64, claimAttempt int, kind, message string) error {
	retryIn := retryBackoff(claimAttempt)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin postpone case: %w", err)
	}
	defer rollback(ctx, tx)
	tag, err := tx.Exec(ctx, `
		UPDATE cases SET next_action_at = now() + $3::interval, last_error_kind = $4, last_error = $5, updated_at = now()
		WHERE id = $1 AND attempts = $2
	`, caseID, claimAttempt, retryIn.String(), kind, message)
	if err != nil {
		return fmt.Errorf("postpone case: %w", err)
	}
	if tag.RowsAffected() == 0 {
		slog.Debug("postpone race", "case_id", caseID, "claim_attempt", claimAttempt)
		return nil
	}
	if err := addEvent(ctx, tx, caseID, "turn_postponed",
		map[string]any{"kind": kind, "error": message, "attempt": claimAttempt}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit postpone case: %w", err)
	}
	slog.Warn("turn postponed", "case_id", caseID, "kind", kind, "attempt", claimAttempt, "retry_in", retryIn)
	return nil
}

// UnansweredFor - how long the oldest lead message of the case without an answer has waited, by
// the database clock. Zero if there is none.
func (s *Store) UnansweredFor(ctx context.Context, caseID int64) (time.Duration, error) {
	var seconds *float64
	err := s.pool.QueryRow(ctx, `
		SELECT EXTRACT(EPOCH FROM now() - min(created_at)) FROM messages
		WHERE case_id = $1 AND direction = 'in' AND processed_at IS NULL
	`, caseID).Scan(&seconds)
	if err != nil {
		return 0, fmt.Errorf("oldest unanswered message: %w", err)
	}
	if seconds == nil {
		return 0, nil
	}
	return time.Duration(*seconds * float64(time.Second)), nil
}

// retryBackoff - the pause before a turn retry: 1, 2, 4 minutes and on up to 16.
func retryBackoff(attempt int) time.Duration {
	backoff := time.Minute
	for index := 1; index < attempt && backoff < 16*time.Minute; index++ {
		backoff *= 2
	}
	return backoff
}

// PeerIdle - the peer's case has no work: no unprocessed message in a live
// case and no undelivered outbox row.
func (s *Store) PeerIdle(ctx context.Context, peerRef string) (bool, error) {
	var busy bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM cases c JOIN leads l ON l.id = c.lead_id
			WHERE l.peer_ref = $1 AND (
				EXISTS (SELECT 1 FROM outbox o WHERE o.case_id = c.id AND o.status IN ('pending', 'sending'))
				OR (c.status IN ('collecting', 'ready_to_decide', 'meeting_scheduled') AND EXISTS (
					SELECT 1 FROM messages m WHERE m.case_id = c.id AND m.direction = 'in' AND m.processed_at IS NULL))
			)
		)
	`, peerRef).Scan(&busy); err != nil {
		return false, fmt.Errorf("check peer idle: %w", err)
	}
	return !busy, nil
}
