// Package dialog - the case handler: case claim, agent turn, applying
// the decision and outbox delivery. Transport and CRM come in from outside as interfaces.
package dialog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/daniil4545/lead-qualifier/internal/agent"
	"github.com/daniil4545/lead-qualifier/internal/calendar"
	"github.com/daniil4545/lead-qualifier/internal/config"
	"github.com/daniil4545/lead-qualifier/internal/crm"
	"github.com/daniil4545/lead-qualifier/internal/qualify"
	"github.com/daniil4545/lead-qualifier/internal/store"
)

// Agent - a model turn. The stub in tests returns a ready Turn.
type Agent interface {
	RunTurn(ctx context.Context, input qualify.DecisionInput) (agent.Turn, error)
	agent.NicheClassifier
}

// Sender - transport to the lead. Returns the external message id.
type Sender interface {
	Send(ctx context.Context, peerRef, text string) (string, error)
}

const (
	// caseLease - the lease of a case for a turn: nine model calls with timeouts
	// fit with room to spare.
	caseLease = 10 * time.Minute
	// outboxLease - the lease of a delivery row.
	outboxLease = time.Minute
	pollPeriod  = 500 * time.Millisecond
	// agentMaxAttempts - how many times a failed turn is deferred and replayed;
	// the next claim sends the case to a human instead of leaving the lead without an answer.
	agentMaxAttempts = 3
	// offeredSlotsTTL - how long a shown set of windows lives.
	offeredSlotsTTL = 48 * time.Hour
)

// Deps - the handler dependencies. A nil Now means the system clock.
type Deps struct {
	Store    *store.Store
	Agent    Agent
	CRM      crm.CRM
	Sender   Sender
	Calendar calendar.Calendar
	Catalog  config.Catalog
	Now      func() time.Time
}

// Worker drives cases and delivery. One instance per process: case claim and
// outbox rows go through SKIP LOCKED, so a second one would not break the order,
// but is not needed either.
type Worker struct {
	store    *store.Store
	agent    Agent
	crm      crm.CRM
	sender   Sender
	calendar calendar.Calendar
	catalog  config.Catalog
	rules    qualify.Rules
	now      func() time.Time
	wake     chan struct{}
}

// New builds the handler.
func New(deps Deps) *Worker {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Worker{
		store: deps.Store, agent: deps.Agent, crm: deps.CRM, sender: deps.Sender,
		calendar: deps.Calendar, catalog: deps.Catalog, rules: qualify.NewRules(deps.Catalog),
		now: now, wake: make(chan struct{}, 1),
	}
}

// StartCase creates a case from a CRM lead: the bot writes the greeting first.
func (w *Worker) StartCase(ctx context.Context, lead qualify.Lead) (qualify.Case, error) {
	greeting := render(w.catalog.Texts.Greeting, "{name}", lead.Name)
	return w.store.CreateCase(ctx, lead, greeting)
}

// Ingest accepts a lead's message and wakes the handler if the case is waiting for a turn.
func (w *Worker) Ingest(ctx context.Context, peerRef, text, externalID string) error {
	_, woke, err := w.store.Ingest(ctx, peerRef, text, externalID)
	if err != nil {
		return err
	}
	if woke {
		w.Wake()
	}
	return nil
}

// Idle - no work is left for the peer: the turn is done, the reply is delivered.
func (w *Worker) Idle(ctx context.Context, peerRef string) (bool, error) {
	return w.store.PeerIdle(ctx, peerRef)
}

// Wake wakes the case loop without waiting for the tick.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run runs two loops: turns on cases and outbox delivery. Stops with
// context cancellation.
func (w *Worker) Run(ctx context.Context) error {
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error { return w.runCases(groupCtx) })
	group.Go(func() error { return w.runOutbox(groupCtx) })
	return group.Wait()
}

func (w *Worker) runCases(ctx context.Context) error {
	ticker := time.NewTicker(pollPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.wake:
		case <-ticker.C:
		}
		claimed, ok, err := w.store.ClaimNextCase(ctx, caseLease)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// A database failure is temporary: the loop continues from the next tick, otherwise
			// the console would accept input with a dead handler.
			slog.Error("claim case failed", "error", err)
			continue
		}
		if !ok {
			continue
		}
		if err := w.ProcessCase(ctx, claimed.ID, claimed.Attempts); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Error("case processing failed", "case_id", claimed.ID, "error", err)
			continue
		}
		w.Wake()
	}
}

func (w *Worker) runOutbox(ctx context.Context) error {
	ticker := time.NewTicker(pollPeriod)
	defer ticker.Stop()
	for {
		delivered, err := w.DeliverNext(ctx)
		if err != nil && ctx.Err() != nil {
			return nil
		}
		if err != nil {
			// The row stays leased and returns to the selection after the lease.
			slog.Error("outbox delivery failed", "error", err)
		}
		if delivered && err == nil {
			w.Wake()
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// DeliverNext delivers one outbox row. A retry is idempotent: the CRM gets
// the row key, the transport gets the same row until it is marked as sent.
// false means there is nothing to deliver.
func (w *Worker) DeliverNext(ctx context.Context) (bool, error) {
	item, ok, err := w.store.ClaimOutbox(ctx, outboxLease)
	if err != nil || !ok {
		return false, err
	}
	key := fmt.Sprintf("outbox:%d", item.ID)
	externalID := ""
	var sendErr error
	switch item.Kind {
	case "message":
		externalID, sendErr = w.sender.Send(ctx, item.PeerRef, item.Text)
	case "crm_note":
		sendErr = w.crm.AddNote(ctx, key, item.CRMRef, item.Text)
	case "crm_task":
		sendErr = w.crm.AddTask(ctx, key, item.CRMRef, item.ManagerID, item.Text)
	default:
		sendErr = fmt.Errorf("unknown outbox kind %q", item.Kind)
	}
	if sendErr != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		err = w.store.RetryOutbox(ctx, item, sendErr)
	} else {
		err = w.store.CompleteOutbox(ctx, item, externalID)
	}
	if errors.Is(err, store.ErrStaleOutbox) {
		slog.Debug("outbox race", "outbox_id", item.ID)
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("finish outbox %d: %w", item.ID, err)
	}
	return true, nil
}
