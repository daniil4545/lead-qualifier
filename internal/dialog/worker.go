// Package dialog - обработчик кейсов: захват кейса, ход агента, применение
// решения и доставка outbox. Транспорт и CRM приходят снаружи интерфейсами.
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

// Agent - ход модели. Заглушка в тестах возвращает готовый Turn.
type Agent interface {
	RunTurn(ctx context.Context, input qualify.DecisionInput) (agent.Turn, error)
	agent.NicheClassifier
}

// Sender - транспорт до лида. Возвращает внешний id сообщения.
type Sender interface {
	Send(ctx context.Context, peerRef, text string) (string, error)
}

const (
	// caseLease - аренда кейса на ход: девять вызовов модели с таймаутами
	// укладываются с запасом.
	caseLease = 10 * time.Minute
	// outboxLease - аренда строки доставки.
	outboxLease = time.Minute
	pollPeriod  = 500 * time.Millisecond
	// agentMaxAttempts - сколько раз сбойный ход откладывается и переигрывается;
	// следующий захват уводит кейс к человеку, а не держит лида без ответа.
	agentMaxAttempts = 3
	// offeredSlotsTTL - сколько живёт показанный набор окон.
	offeredSlotsTTL = 48 * time.Hour
)

// Deps - зависимости обработчика. Now nil - системные часы.
type Deps struct {
	Store    *store.Store
	Agent    Agent
	CRM      crm.CRM
	Sender   Sender
	Calendar calendar.Calendar
	Catalog  config.Catalog
	Now      func() time.Time
}

// Worker ведёт кейсы и доставку. Один экземпляр на процесс: захват кейса и
// строки outbox идёт через SKIP LOCKED, так что второй не сломал бы порядок,
// но и не нужен.
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

// New собирает обработчик.
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

// StartCase заводит кейс по заявке из CRM: бот пишет приветствие первым.
func (w *Worker) StartCase(ctx context.Context, lead qualify.Lead) (qualify.Case, error) {
	greeting := render(w.catalog.Texts.Greeting, "{name}", lead.Name)
	return w.store.CreateCase(ctx, lead, greeting)
}

// Ingest принимает реплику лида и будит обработчик, если кейс ждёт хода.
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

// Idle - по собеседнику не осталось работы: ход сделан, ответ доставлен.
func (w *Worker) Idle(ctx context.Context, peerRef string) (bool, error) {
	return w.store.PeerIdle(ctx, peerRef)
}

// Wake будит цикл кейсов, не дожидаясь тика.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run крутит два цикла: ходы по кейсам и доставку outbox. Останавливается с
// отменой контекста.
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
			// Сбой базы временный: цикл продолжает со следующего тика, иначе
			// консоль принимала бы ввод при мёртвом обработчике.
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
			// Строка остаётся арендованной и вернётся в выборку после аренды.
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

// DeliverNext доставляет одну строку outbox. Повтор идемпотентен: CRM получает
// ключ строки, транспорт - ту же строку, пока она не отмечена отправленной.
// false - доставлять нечего.
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
