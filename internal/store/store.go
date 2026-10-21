// Package store - state in Postgres: leads, cases, messages, decisions,
// bookings and outbox. The decision, the status change, the booking and outward actions are written in a single
// transaction: a failure between them neither loses nor duplicates an effect.
package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

//go:embed schema.sql
var schema string

var (
	// ErrUnknownPeer - an incoming message from a peer with no lead.
	ErrUnknownPeer = errors.New("unknown peer")
	// ErrFrozenCase - the case is closed, a decision is not applied to it.
	ErrFrozenCase = errors.New("case is frozen")
	// ErrStaleDecision - a new message arrived during the turn: the turn is replayed
	// with the full block.
	ErrStaleDecision = errors.New("decision input is stale")
	// ErrAlreadyApplied - a decision is already applied to this input.
	ErrAlreadyApplied = errors.New("decision already applied")
	// ErrSlotTaken - the manager's window was taken during the turn. The decision transaction
	// rolls back entirely: the lead will not get a confirmation of a booking that does not exist.
	ErrSlotTaken = errors.New("meeting slot is taken")
	// ErrStaleOutbox - another pass took over the outbox row.
	ErrStaleOutbox = errors.New("outbox claim is stale")
)

// Store - database access.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to the database and checks the connection.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close closes the connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// Migrate applies the schema. It is idempotent: a repeated start changes nothing.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// Decision - the decision of a turn being applied. Status is the case status after the decision.
type Decision struct {
	Source      string // llm | rule
	Status      qualify.Status
	NextAction  string
	Reason      string
	Facts       qualify.Facts
	Message     string
	RawResponse map[string]any
}

// Reply - a message to the lead in the decision plan.
type Reply struct {
	Purpose string
	Text    string
}

// Task - a task for the manager in the CRM; an empty ManagerID means the deal owner.
type Task struct {
	ManagerID string
	Text      string
}

// Plan - the decision effects written in its transaction.
type Plan struct {
	Reply   *Reply
	CRMNote string
	CRMTask *Task
	// Slots - the new set of windows shown to the lead; SlotsTTL - until when it is alive.
	Slots    []qualify.OfferedSlot
	SlotsTTL time.Time
	// DropSlots - the set is no longer valid (booking, refusal).
	DropSlots bool
	// Booking - a new booking. Uniqueness of (manager, start) is held by an index.
	Booking *qualify.Meeting
	// CancelMeeting - cancel the case's active booking.
	CancelMeeting bool
	// CancelOutbox - cancel undelivered messages to the lead before the reply of this
	// decision: after a farewell nothing that was queued goes out.
	CancelOutbox bool
}

type rowScanner interface {
	Scan(dest ...any) error
}

const caseColumns = `id, lead_id, status, facts, decision_reason, attempts, created_at, updated_at`

func scanCase(row rowScanner) (qualify.Case, error) {
	var item qualify.Case
	var status string
	var facts []byte
	if err := row.Scan(&item.ID, &item.LeadID, &status, &facts, &item.DecisionReason,
		&item.Attempts, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return qualify.Case{}, err
	}
	item.Status = qualify.Status(status)
	if err := json.Unmarshal(facts, &item.Facts); err != nil {
		return qualify.Case{}, fmt.Errorf("decode case facts: %w", err)
	}
	if item.Facts == nil {
		item.Facts = qualify.Facts{}
	}
	return item, nil
}

func addEvent(ctx context.Context, tx pgx.Tx, caseID int64, eventType string, payload map[string]any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", eventType, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO case_events (case_id, event_type, payload) VALUES ($1, $2, $3)`,
		caseID, eventType, encoded); err != nil {
		return fmt.Errorf("insert %s event: %w", eventType, err)
	}
	return nil
}

func changeStatus(ctx context.Context, tx pgx.Tx, caseID int64, from, to qualify.Status, trigger string) error {
	if from == to {
		return nil
	}
	if err := addEvent(ctx, tx, caseID, "status_changed", map[string]any{"from": from, "to": to}); err != nil {
		return err
	}
	slog.Info("case status changed", "case_id", caseID, "from", string(from), "to", string(to), "trigger", trigger)
	return nil
}

func insertOutbox(ctx context.Context, tx pgx.Tx, caseID int64, kind, purpose, causeKey, text, managerID string) error {
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO outbox (case_id, kind, purpose, cause_key, text, manager_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (case_id, kind, purpose, cause_key) DO NOTHING
		RETURNING id
	`, caseID, kind, purpose, causeKey, text, managerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Debug("outbox dedup", "case_id", caseID, "kind", kind, "purpose", purpose)
		return nil
	}
	if err != nil {
		return fmt.Errorf("insert outbox %s: %w", kind, err)
	}
	slog.Info("outbox planned", "case_id", caseID, "outbox_id", id, "kind", kind, "purpose", purpose)
	return nil
}

func rollback(ctx context.Context, tx pgx.Tx) {
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		slog.Warn("rollback failed", "error", err)
	}
}
