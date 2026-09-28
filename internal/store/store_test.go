package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

var databaseSeq atomic.Int64

// openStore - интеграционный ярус: своя база на тест в Postgres из DATABASE_URL
// (Makefile, docker-compose.yml). Под -short тест пропускается.
func openStore(t *testing.T) *Store {
	t.Helper()
	if testing.Short() {
		t.Skip("integration tier: needs Postgres")
	}
	adminURL := os.Getenv("DATABASE_URL")
	if adminURL == "" {
		t.Fatal("DATABASE_URL is required for the integration tier; run with -short to skip it")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	name := fmt.Sprintf("lq_store_%d_%d", os.Getpid(), databaseSeq.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })
	cfg, err := pgx.ParseConfig(adminURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	s, err := Open(ctx, fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, name))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

var testNow = time.Date(2026, time.October, 5, 3, 0, 0, 0, time.UTC)

// newCase заводит лида и кейс в сборе; приветствие уже доставлено.
func newCase(t *testing.T, s *Store, peer string) int64 {
	t.Helper()
	ctx := context.Background()
	c, err := s.CreateCase(ctx, qualify.Lead{Name: "Олег", PeerRef: peer, CRMRef: "crm-" + peer}, "Здравствуйте!")
	if err != nil {
		t.Fatalf("create case: %v", err)
	}
	for {
		item, ok, err := s.ClaimOutbox(ctx, time.Minute)
		if err != nil {
			t.Fatalf("claim greeting: %v", err)
		}
		if !ok {
			return c.ID
		}
		if err := s.CompleteOutbox(ctx, item, fmt.Sprintf("ext-%d", item.ID)); err != nil {
			t.Fatalf("complete greeting: %v", err)
		}
	}
}

// pendingInput - реплика лида и снимок кейса для хода.
func pendingInput(t *testing.T, s *Store, peer, text string) qualify.DecisionInput {
	t.Helper()
	ctx := context.Background()
	caseID, _, err := s.Ingest(ctx, peer, text, "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	input, err := s.LoadDecisionInput(ctx, caseID, testNow)
	if err != nil {
		t.Fatalf("load decision input: %v", err)
	}
	return input
}

func decision(status qualify.Status, facts qualify.Facts) Decision {
	return Decision{Source: "rule", Status: status, NextAction: "wait", Reason: "test", Facts: facts}
}

func reply(text string) Plan {
	return Plan{Reply: &Reply{Purpose: "reply", Text: text}}
}

// moveTo переводит кейс в статус решением на новую реплику лида.
func moveTo(t *testing.T, s *Store, peer string, status qualify.Status, facts qualify.Facts) int64 {
	t.Helper()
	input := pendingInput(t, s, peer, "реплика перед решением")
	if err := s.ApplyDecision(context.Background(), input.Case.ID, input.LastIncomingMessageID, decision(status, facts), Plan{}); err != nil {
		t.Fatalf("apply %s: %v", status, err)
	}
	return input.Case.ID
}

func loadCase(t *testing.T, s *Store, caseID int64) qualify.DecisionInput {
	t.Helper()
	input, err := s.LoadDecisionInput(context.Background(), caseID, testNow)
	if err != nil {
		t.Fatalf("load case %d: %v", caseID, err)
	}
	return input
}

// drainOutbox забирает все строки доставки и отмечает их отправленными.
func drainOutbox(t *testing.T, s *Store) []OutboxItem {
	t.Helper()
	ctx := context.Background()
	var items []OutboxItem
	for {
		item, ok, err := s.ClaimOutbox(ctx, time.Minute)
		if err != nil {
			t.Fatalf("claim outbox: %v", err)
		}
		if !ok {
			return items
		}
		if err := s.CompleteOutbox(ctx, item, fmt.Sprintf("ext-%d", item.ID)); err != nil {
			t.Fatalf("complete outbox: %v", err)
		}
		items = append(items, item)
	}
}

func claimNext(t *testing.T, s *Store) (int64, bool) {
	t.Helper()
	c, ok, err := s.ClaimNextCase(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("claim next case: %v", err)
	}
	return c.ID, ok
}

// Входящее по статусу: живой кейс встаёт в очередь хода, закрытый копит
// сообщение молча, отказ возвращается в сбор с прежними фактами.
func TestIngest_ByStatus(t *testing.T) {
	t.Parallel()
	facts := qualify.Facts{qualify.FactCity: "Томск"}
	cases := []struct {
		status     qualify.Status
		wantQueued bool
		wantStatus qualify.Status
	}{
		{qualify.StatusCollecting, true, qualify.StatusCollecting},
		{qualify.StatusReadyToDecide, true, qualify.StatusReadyToDecide},
		{qualify.StatusMeetingScheduled, true, qualify.StatusMeetingScheduled},
		{qualify.StatusRejected, false, qualify.StatusRejected},
		{qualify.StatusStopped, false, qualify.StatusStopped},
		{qualify.StatusManualReview, false, qualify.StatusManualReview},
		{qualify.StatusRefused, true, qualify.StatusCollecting},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			t.Parallel()
			s := openStore(t)
			caseID := newCase(t, s, "peer")
			if tc.status != qualify.StatusCollecting {
				moveTo(t, s, "peer", tc.status, facts)
			}
			if _, ok := claimNext(t, s); ok {
				t.Fatal("case claimed before new incoming")
			}

			_, queued, err := s.Ingest(context.Background(), "peer", "есть вопрос", "ext-in-1")
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			_, claimed := claimNext(t, s)
			input := loadCase(t, s, caseID)
			if queued != tc.wantQueued || claimed != tc.wantQueued {
				t.Errorf("from %s: queued = %v, claimed = %v, want %v", tc.status, queued, claimed, tc.wantQueued)
			}
			if input.Case.Status != tc.wantStatus {
				t.Errorf("from %s: status = %s, want %s", tc.status, input.Case.Status, tc.wantStatus)
			}
			if last := input.Messages[len(input.Messages)-1]; last.Text != "есть вопрос" {
				t.Errorf("from %s: last message = %q, want incoming saved", tc.status, last.Text)
			}
			if tc.status != qualify.StatusCollecting && input.Case.Facts.Value(qualify.FactCity) != "Томск" {
				t.Errorf("from %s: facts = %v, want kept", tc.status, input.Case.Facts)
			}
		})
	}
}

func TestIngest_UnknownPeer(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	if _, _, err := s.Ingest(context.Background(), "stranger", "привет", ""); !errors.Is(err, ErrUnknownPeer) {
		t.Fatalf("Ingest from unknown peer: err = %v, want ErrUnknownPeer", err)
	}
}

// Двойную бронь держит база: второе решение на то же окно менеджера
// откатывается целиком, ответ лиду не ставится.
func TestApplyDecision_DoubleBooking(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	start := time.Date(2026, time.October, 6, 5, 0, 0, 0, time.UTC)
	for _, peer := range []string{"peer-1", "peer-2", "peer-3"} {
		newCase(t, s, peer)
	}
	book := func(peer, manager string) (int64, error) {
		input := pendingInput(t, s, peer, "завтра в 12")
		plan := reply("Записала вас")
		plan.Booking = &qualify.Meeting{CaseID: input.Case.ID, ManagerID: manager, Start: start, End: start.Add(time.Hour), Status: qualify.MeetingScheduled}
		return input.Case.ID, s.ApplyDecision(ctx, input.Case.ID, input.LastIncomingMessageID, decision(qualify.StatusMeetingScheduled, nil), plan)
	}
	first, err := book("peer-1", "pavel")
	if err != nil {
		t.Fatalf("first booking: %v", err)
	}
	second, err := book("peer-2", "pavel")
	if !errors.Is(err, ErrSlotTaken) {
		t.Fatalf("second booking of the same slot: err = %v, want ErrSlotTaken", err)
	}
	if _, err := book("peer-3", "irina"); err != nil {
		t.Fatalf("same start with another manager: %v", err)
	}

	if status := loadCase(t, s, second).Case.Status; status != qualify.StatusCollecting {
		t.Errorf("rejected booking: case status = %s, want collecting", status)
	}
	for _, item := range drainOutbox(t, s) {
		if item.CaseID == second {
			t.Errorf("rejected booking left outbox row %+v", item)
		}
	}
	meetings, err := s.BusyMeetings(ctx, start.Add(-time.Hour), start.Add(time.Hour))
	if err != nil {
		t.Fatalf("busy meetings: %v", err)
	}
	var pavel []int64
	for _, m := range meetings {
		if m.ManagerID == "pavel" {
			pavel = append(pavel, m.CaseID)
		}
	}
	if len(pavel) != 1 || pavel[0] != first {
		t.Errorf("pavel meetings by case = %v, want only case %d", pavel, first)
	}
}

// Повтор решения на тот же вход отбивается и не ставит вторую строку outbox.
func TestApplyDecision_RepeatIsRejected(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	caseID := newCase(t, s, "peer")
	input := pendingInput(t, s, "peer", "расскажите о тарифах")
	apply := func() error {
		return s.ApplyDecision(ctx, caseID, input.LastIncomingMessageID, decision(qualify.StatusCollecting, nil), reply("Ответ по тарифам"))
	}
	if err := apply(); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := apply(); !errors.Is(err, ErrAlreadyApplied) {
		t.Fatalf("repeat apply: err = %v, want ErrAlreadyApplied", err)
	}
	if items := drainOutbox(t, s); len(items) != 1 {
		t.Fatalf("outbox rows = %d, want 1", len(items))
	}
}

// Реплика, пришедшая во время хода, делает решение устаревшим.
func TestApplyDecision_StaleInput(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	caseID := newCase(t, s, "peer")
	input := pendingInput(t, s, "peer", "у нас сеть магазинов")
	if _, _, err := s.Ingest(ctx, "peer", "и ещё склад", ""); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	err := s.ApplyDecision(ctx, caseID, input.LastIncomingMessageID, decision(qualify.StatusRejected, nil), reply("Отказ"))
	if !errors.Is(err, ErrStaleDecision) {
		t.Fatalf("apply on stale input: err = %v, want ErrStaleDecision", err)
	}
	if status := loadCase(t, s, caseID).Case.Status; status != qualify.StatusCollecting {
		t.Errorf("status after stale decision = %s, want collecting", status)
	}
}

func TestApplyDecision_FrozenCase(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	caseID := newCase(t, s, "peer")
	moveTo(t, s, "peer", qualify.StatusRejected, nil)
	input := pendingInput(t, s, "peer", "а всё-таки?")
	err := s.ApplyDecision(context.Background(), caseID, input.LastIncomingMessageID, decision(qualify.StatusReadyToDecide, nil), reply("Окна"))
	if !errors.Is(err, ErrFrozenCase) {
		t.Fatalf("apply to rejected case: err = %v, want ErrFrozenCase", err)
	}
}

// Строка, брошенная с истёкшей арендой, возвращается в выборку; доставку
// фиксирует только последний захват, и сообщение попадает в историю один раз.
func TestOutbox_RedeliveryAfterLeaseExpiry(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	c, err := s.CreateCase(ctx, qualify.Lead{Name: "Олег", PeerRef: "peer", CRMRef: "crm-1"}, "Здравствуйте!")
	if err != nil {
		t.Fatalf("create case: %v", err)
	}
	first, ok, err := s.ClaimOutbox(ctx, time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("first claim: ok = %v, err = %v", ok, err)
	}
	var second OutboxItem
	deadline := time.Now().Add(5 * time.Second)
	for {
		item, ok, err := s.ClaimOutbox(ctx, time.Minute)
		if err != nil {
			t.Fatalf("second claim: %v", err)
		}
		if ok {
			second = item
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expired lease did not return the row within 5s")
		}
	}
	if second.ID != first.ID {
		t.Fatalf("second claim got row %d, want abandoned row %d", second.ID, first.ID)
	}
	if err := s.CompleteOutbox(ctx, first, "ext-1"); !errors.Is(err, ErrStaleOutbox) {
		t.Fatalf("complete with expired claim: err = %v, want ErrStaleOutbox", err)
	}
	if err := s.CompleteOutbox(ctx, second, "ext-1"); err != nil {
		t.Fatalf("complete with current claim: %v", err)
	}
	if rest := drainOutbox(t, s); len(rest) != 0 {
		t.Fatalf("outbox after delivery = %+v, want empty", rest)
	}
	var outgoing int
	for _, m := range loadCase(t, s, c.ID).Messages {
		if m.Direction == qualify.DirectionOut {
			outgoing++
		}
	}
	if outgoing != 1 {
		t.Fatalf("outgoing messages in history = %d, want 1", outgoing)
	}
}

// Вторая бронь того же кейса упирается в индекс кейса, а не окна: это не
// «окно заняли», решение откатывается, прежняя бронь остаётся.
func TestApplyDecision_SecondBookingOfCase(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	caseID := newCase(t, s, "peer")
	first := time.Date(2026, time.October, 6, 5, 0, 0, 0, time.UTC)
	book := func(start time.Time) error {
		input := pendingInput(t, s, "peer", "запишите")
		plan := reply("Записала вас")
		plan.Booking = &qualify.Meeting{CaseID: caseID, ManagerID: "pavel", Start: start, End: start.Add(time.Hour), Status: qualify.MeetingScheduled}
		return s.ApplyDecision(ctx, caseID, input.LastIncomingMessageID, decision(qualify.StatusMeetingScheduled, nil), plan)
	}
	if err := book(first); err != nil {
		t.Fatalf("first booking: %v", err)
	}
	drainOutbox(t, s)
	err := book(first.Add(2 * time.Hour))
	if err == nil || errors.Is(err, ErrSlotTaken) {
		t.Fatalf("second booking of the same case: err = %v, want a non-slot error", err)
	}
	meetings, err := s.BusyMeetings(ctx, first.Add(-time.Hour), first.Add(4*time.Hour))
	if err != nil {
		t.Fatalf("busy meetings: %v", err)
	}
	if len(meetings) != 1 || !meetings[0].Start.Equal(first) {
		t.Fatalf("meetings = %+v, want only the first booking", meetings)
	}
	if items := drainOutbox(t, s); len(items) != 0 {
		t.Fatalf("outbox after rolled back booking = %+v, want empty", items)
	}
}
