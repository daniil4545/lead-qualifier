package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// OutboxItem - a delivery row with the lead's addresses: where to write and which deal
// to touch in the CRM.
type OutboxItem struct {
	ID        int64
	CaseID    int64
	Kind      string // message | crm_note | crm_task
	Purpose   string
	Text      string
	ManagerID string
	PeerRef   string
	CRMRef    string
	Attempts  int
}

// outboxMaxFailures - after this many failed sends the row is considered
// undelivered and stays in the audit with the status failed.
const outboxMaxFailures = 3

// ClaimOutbox leases the next delivery row. The lease expires on its own: a row
// abandoned by a crashed process returns to the selection.
func (s *Store) ClaimOutbox(ctx context.Context, lease time.Duration) (OutboxItem, bool, error) {
	var item OutboxItem
	err := s.pool.QueryRow(ctx, `
		WITH next AS (
			SELECT o.id FROM outbox o
			WHERE o.status IN ('pending', 'sending') AND o.next_attempt_at <= now()
			ORDER BY o.next_attempt_at, o.id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox o SET status = 'sending', attempts = o.attempts + 1, next_attempt_at = now() + $1::interval
		FROM next, cases c, leads l
		WHERE o.id = next.id AND c.id = o.case_id AND l.id = c.lead_id
		RETURNING o.id, o.case_id, o.kind, o.purpose, o.text, o.manager_id, l.peer_ref, l.crm_ref, o.attempts
	`, lease.String()).Scan(&item.ID, &item.CaseID, &item.Kind, &item.Purpose, &item.Text, &item.ManagerID,
		&item.PeerRef, &item.CRMRef, &item.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return OutboxItem{}, false, nil
	}
	if err != nil {
		return OutboxItem{}, false, fmt.Errorf("claim outbox: %w", err)
	}
	return item, true, nil
}

// CompleteOutbox records delivery. The message to the lead enters the case history
// in the same transaction: the model sees only what the lead actually received.
func (s *Store) CompleteOutbox(ctx context.Context, item OutboxItem, externalID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin complete outbox: %w", err)
	}
	defer rollback(ctx, tx)
	if err := lockClaim(ctx, tx, item); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE outbox SET status = 'sent', external_id = $2, sent_at = now(), last_error = '' WHERE id = $1
	`, item.ID, externalID); err != nil {
		return fmt.Errorf("mark outbox sent: %w", err)
	}
	if item.Kind == "message" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO messages (case_id, direction, text, external_id) VALUES ($1, 'out', $2, $3)
		`, item.CaseID, item.Text, externalID); err != nil {
			return fmt.Errorf("save sent message: %w", err)
		}
	}
	if err := addEvent(ctx, tx, item.CaseID, "outbox_sent", map[string]any{"outbox_id": item.ID,
		"kind": item.Kind, "purpose": item.Purpose}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit complete outbox: %w", err)
	}
	slog.Info("outbox sent", "case_id", item.CaseID, "outbox_id", item.ID, "kind", item.Kind, "purpose", item.Purpose)
	return nil
}

// RetryOutbox defers a row after a delivery failure; after outboxMaxFailures
// the row goes to failed.
func (s *Store) RetryOutbox(ctx context.Context, item OutboxItem, cause error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin retry outbox: %w", err)
	}
	defer rollback(ctx, tx)
	if err := lockClaim(ctx, tx, item); err != nil {
		return err
	}
	var failures int
	if err := tx.QueryRow(ctx, `
		UPDATE outbox SET failures = failures + 1, last_error = $2,
		       status = CASE WHEN failures + 1 >= $3 THEN 'failed' ELSE 'pending' END,
		       next_attempt_at = now() + interval '1 minute'
		WHERE id = $1 RETURNING failures
	`, item.ID, cause.Error(), outboxMaxFailures).Scan(&failures); err != nil {
		return fmt.Errorf("reschedule outbox: %w", err)
	}
	eventType := "outbox_retry"
	if failures >= outboxMaxFailures {
		eventType = "outbox_failed"
	}
	if err := addEvent(ctx, tx, item.CaseID, eventType, map[string]any{"outbox_id": item.ID, "failures": failures}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit retry outbox: %w", err)
	}
	slog.Warn(eventType, "case_id", item.CaseID, "outbox_id", item.ID, "kind", item.Kind, "failures", failures,
		"error", cause.Error())
	return nil
}

// lockClaim checks that the row still belongs to this lease.
func lockClaim(ctx context.Context, tx pgx.Tx, item OutboxItem) error {
	var status string
	var attempts int
	if err := tx.QueryRow(ctx, `SELECT status, attempts FROM outbox WHERE id = $1 FOR UPDATE`,
		item.ID).Scan(&status, &attempts); err != nil {
		return fmt.Errorf("lock outbox %d: %w", item.ID, err)
	}
	if status != "sending" || attempts != item.Attempts {
		return fmt.Errorf("%w: outbox %d status %s attempts %d", ErrStaleOutbox, item.ID, status, attempts)
	}
	return nil
}
