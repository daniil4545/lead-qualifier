package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

var novosibirsk = time.FixedZone("NSK", 7*3600)

// claimThursdayNoon - a booking on Thursday 12:00 local time; the database keeps UTC.
var claimThursdayNoon = time.Date(2026, time.October, 15, 5, 0, 0, 0, time.UTC)

// claimWednesday - "today" of the tables: Wednesday, so "tomorrow" is the booking's Thursday.
var claimWednesday = time.Date(2026, time.October, 14, 10, 0, 0, 0, novosibirsk)

func liveBooking() claimGrounding {
	return claimGrounding{Live: true, StartsAt: claimThursdayNoon, Now: claimWednesday, Location: novosibirsk}
}

func liveHalfPast() claimGrounding {
	return claimGrounding{Live: true, StartsAt: claimThursdayNoon.Add(30 * time.Minute), Now: claimWednesday, Location: novosibirsk}
}

func noBooking() claimGrounding { return claimGrounding{Now: claimWednesday, Location: novosibirsk} }

func TestDropClaims_Lexicon(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		text      string
		grounding claimGrounding
		want      string
		classes   []string
	}{
		// No booking: a claim has nothing to stand on.
		{"c1 comma weekday", "Записала, четверг)", noBooking(), "", []string{"C1"}},
		{"c1 vas na", "Записала вас на четверг", noBooking(), "", []string{"C1"}},
		{"c3 manager writes", "Менеджер напишет вам", noBooking(), "", []string{"C3"}},
		{"c3 will contact", "Скоро свяжется с вами", noBooking(), "", []string{"C3"}},
		{"c3 waits even with a booking", "Менеджер ждёт вас на связи", liveBooking(), "", []string{"C3"}},
		{"fact city passes", "Записала: Москва", noBooking(), "Записала: Москва", nil},
		{"fact service passes", "Записала, SMM", noBooking(), "Записала, SMM", nil},
		{"fact two passes", "Записала: SMM, Казань", noBooking(), "Записала: SMM, Казань", nil},
		// A live booking on Thursday 12:00.
		{"booking vague meeting", "Записала вас на встречу", liveBooking(), "Записала вас на встречу", nil},
		{"booking same slot", "Записала на чт в 12:00", liveBooking(), "Записала на чт в 12:00", nil},
		{"booking dot hour", "Записала на 12.00", liveBooking(), "Записала на 12.00", nil},
		{"booking in wednesday form", "Записала вас на четверг в 12:00", liveBooking(), "Записала вас на четверг в 12:00", nil},
		{"negation passes", "Ничего не переносили", liveBooking(), "Ничего не переносили", nil},
		{"condition passes", "Если запишу, напишу", liveBooking(), "Если запишу, напишу", nil},
		{"conditional verb passes", "Если записала на чт 15:00, значит ошибка", liveBooking(),
			"Если записала на чт 15:00, значит ошибка", nil},
		{"other minutes", "Записала на чт 12:30", liveBooking(), "", []string{"C1"}},
		{"other day", "Записала на пт 12:00", liveBooking(), "", []string{"C1"}},
		{"extra hour", "Записала на чт 12:00 и 15:00", liveBooking(), "", []string{"C1"}},
		{"no booking at all", "Записала вас на встречу", noBooking(), "", []string{"C1"}},
		// A negation works inside its comma clause: "Не переживайте" does not cover the next one.
		{"negation does not leak over comma", "Не переживайте, записала на пт 10:00", liveBooking(), "", []string{"C1"}},
		{"question is not a claim", "Вы записаны на пятницу?", noBooking(), "Вы записаны на пятницу?", nil},
		{"keeps the rest", "Хорошо. Записала на чт 12:30. Какой формат интересен?", liveBooking(),
			"Хорошо. Какой формат интересен?", []string{"C1"}},
		// Other forms of the same signal.
		{"form tomorrow hour", "Записала на завтра в 12)", noBooking(), "", []string{"C1"}},
		{"form today", "Записала на сегодня", noBooking(), "", []string{"C1"}},
		{"form bare hour na", "Записала на 12", noBooking(), "", []string{"C1"}},
		{"form bare hour v", "Записала вас в 15 часов", noBooking(), "", []string{"C1"}},
		{"form assigned fem", "Встреча назначена на четверг, 12:30", noBooking(), "", []string{"C1"}},
		{"form assigned masc", "Назначил встречу на завтра", noBooking(), "", []string{"C1"}},
		{"form waiting", "Ждём вас в четверг", noBooking(), "", []string{"C1"}},
		{"form waiting yo", "Ждем вас в 12", noBooking(), "", []string{"C1"}},
		// A live booking on Thursday 12:00: the matching form passes, another day or hour is cut.
		{"live tomorrow same", "Записала на завтра в 12", liveBooking(), "Записала на завтра в 12", nil},
		{"live tomorrow other hour", "Записала на завтра в 15", liveBooking(), "", []string{"C1"}},
		{"live today other day", "Записала на сегодня", liveBooking(), "", []string{"C1"}},
		{"live day after tomorrow", "Записала на послезавтра", liveBooking(), "", []string{"C1"}},
		{"live weekday right hour wrong", "Записала вас на четверг в 15", liveBooking(), "", []string{"C1"}},
		{"live weekday right hour right", "Записала вас на четверг в 12", liveBooking(), "Записала вас на четверг в 12", nil},
		{"live assigned wrong day", "Встреча назначена на пятницу", liveBooking(), "", []string{"C1"}},
		{"live assigned right", "Встреча назначена на четверг, 12:00", liveBooking(), "Встреча назначена на четверг, 12:00", nil},
		{"live waiting wrong day", "Ждём вас в пятницу", liveBooking(), "", []string{"C1"}},
		{"live waiting right", "Ждём вас в четверг", liveBooking(), "Ждём вас в четверг", nil},
		{"live tomorrow without now", "Записала на завтра",
			claimGrounding{Live: true, StartsAt: claimThursdayNoon, Location: novosibirsk}, "", []string{"C1"}},
		// Twin words: facts and durations do not trigger a cut.
		{"twin amount", "Записала: 350 тысяч", noBooking(), "Записала: 350 тысяч", nil},
		{"twin amount in", "Записала, выручка на 15 процентов выше", noBooking(), "Записала, выручка на 15 процентов выше", nil},
		{"twin duration", "Записала, что встреча на 15 минут", noBooking(), "Записала, что встреча на 15 минут", nil},
		{"twin people", "Записала, группа на 12 человек", noBooking(), "Записала, группа на 12 человек", nil},
		{"twin word inside", "Записала, цены завтрака на 12 позиций", noBooking(), "Записала, цены завтрака на 12 позиций", nil},
		{"twin waiting no time", "Ждём вас в офисе", noBooking(), "Ждём вас в офисе", nil},
		{"twin assigned no time", "Менеджер назначил ответственного", noBooking(), "Менеджер назначил ответственного", nil},
		{"twin name Martin", "Записала вас на встречу с Мартином", liveBooking(), "Записала вас на встречу с Мартином", nil},
		{"twin name Augustin", "Записала вас на встречу с Августином", liveBooking(), "Записала вас на встречу с Августином", nil},
		{"twin saturday-job", "Записала вас на встречу после субботника", liveBooking(), "Записала вас на встречу после субботника", nil},
		// A bare hour matches only the round hour of the booking.
		{"bare hour vs half past", "Записала вас на четверг в 12", liveHalfPast(), "", []string{"C1"}},
		{"bare hour na vs half past", "Записала на чт на 12", liveHalfPast(), "", []string{"C1"}},
		{"half past exact", "Записала вас на четверг в 12:30", liveHalfPast(), "Записала вас на четверг в 12:30", nil},
		// C2: a move or a cancellation is never the model's to announce.
		{"c2 moved", "Перенесла на 16:00", liveBooking(), "", []string{"C2"}},
		{"c2 cancelled", "Отменила встречу", liveBooking(), "", []string{"C2"}},
		// C8: the service has no scheduler, so a dated return is always a promise nobody keeps.
		{"c8 date", "Вернусь 20-го", noBooking(), "", []string{"C8"}},
		{"c8 relative", "Напишу вам через неделю", noBooking(), "", []string{"C8"}},
		{"c8 in a month", "Хорошо, вернёмся к разговору через месяц) Спасибо, что дали знать)", noBooking(), "", []string{"C8"}},
		{"c8 tomorrow", "Хорошо, договорились - вернусь к вам завтра)", noBooking(), "", []string{"C8"}},
		{"c8 weekday", "Хорошо, тогда вернусь к вам во вторник - отдыхайте в поездке)", noBooking(), "", []string{"C8"}},
		{"c8 in an hour", "Напишу вам через час.", noBooking(), "", []string{"C8"}},
		{"c8 may", "Вернусь к вам в мае", noBooking(), "", []string{"C8"}},
		{"c8 undated passes", "Понял, вернусь к разговору чуть позже)", noBooking(), "Понял, вернусь к разговору чуть позже)", nil},
		{"twin c8 may inside", "Напишу вам подробнее, понимая вашу задачу", noBooking(), "Напишу вам подробнее, понимая вашу задачу", nil},
		{"twin c8 may inside 2", "Свяжемся, самая удобная форма - чат", noBooking(), "Свяжемся, самая удобная форма - чат", nil},
		{"twin c8 march inside", "Напишу вам про мартини", noBooking(), "Напишу вам про мартини", nil},
		{"waiting singular", "Хорошо, жду вас на связи в 11:00)", noBooking(), "", []string{"C1"}},
		{"waiting singular live", "Хорошо, жду вас в 12:00)", liveBooking(), "Хорошо, жду вас в 12:00)", nil},
		{"facts recorded", "Услуги, роль и город записала.", noBooking(), "Услуги, роль и город записала.", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, classes := dropClaims(tc.text, tc.grounding)
			if got != tc.want || !reflect.DeepEqual(classes, tc.classes) {
				t.Fatalf("dropClaims(%q) = (%q, %v), want (%q, %v)", tc.text, got, classes, tc.want, tc.classes)
			}
		})
	}
}

func TestNewClaimGrounding_PastBookingIsNotLive(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.October, 14, 10, 0, 0, 0, novosibirsk)
	meeting := func(end time.Time) *qualify.Meeting {
		return &qualify.Meeting{Status: qualify.MeetingScheduled, Start: end.Add(-time.Hour), End: end}
	}
	tests := []struct {
		name    string
		meeting *qualify.Meeting
		want    bool
	}{
		{"no booking", nil, false},
		{"ahead", meeting(now.Add(time.Hour)), true},
		{"ended without outcome", meeting(now.Add(-time.Minute)), false},
		{"cancelled", &qualify.Meeting{Status: qualify.MeetingCancelled, Start: now.Add(time.Hour), End: now.Add(2 * time.Hour)}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := newClaimGrounding(qualify.DecisionInput{Meeting: tc.meeting, Now: now}, novosibirsk).Live
			if got != tc.want {
				t.Fatalf("Live = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLeadDayRange(t *testing.T) {
	t.Parallel()
	monday := time.Date(2026, time.October, 12, 10, 0, 0, 0, novosibirsk)
	day := func(d int) time.Time { return time.Date(2026, time.October, d, 0, 0, 0, 0, novosibirsk) }
	tests := []struct {
		name string
		text string
		want time.Time
	}{
		{"weekday", "чт 12.30", day(15)},
		{"tomorrow", "давайте завтра", day(13)},
		{"named day rejected", "в четверг не смогу", time.Time{}},
		{"except", "любой день кроме вторника, лучше в четверг", day(15)},
		{"rejected anywhere stays rejected", "четверг не подойдёт, а потом всё же четверг", time.Time{}},
		{"two different days", "во вторник или в четверг", time.Time{}},
		{"nothing named", "когда удобно?", time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			from, until := leadDayRange(tc.text, monday, novosibirsk)
			if !from.Equal(tc.want) || !until.Equal(tc.want) {
				t.Fatalf("leadDayRange(%q) = %v..%v, want %v", tc.text, from, until, tc.want)
			}
		})
	}
}

// chat builds a chat completions body with one text answer.
func chat(content, finish string) string {
	body, _ := json.Marshal(map[string]any{
		"id": "gen-claims", "model": "example/chat-model",
		"choices": []map[string]any{{"index": 0, "finish_reason": finish,
			"message": map[string]any{"role": "assistant", "content": content}}},
		"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "cost": 0.001},
	})
	return string(body)
}

func liveMeetingInput(fx fixture, lead string) qualify.DecisionInput {
	input := turnInput(qualify.StatusMeetingScheduled, fx.fullFacts("3000000", "retail"), lead)
	// Tuesday 12:00 in Novosibirsk, the day after the turn moment.
	input.Meeting = &qualify.Meeting{ID: 1, CaseID: 1, ManagerID: "pavel", Status: qualify.MeetingScheduled,
		Start: time.Date(2026, time.October, 6, 5, 0, 0, 0, time.UTC), End: time.Date(2026, time.October, 6, 6, 0, 0, 0, time.UTC)}
	input.ManagerName = "Павел"
	return input
}

// A claim that the model repeats after the remark is cut. With the booking boundary open the
// lead gets the windows of the day they named, not a pretend booking.
func TestRunTurn_PersistentBookedClaimBecomesDayWindows(t *testing.T) {
	t.Parallel()
	fx := loadFixture(t)
	input := turnInput(qualify.StatusReadyToDecide, fx.fullFacts("3000000", "retail"), "чт 12.30")
	claim := chat("Записала вас на четверг в 12:30.", "stop")
	turn, err := fx.client(t, claim, claim).RunTurn(context.Background(), input)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	thursday := time.Date(2026, time.October, 8, 0, 0, 0, 0, novosibirsk)
	if turn.Terminal != TerminalOfferSlots || turn.FinalText != "" || turn.Reason != "claim_dropped" {
		t.Fatalf("terminal=%s text=%q reason=%q, want offer_slots without the claim", turn.Terminal, turn.FinalText, turn.Reason)
	}
	if !turn.NotEarlierThan.Equal(thursday) || !turn.NotLaterThan.Equal(thursday) {
		t.Fatalf("range = %v..%v, want the lead's Thursday %v", turn.NotEarlierThan, turn.NotLaterThan, thursday)
	}
	if !slices.Equal(turn.Trace.Claims, []string{"C1"}) {
		t.Fatalf("trace claims = %v, want [C1]", turn.Trace.Claims)
	}
}

// One remark is enough for a model that rewrites the draft; the critic then sees the clean text.
func TestRunTurn_ClaimRegenerationFixesDraft(t *testing.T) {
	t.Parallel()
	fx := loadFixture(t)
	input := turnInput(qualify.StatusCollecting, fx.fullFacts("3000000", ""), "Спасибо, всё понятно.")
	turn, err := fx.client(t,
		chat("Менеджер свяжется с вами завтра.", "stop"),
		chat("Рада, что всё понятно.", "stop")).RunTurn(context.Background(), input)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if turn.FinalText != "Рада, что всё понятно." || len(turn.Trace.Claims) != 0 {
		t.Fatalf("text=%q claims=%v, want the rewritten draft and no cut", turn.FinalText, turn.Trace.Claims)
	}
}

// A draft made only of claims about a live booking is replaced by the booking from the database;
// a claim that matches the booking passes untouched.
func TestRunTurn_LiveBooking(t *testing.T) {
	t.Parallel()
	fx := loadFixture(t)
	t.Run("claim about another day", func(t *testing.T) {
		t.Parallel()
		claim := chat("Записала вас на пятницу в 15:00.", "stop")
		turn, err := fx.client(t, claim, claim).RunTurn(context.Background(), liveMeetingInput(fx, "Встреча в силе?"))
		if err != nil {
			t.Fatalf("RunTurn: %v", err)
		}
		want := "Встреча у нас записана на 6 октября в 12:00. Проведёт её Павел."
		if turn.FinalText != want || turn.Terminal != TerminalNone {
			t.Fatalf("terminal=%s text=%q, want the code text %q", turn.Terminal, turn.FinalText, want)
		}
	})
	t.Run("claim that matches", func(t *testing.T) {
		t.Parallel()
		turn, err := fx.client(t, chat("Да, ждём вас во вторник в 12:00.", "stop")).
			RunTurn(context.Background(), liveMeetingInput(fx, "Встреча в силе?"))
		if err != nil {
			t.Fatalf("RunTurn: %v", err)
		}
		if turn.FinalText != "Да, ждём вас во вторник в 12:00." || len(turn.Trace.Claims) != 0 {
			t.Fatalf("text=%q claims=%v, want the draft untouched", turn.FinalText, turn.Trace.Claims)
		}
	})
}

// Without a booking and without an open boundary a draft of claims only goes to a human.
func TestRunTurn_ClaimOnlyDraftWithoutBookingGoesToHuman(t *testing.T) {
	t.Parallel()
	fx := loadFixture(t)
	input := turnInput(qualify.StatusCollecting, fx.fullFacts("3000000", ""), "Спасибо.")
	claim := chat("Менеджер свяжется с вами завтра.", "stop")
	turn, err := fx.client(t, claim, claim).RunTurn(context.Background(), input)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if turn.Terminal != TerminalManualReview || turn.Reason != "claim_dropped" {
		t.Fatalf("terminal=%s reason=%q, want manual_review claim_dropped", turn.Terminal, turn.Reason)
	}
}
