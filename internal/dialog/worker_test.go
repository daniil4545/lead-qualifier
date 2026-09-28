package dialog

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/daniil4545/lead-qualifier/internal/agent"
	"github.com/daniil4545/lead-qualifier/internal/calendar"
	"github.com/daniil4545/lead-qualifier/internal/config"
	"github.com/daniil4545/lead-qualifier/internal/crm"
	"github.com/daniil4545/lead-qualifier/internal/qualify"
	"github.com/daniil4545/lead-qualifier/internal/store"
)

var databaseSeq atomic.Int64

// openDatabase - интеграционный ярус: своя база на тест в Postgres из
// DATABASE_URL (Makefile, docker-compose.yml). Под -short тест пропускается.
func openDatabase(t *testing.T) (*store.Store, *pgx.Conn) {
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
	name := fmt.Sprintf("lq_dialog_%d_%d", os.Getpid(), databaseSeq.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })
	cfg := admin.Config()
	url := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
	s, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return s, conn
}

type result struct {
	turn agent.Turn
	err  error
}

// stubAgent отдаёт заранее заданные ходы своей очередью на каждый кейс
// и считает вызовы хода по кейсу.
type stubAgent struct {
	mu      sync.Mutex
	results map[int64][]result
	calls   map[int64]int
}

func (a *stubAgent) queue(caseID int64, res result) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.results[caseID] = append(a.results[caseID], res)
}

func (a *stubAgent) RunTurn(_ context.Context, input qualify.DecisionInput) (agent.Turn, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls[input.Case.ID]++
	queue := a.results[input.Case.ID]
	if len(queue) == 0 {
		return agent.Turn{}, fmt.Errorf("stub agent: unexpected turn for case %d", input.Case.ID)
	}
	a.results[input.Case.ID] = queue[1:]
	return queue[0].turn, queue[0].err
}

func (a *stubAgent) ClassifyNiche(context.Context, agent.NicheInput) (qualify.NicheVerdict, error) {
	return qualify.NicheVerdict{Direction: "retail"}, nil
}

// hookCalendar вызывает beforeAvailable перед проверкой часа: так тест
// вклинивается между подбором окна и транзакцией брони.
type hookCalendar struct {
	calendar.Calendar
	beforeAvailable func()
}

func (c *hookCalendar) Available(busy calendar.Busy, now, start time.Time) (calendar.Slot, bool) {
	if hook := c.beforeAvailable; hook != nil {
		c.beforeAvailable = nil
		hook()
	}
	return c.Calendar.Available(busy, now, start)
}

type sentMessage struct{ peer, text string }

type recordingSender struct {
	mu   sync.Mutex
	sent []sentMessage
}

func (s *recordingSender) Send(_ context.Context, peerRef, text string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, sentMessage{peerRef, text})
	return fmt.Sprintf("out-%d", len(s.sent)), nil
}

func (s *recordingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func (s *recordingSender) since(from int) []sentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sent[from:])
}

// now - понедельник 5 октября 2026, 10:00 по Новосибирску (пояс data/example).
var now = time.Date(2026, time.October, 5, 3, 0, 0, 0, time.UTC)

// Вторник 6 октября по Новосибирску: 12:00 работают оба менеджера каталога,
// 16:00 - только pavel.
var (
	tuesdayNoon = time.Date(2026, time.October, 6, 5, 0, 0, 0, time.UTC)
	tuesday16   = time.Date(2026, time.October, 6, 9, 0, 0, 0, time.UTC)
)

type harness struct {
	t        *testing.T
	store    *store.Store
	db       *pgx.Conn
	worker   *Worker
	agent    *stubAgent
	calendar *hookCalendar
	crm      *crm.Fake
	sender   *recordingSender
	catalog  config.Catalog
	rules    qualify.Rules
	cases    map[string]int64 // кейс по адресу лида
}

const exampleCatalog = "../../data/example"

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessFor(t, exampleCatalog)
}

// newHarnessFor собирает обработчик над каталогом из папки dir.
func newHarnessFor(t *testing.T, dir string) *harness {
	t.Helper()
	s, db := openDatabase(t)
	catalog, err := config.LoadCatalog(dir)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	local, err := calendar.NewLocal(catalog)
	if err != nil {
		t.Fatalf("calendar: %v", err)
	}
	h := &harness{t: t, store: s, db: db,
		agent:    &stubAgent{results: map[int64][]result{}, calls: map[int64]int{}},
		calendar: &hookCalendar{Calendar: local}, crm: crm.NewFake(nil), sender: &recordingSender{},
		catalog: catalog, rules: qualify.NewRules(catalog), cases: map[string]int64{}}
	h.worker = New(Deps{Store: s, Agent: h.agent, CRM: h.crm, Sender: h.sender, Calendar: h.calendar,
		Catalog: catalog, Now: func() time.Time { return now }})
	return h
}

// start заводит заявку в CRM и кейс; приветствие доставлено.
func (h *harness) start(peer string) int64 {
	h.t.Helper()
	ctx := context.Background()
	ref, err := h.crm.CreateLead(ctx, "Олег")
	if err != nil {
		h.t.Fatalf("create lead: %v", err)
	}
	c, err := h.worker.StartCase(ctx, qualify.Lead{Name: "Олег", PeerRef: peer, CRMRef: ref})
	if err != nil {
		h.t.Fatalf("start case: %v", err)
	}
	h.cases[peer] = c.ID
	h.deliver()
	return c.ID
}

func (h *harness) deliver() {
	h.t.Helper()
	for range 100 {
		ok, err := h.worker.DeliverNext(context.Background())
		if err != nil {
			h.t.Fatalf("deliver: %v", err)
		}
		if !ok {
			return
		}
	}
	h.t.Fatal("outbox did not drain in 100 deliveries")
}

// say - реплика лида, ход его кейса с заданным результатом агента и доставка.
func (h *harness) say(peer, text string, res result) {
	h.t.Helper()
	caseID := h.cases[peer]
	h.agent.queue(caseID, res)
	if err := h.worker.Ingest(context.Background(), peer, text, ""); err != nil {
		h.t.Fatalf("ingest: %v", err)
	}
	h.turnOf(caseID)
}

// turnOf делает ход и требует, чтобы в ход взят был именно этот кейс.
func (h *harness) turnOf(caseID int64) {
	h.t.Helper()
	claimed, ok := h.turn()
	if !ok || claimed != caseID {
		h.t.Fatalf("claimed case %d (ok %v), want case %d", claimed, ok, caseID)
	}
}

// sayToFrozen - реплика лида в закрытый кейс: хода быть не должно.
func (h *harness) sayToFrozen(peer, text string) {
	h.t.Helper()
	if err := h.worker.Ingest(context.Background(), peer, text, ""); err != nil {
		h.t.Fatalf("ingest: %v", err)
	}
	if claimed, ok := h.turn(); ok {
		h.t.Errorf("frozen case %d claimed for a turn after %q", claimed, text)
	}
}

// turn делает ход по кейсу в очереди, если он есть, и доставляет outbox.
func (h *harness) turn() (int64, bool) {
	h.t.Helper()
	ctx := context.Background()
	c, ok, err := h.store.ClaimNextCase(ctx, time.Minute)
	if err != nil {
		h.t.Fatalf("claim case: %v", err)
	}
	if ok {
		if err := h.worker.ProcessCase(ctx, c.ID, c.Attempts); err != nil {
			h.t.Fatalf("process case: %v", err)
		}
	}
	h.deliver()
	return c.ID, ok
}

// elapseBackoff - время отложенного хода прошло: кейс снова можно взять в ход.
func (h *harness) elapseBackoff() {
	h.t.Helper()
	if _, err := h.db.Exec(context.Background(), "UPDATE cases SET next_action_at = now()"); err != nil {
		h.t.Fatalf("elapse backoff: %v", err)
	}
}

func (h *harness) load(caseID int64) qualify.Case {
	h.t.Helper()
	input, err := h.store.LoadDecisionInput(context.Background(), caseID, now)
	if err != nil {
		h.t.Fatalf("load case: %v", err)
	}
	return input.Case
}

func (h *harness) status(caseID int64) qualify.Status {
	h.t.Helper()
	return h.load(caseID).Status
}

func (h *harness) meetings() []qualify.Meeting {
	h.t.Helper()
	meetings, err := h.store.BusyMeetings(context.Background(), now, now.Add(calendar.Horizon))
	if err != nil {
		h.t.Fatalf("busy meetings: %v", err)
	}
	return meetings
}

// meetingOf - активная бронь кейса; нулевой Start - брони нет.
func (h *harness) meetingOf(caseID int64) qualify.Meeting {
	h.t.Helper()
	for _, m := range h.meetings() {
		if m.CaseID == caseID {
			return m
		}
	}
	return qualify.Meeting{}
}

func (h *harness) facts(revenue string, verdict qualify.NicheVerdict) qualify.Facts {
	s := func(v string) *string { return &v }
	owner := true
	facts := qualify.Merge(qualify.Facts{}, qualify.FactDelta{Service: s("сеть магазинов"), Revenue: s(revenue),
		Role: s("директор"), IsOwner: &owner, City: s("Томск")})
	h.rules.ApplyVerdict(facts, verdict)
	return facts
}

var placeholder = regexp.MustCompile(`\{[a-z]+\}`)

// fragment - самый длинный постоянный кусок текста каталога между подстановками.
func fragment(template string) string {
	longest := ""
	for _, part := range placeholder.Split(template, -1) {
		if part = strings.TrimSpace(part); len(part) > len(longest) {
			longest = part
		}
	}
	return longest
}

var retail = qualify.NicheVerdict{Direction: "retail"}

func complete(facts qualify.Facts) result {
	return result{turn: agent.Turn{Terminal: agent.TerminalComplete, Facts: facts, FactsChanged: true}}
}

func bookAt(facts qualify.Facts, at time.Time) result {
	return result{turn: agent.Turn{Terminal: agent.TerminalBookSlot, Facts: facts, RequestedAt: at}}
}

func terminal(kind agent.Terminal, reason string) result {
	return result{turn: agent.Turn{Terminal: kind, Reason: reason}}
}

// transition - клетка таблицы «состояние против хода».
type transition struct {
	name       string
	from       qualify.Status
	act        func(h *harness) result
	wantStatus qualify.Status
	// wantText - текст каталога, который ушёл лиду; nil - не ушло ничего.
	wantText func(texts config.Texts) string
	// wantReason - начало причины решения; пусто - не проверяется.
	wantReason string
	// wantCRM - виды новых записей CRM; crmExact - ровно они, иначе среди прочих.
	wantCRM     []string
	crmExact    bool
	wantMeeting bool
}

// Таблица «состояние против хода» среза 1: из какого статуса, каким ходом,
// в какой статус, что ушло лиду и что появилось в CRM.
func TestProcessCase_Transitions(t *testing.T) {
	t.Parallel()
	failure := &agent.ExternalFailure{Kind: "http", Message: "upstream 503", Status: 503}
	full := func(h *harness) qualify.Facts { return h.facts("3000000", retail) }
	low := func(h *harness) qualify.Facts { return h.facts("500000", retail) }
	cases := []transition{
		{name: "collecting complete offer", from: qualify.StatusCollecting,
			act:        func(h *harness) result { return complete(full(h)) },
			wantStatus: qualify.StatusReadyToDecide, wantText: func(x config.Texts) string { return x.SlotsFooter }},
		{name: "collecting complete too early", from: qualify.StatusCollecting,
			act:        func(h *harness) result { return complete(low(h)) },
			wantStatus: qualify.StatusRejected, wantText: func(x config.Texts) string { return x.TooEarly },
			wantCRM: []string{"note"}, crmExact: true},
		{name: "collecting complete off profile", from: qualify.StatusCollecting,
			act: func(h *harness) result {
				return complete(h.facts("30000000", qualify.NicheVerdict{OffProfileReason: "gambling"}))
			},
			wantStatus: qualify.StatusRejected, wantText: func(x config.Texts) string { return x.OffProfile },
			wantCRM: []string{"note"}, crmExact: true},
		{name: "collecting refuse", from: qualify.StatusCollecting,
			act:        func(*harness) result { return terminal(agent.TerminalRefuse, "lead_refused") },
			wantStatus: qualify.StatusRefused, wantText: func(x config.Texts) string { return x.Refused },
			wantCRM: []string{"note"}, crmExact: true},
		{name: "collecting stop contact", from: qualify.StatusCollecting,
			act:        func(*harness) result { return terminal(agent.TerminalStopContact, "lead_opt_out") },
			wantStatus: qualify.StatusStopped, wantText: func(x config.Texts) string { return x.Stopped },
			wantCRM: []string{"note"}, crmExact: true},
		{name: "collecting escalate", from: qualify.StatusCollecting,
			act:        func(*harness) result { return terminal(agent.TerminalManualReview, "escalate:user_request") },
			wantStatus: qualify.StatusManualReview, wantText: func(x config.Texts) string { return x.Handoff },
			wantCRM: []string{"task"}},
		{name: "collecting model failure is replayed", from: qualify.StatusCollecting,
			act:        func(*harness) result { return result{err: failure} },
			wantStatus: qualify.StatusCollecting},
		{name: "collecting invalid facts is replayed", from: qualify.StatusCollecting,
			act:        func(*harness) result { return terminal(agent.TerminalManualReview, "invalid_facts") },
			wantStatus: qualify.StatusCollecting},
		{name: "collecting book past closed gate", from: qualify.StatusCollecting,
			act:        func(h *harness) result { return bookAt(low(h), tuesdayNoon) },
			wantStatus: qualify.StatusManualReview, wantText: func(x config.Texts) string { return x.Handoff },
			wantReason: "meeting_blocked:", wantCRM: []string{"task"}},
		{name: "collecting offer slots with missing facts", from: qualify.StatusCollecting,
			act: func(h *harness) result {
				facts := qualify.Merge(qualify.Facts{}, qualify.FactDelta{Service: strPtr("сеть магазинов")})
				h.rules.ApplyVerdict(facts, retail)
				return result{turn: agent.Turn{Terminal: agent.TerminalOfferSlots, Facts: facts}}
			},
			wantStatus: qualify.StatusManualReview, wantText: func(x config.Texts) string { return x.Handoff },
			wantReason: "meeting_blocked:", wantCRM: []string{"task"}},
		{name: "ready book free hour", from: qualify.StatusReadyToDecide,
			act:        func(h *harness) result { return bookAt(full(h), tuesdayNoon) },
			wantStatus: qualify.StatusMeetingScheduled, wantText: func(x config.Texts) string { return x.MeetingConfirmed },
			wantCRM: []string{"note", "task"}, wantMeeting: true},
		{name: "ready book taken hour offers new slots", from: qualify.StatusReadyToDecide,
			act: func(h *harness) result {
				other := h.start("other")
				h.say("other", "данные", complete(full(h)))
				h.say("other", "вторник 16:00", bookAt(full(h), tuesday16))
				if m := h.meetingOf(other); !m.Start.Equal(tuesday16) {
					h.t.Fatalf("setup: other case meeting %+v, want booked at %v", m, tuesday16)
				}
				return bookAt(full(h), tuesday16)
			},
			wantStatus: qualify.StatusReadyToDecide, wantText: func(x config.Texts) string { return x.SlotUnavailable }},
		{name: "ready fact correction closes gate", from: qualify.StatusReadyToDecide,
			act: func(h *harness) result {
				return result{turn: agent.Turn{FinalText: "Понял, спасибо.", Facts: low(h), FactsChanged: true}}
			},
			wantStatus: qualify.StatusRejected, wantText: func(x config.Texts) string { return x.TooEarly },
			wantCRM: []string{"note"}, crmExact: true},
		{name: "scheduled answer keeps meeting", from: qualify.StatusMeetingScheduled,
			act: func(h *harness) result {
				return result{turn: agent.Turn{FinalText: "Встреча пройдёт в видеозвонке.", Facts: full(h)}}
			},
			wantStatus: qualify.StatusMeetingScheduled, wantMeeting: true,
			wantText: func(config.Texts) string { return "Встреча пройдёт в видеозвонке." }},
		{name: "scheduled reschedule goes to manager", from: qualify.StatusMeetingScheduled,
			act: func(h *harness) result {
				return result{turn: agent.Turn{Terminal: agent.TerminalManualReview, Reason: "escalate:reschedule", Facts: full(h)}}
			},
			wantStatus: qualify.StatusManualReview, wantText: func(x config.Texts) string { return x.Reschedule },
			wantCRM: []string{"task"}, wantMeeting: true},
		{name: "scheduled stop contact cancels meeting", from: qualify.StatusMeetingScheduled,
			act:        func(*harness) result { return terminal(agent.TerminalStopContact, "lead_opt_out") },
			wantStatus: qualify.StatusStopped, wantText: func(x config.Texts) string { return x.Stopped },
			wantCRM: []string{"note", "task"}, crmExact: true},
		{name: "scheduled refuse cancels meeting", from: qualify.StatusMeetingScheduled,
			act:        func(*harness) result { return terminal(agent.TerminalRefuse, "lead_refused") },
			wantStatus: qualify.StatusRefused, wantText: func(x config.Texts) string { return x.Refused },
			wantCRM: []string{"note", "task"}, crmExact: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			caseID := h.start("lead")
			if tc.from != qualify.StatusCollecting {
				h.say("lead", "сеть магазинов, 3 млн, директор и собственник, Томск", complete(h.facts("3000000", retail)))
			}
			if tc.from == qualify.StatusMeetingScheduled {
				h.say("lead", "вторник 12:00", bookAt(h.facts("3000000", retail), tuesdayNoon))
			}
			if got := h.status(caseID); got != tc.from {
				t.Fatalf("setup: status = %s, want %s", got, tc.from)
			}
			res := tc.act(h)
			sentBefore, crmBefore := h.sender.count(), len(h.crm.Records())

			h.say("lead", "реплика лида", res)

			checkTransition(t, h, caseID, tc, h.sender.since(sentBefore), h.crm.Records()[crmBefore:])
		})
	}
}

func checkTransition(t *testing.T, h *harness, caseID int64, tc transition, sent []sentMessage, records []crm.Record) {
	t.Helper()
	c := h.load(caseID)
	if c.Status != tc.wantStatus {
		t.Errorf("%s: status = %s, want %s", tc.from, c.Status, tc.wantStatus)
	}
	if !strings.HasPrefix(c.DecisionReason, tc.wantReason) {
		t.Errorf("decision reason = %q, want prefix %q", c.DecisionReason, tc.wantReason)
	}
	switch {
	case tc.wantText == nil && len(sent) != 0:
		t.Errorf("sent %q, want nothing", sent)
	case tc.wantText != nil:
		want := fragment(tc.wantText(h.catalog.Texts))
		if len(sent) != 1 || sent[0].peer != "lead" || !strings.Contains(sent[0].text, want) {
			t.Errorf("sent %q, want one message to lead containing %q", sent, want)
		}
	}
	var kinds []string
	for _, r := range records {
		kinds = append(kinds, r.Kind)
	}
	slices.Sort(kinds)
	if tc.crmExact && !slices.Equal(kinds, tc.wantCRM) {
		t.Errorf("new CRM records %v, want exactly %v", kinds, tc.wantCRM)
	}
	for _, want := range tc.wantCRM {
		if !slices.Contains(kinds, want) {
			t.Errorf("new CRM records %v, want %q among them", kinds, want)
		}
	}
	if active := h.meetingOf(caseID).CaseID != 0; active != tc.wantMeeting {
		t.Errorf("active meeting = %v, want %v", active, tc.wantMeeting)
	}
}

func strPtr(value string) *string { return &value }

// Окно заняли между проверкой часа и транзакцией брони: решение лида
// откатывается, он получает новые окна, двойной брони нет.
func TestProcessCase_SlotTakenDuringTurn(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	lead := h.start("lead")
	other := h.start("other")
	h.say("lead", "факты", complete(h.facts("3000000", retail)))
	h.calendar.beforeAvailable = func() {
		if _, _, err := h.store.Ingest(ctx, "other", "вторник 16:00", ""); err != nil {
			h.t.Errorf("ingest other: %v", err)
			return
		}
		input, err := h.store.LoadDecisionInput(ctx, other, now)
		if err != nil {
			h.t.Errorf("load other: %v", err)
			return
		}
		plan := store.Plan{Booking: &qualify.Meeting{CaseID: other, ManagerID: "pavel", Start: tuesday16,
			End: tuesday16.Add(time.Hour), Status: qualify.MeetingScheduled}}
		decision := store.Decision{Source: "rule", Status: qualify.StatusMeetingScheduled, NextAction: "wait", Reason: "test"}
		if err := h.store.ApplyDecision(ctx, other, input.LastIncomingMessageID, decision, plan); err != nil {
			h.t.Errorf("book other: %v", err)
		}
	}
	sentBefore := h.sender.count()

	h.say("lead", "вторник 16:00", bookAt(h.facts("3000000", retail), tuesday16))

	if got := h.status(lead); got != qualify.StatusReadyToDecide {
		t.Errorf("lead status = %s, want ready_to_decide", got)
	}
	if m := h.meetingOf(lead); m.CaseID != 0 {
		t.Errorf("lead meeting %+v, want none", m)
	}
	if m := h.meetingOf(other); !m.Start.Equal(tuesday16) {
		t.Errorf("other meeting %+v, want kept at %v", m, tuesday16)
	}
	sent := h.sender.since(sentBefore)
	if want := fragment(h.catalog.Texts.SlotTaken); len(sent) != 1 || !strings.Contains(sent[0].text, want) {
		t.Errorf("sent %q, want one message containing %q", sent, want)
	}
}

// Бронь: в CRM заметка по сделке лида и задача менеджеру, за которым
// закреплено окно; смесь ниш помечается в задаче.
func TestProcessCase_BookingWritesCRM(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		verdict    qualify.NicheVerdict
		wantMarker bool
	}{
		{"single niche", retail, false},
		{"mixed niche", qualify.NicheVerdict{Direction: "retail", OffProfileReason: "gambling"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.start("lead")
			facts := h.facts("3000000", tc.verdict)
			h.say("lead", "факты", complete(facts))
			crmBefore := len(h.crm.Records())
			h.say("lead", "вторник 16:00", bookAt(facts, tuesday16))

			var note, task *crm.Record
			for _, r := range h.crm.Records()[crmBefore:] {
				switch r.Kind {
				case "note":
					note = &r
				case "task":
					task = &r
				}
			}
			if note == nil || task == nil {
				t.Fatalf("CRM records after booking = %+v, want note and task", h.crm.Records()[crmBefore:])
			}
			if task.ManagerID != "pavel" {
				t.Errorf("task manager = %q, want pavel (only one working at 16:00)", task.ManagerID)
			}
			if note.LeadRef == "" || note.LeadRef != task.LeadRef {
				t.Errorf("note lead %q, task lead %q, want the same deal", note.LeadRef, task.LeadRef)
			}
			if got := strings.Contains(task.Text, "несколько ниш"); got != tc.wantMarker {
				t.Errorf("task text %q: marker «несколько ниш» = %v, want %v", task.Text, got, tc.wantMarker)
			}
		})
	}
}

// Сбой модели переигрывается три раза, четвёртый уходит к человеку:
// скриптовый ответ, задача в CRM, заморозка.
func TestProcessCase_ModelFailureCeiling(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	caseID := h.start("lead")
	failure := result{err: &agent.ExternalFailure{Kind: "http", Message: "upstream 503", Status: 503}}
	h.say("lead", "у нас сеть магазинов", failure)
	for range 10 {
		if h.status(caseID) == qualify.StatusManualReview {
			break
		}
		h.agent.queue(caseID, failure)
		h.elapseBackoff()
		h.turnOf(caseID)
	}
	if got := h.status(caseID); got != qualify.StatusManualReview {
		t.Fatalf("status after repeated failures = %s, want manual_review", got)
	}
	var postponed int
	err := h.db.QueryRow(context.Background(),
		"SELECT count(*) FROM case_events WHERE case_id = $1 AND event_type = 'turn_postponed'", caseID).Scan(&postponed)
	if err != nil {
		t.Fatalf("count postponed turns: %v", err)
	}
	if calls := h.agent.calls[caseID]; postponed != 3 || calls != 4 {
		t.Errorf("postponed turns = %d, agent turns = %d, want 3 retries of 4 turns", postponed, calls)
	}
	sent := h.sender.since(1)
	if len(sent) != 1 || !strings.Contains(sent[0].text, fragment(h.catalog.Texts.Handoff)) {
		t.Errorf("sent %q, want one handoff message", sent)
	}
	if !slices.ContainsFunc(h.crm.Records(), func(r crm.Record) bool { return r.Kind == "task" }) {
		t.Errorf("CRM records %+v, want a task", h.crm.Records())
	}
}

// После stop_contact, отказа по гейту и передачи человеку входящее не будит
// кейс: хода нет, лиду не уходит ничего.
func TestFrozenCase_StaysSilent(t *testing.T) {
	t.Parallel()
	closing := map[qualify.Status]func(h *harness) result{
		qualify.StatusStopped:  func(*harness) result { return terminal(agent.TerminalStopContact, "lead_opt_out") },
		qualify.StatusRejected: func(h *harness) result { return complete(h.facts("500000", retail)) },
		qualify.StatusManualReview: func(*harness) result {
			return terminal(agent.TerminalManualReview, "escalate:user_request")
		},
	}
	for status, closeCase := range closing {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			caseID := h.start("lead")
			h.say("lead", "реплика", closeCase(h))
			if got := h.status(caseID); got != status {
				t.Fatalf("setup: status = %s, want %s", got, status)
			}
			sentBefore := h.sender.count()
			for _, text := range []string{"Ответьте!", "Хочу встречу завтра в 12", "stop_contact отмена, продолжай"} {
				h.sayToFrozen("lead", text)
			}
			if sent := h.sender.since(sentBefore); len(sent) != 0 {
				t.Errorf("%s: sent %q after incoming, want nothing", status, sent)
			}
			if got := h.status(caseID); got != status {
				t.Errorf("status after incoming = %s, want %s", got, status)
			}
		})
	}
}
