// Package store - состояние в Postgres: лиды, кейсы, сообщения, решения,
// брони и outbox. Решение, смена статуса, бронь и действия наружу пишутся одной
// транзакцией: сбой между ними не теряет и не дублирует эффект.
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
	// ErrUnknownPeer - входящее от собеседника без заявки.
	ErrUnknownPeer = errors.New("unknown peer")
	// ErrFrozenCase - кейс закрыт, решение к нему не применяется.
	ErrFrozenCase = errors.New("case is frozen")
	// ErrStaleDecision - пока шёл ход, пришла новая реплика: ход переигрывается
	// уже с полным блоком.
	ErrStaleDecision = errors.New("decision input is stale")
	// ErrAlreadyApplied - на этот вход решение уже применено.
	ErrAlreadyApplied = errors.New("decision already applied")
	// ErrSlotTaken - окно менеджера заняли, пока шёл ход. Транзакция решения
	// откатывается целиком: лид не получит подтверждение брони, которой нет.
	ErrSlotTaken = errors.New("meeting slot is taken")
	// ErrStaleOutbox - строку outbox перехватил другой проход.
	ErrStaleOutbox = errors.New("outbox claim is stale")
)

// Store - доступ к базе.
type Store struct {
	pool *pgxpool.Pool
}

// Open подключается к базе и проверяет соединение.
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

// Close закрывает пул соединений.
func (s *Store) Close() {
	s.pool.Close()
}

// Migrate применяет схему. Она идемпотентна: повторный старт ничего не меняет.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// Decision - применяемое решение хода. Status - статус кейса после решения.
type Decision struct {
	Source      string // llm | rule
	Status      qualify.Status
	NextAction  string
	Reason      string
	Facts       qualify.Facts
	Message     string
	RawResponse map[string]any
}

// Reply - сообщение лиду в плане решения.
type Reply struct {
	Purpose string
	Text    string
}

// Task - задача менеджеру в CRM; пустой ManagerID - ответственному сделки.
type Task struct {
	ManagerID string
	Text      string
}

// Plan - эффекты решения, которые пишутся в его транзакции.
type Plan struct {
	Reply   *Reply
	CRMNote string
	CRMTask *Task
	// Slots - новый набор окон, показанных лиду; SlotsTTL - до когда он живой.
	Slots    []qualify.OfferedSlot
	SlotsTTL time.Time
	// DropSlots - набор больше не действует (бронь, отказ).
	DropSlots bool
	// Booking - новая бронь. Уникальность (менеджер, начало) держит индекс.
	Booking *qualify.Meeting
	// CancelMeeting - снять активную бронь кейса.
	CancelMeeting bool
	// CancelOutbox - погасить недоставленные сообщения лиду до ответа этого
	// решения: после прощания не уходит ничего, что стояло в очереди.
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
