package dialog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/daniil4545/lead-qualifier/internal/calendar"
	"github.com/daniil4545/lead-qualifier/internal/qualify"
	"github.com/daniil4545/lead-qualifier/internal/store"
)

var zeroTime time.Time

// busy - manager occupancy by database bookings over the selection horizon. The start is
// a day back: a meeting that began before now can still occupy a window.
func (w *Worker) busy(ctx context.Context, now time.Time) (calendar.Busy, error) {
	meetings, err := w.store.BusyMeetings(ctx, now.Add(-24*time.Hour), now.Add(calendar.Horizon+24*time.Hour))
	if err != nil {
		return nil, err
	}
	busy := calendar.Busy{}
	for _, meeting := range meetings {
		busy[meeting.ManagerID] = append(busy[meeting.ManagerID], calendar.Interval{Start: meeting.Start, End: meeting.End})
	}
	return busy, nil
}

// validRange brings the lead's window to a selection range: the past is dropped, the upper
// bound includes the named day entirely, an inverted range is dropped
// entirely. The window came from model output, a typo must not give an empty selection.
func (w *Worker) validRange(now, from, until time.Time) (time.Time, time.Time) {
	if !from.After(now) {
		from = zeroTime
	}
	if !until.IsZero() {
		until = qualify.StartOfDay(until, w.catalog.Location).AddDate(0, 0, 1)
		if !until.After(now) || (!from.IsZero() && !until.After(from)) {
			return zeroTime, zeroTime
		}
	}
	return from, until
}

// offerSlots selects windows and plans showing them. reply is the model's reply, it
// goes first; prefix is the code's line before the list (a busy hour,
// an intercepted window).
func (w *Worker) offerSlots(ctx context.Context, input qualify.DecisionInput, reply, prefix string, from, until time.Time) (store.Decision, store.Plan, error) {
	now := w.now()
	busy, err := w.busy(ctx, now)
	if err != nil {
		return store.Decision{}, store.Plan{}, err
	}
	from, until = w.validRange(now, from, until)
	slots := w.calendar.Offer(busy, now, from, until)
	if len(slots) == 0 && (!from.IsZero() || !until.IsZero()) {
		slots = w.calendar.Offer(busy, now, zeroTime, zeroTime)
		prefix = joinLines(prefix, w.catalog.Texts.SlotsBeyondRequest)
	}
	if len(slots) == 0 {
		slog.Warn("meeting slots unavailable", "case_id", input.Case.ID)
		decision, plan, err := w.manualReview(input, "no_slots", "llm")
		plan.Reply.Text = withReply(reply, plan.Reply.Text)
		return decision, plan, err
	}
	offered := make([]qualify.OfferedSlot, 0, len(slots))
	for _, slot := range slots {
		offered = append(offered, qualify.OfferedSlot{ManagerID: slot.ManagerID, Start: slot.Start, End: slot.End})
	}
	slog.Info("meeting slots offered", "case_id", input.Case.ID, "slots", len(slots), "manager_id", slots[0].ManagerID)
	decision := store.Decision{Source: "llm", Status: qualify.StatusReadyToDecide, NextAction: "offer_slots",
		Reason: "предложены свободные окна", Facts: input.Case.Facts}
	return decision, store.Plan{
		Reply:    &store.Reply{Purpose: "slots_offer", Text: w.slotOfferText(offered, reply, prefix)},
		Slots:    offered,
		SlotsTTL: now.Add(offeredSlotsTTL),
	}, nil
}

// bookSlot books the hour the lead named if it is free for any of the
// managers. A busy hour gives a corrective showing of windows from the same day. The booking
// itself is written by the decision transaction, a double entry is rejected by the database.
func (w *Worker) bookSlot(ctx context.Context, input qualify.DecisionInput, requested time.Time) (store.Decision, store.Plan, error) {
	now := w.now()
	busy, err := w.busy(ctx, now)
	if err != nil {
		return store.Decision{}, store.Plan{}, err
	}
	slot, ok := w.calendar.Available(busy, now, requested)
	if !ok {
		slog.Info("requested hour unavailable", "case_id", input.Case.ID, "requested_at", requested)
		prefix := render(w.catalog.Texts.SlotUnavailable, "{time}", w.dateTime(requested))
		return w.offerSlots(ctx, input, "", prefix, qualify.StartOfDay(requested, w.catalog.Location), zeroTime)
	}
	manager := w.managerName(slot.ManagerID)
	confirmed := render(w.catalog.Texts.MeetingConfirmed,
		"{time}", w.dateTime(slot.Start), "{manager}", manager,
		"{duration}", fmt.Sprint(w.catalog.MeetingMinutes))
	task := "Встреча " + w.dateTime(slot.Start) + " с лидом " + input.Lead.Name + ". " + w.factsSummary(input.Case.Facts)
	if w.rules.Mixed(input.Case.Facts) {
		task += " Внимание: несколько ниш, уточните основное направление."
	}
	decision := store.Decision{Source: "llm", Status: qualify.StatusMeetingScheduled, NextAction: "book_slot",
		Reason: "встреча назначена", Facts: input.Case.Facts}
	return decision, store.Plan{
		Reply:     &store.Reply{Purpose: "meeting_confirmed", Text: confirmed},
		Booking:   &qualify.Meeting{CaseID: input.Case.ID, ManagerID: slot.ManagerID, Start: slot.Start, End: slot.End},
		DropSlots: true,
		CRMNote:   w.leadNote(input, "назначена встреча "+w.dateTime(slot.Start)+", менеджер "+manager),
		CRMTask:   &store.Task{ManagerID: slot.ManagerID, Text: task},
	}, nil
}

// slotOfferText - the list of windows as a line per day: consecutive windows merge
// into a range of starts, otherwise a list of 15 times reads as a wall of text.
func (w *Worker) slotOfferText(slots []qualify.OfferedSlot, reply, prefix string) string {
	if reply == "" && prefix == "" {
		prefix = w.catalog.Texts.SlotsHeadline
	}
	var text strings.Builder
	for _, part := range []string{reply, prefix} {
		if part != "" {
			text.WriteString(part + "\n")
		}
	}
	location := w.catalog.Location
	for start := 0; start < len(slots); {
		day := qualify.StartOfDay(slots[start].Start, location)
		end := start
		for end < len(slots) && qualify.StartOfDay(slots[end].Start, location).Equal(day) {
			end++
		}
		text.WriteString("- " + qualify.DayHeader(day, location) + ": " + startRanges(slots[start:end], location) + "\n")
		start = end
	}
	text.WriteString(w.catalog.Texts.SlotsFooter)
	return text.String()
}

// startRanges - "10:00, 12:00-15:30 inclusive": windows going one after
// another without a gap as one range of starts; the last time is the last
// possible start.
func startRanges(slots []qualify.OfferedSlot, location *time.Location) string {
	clock := func(at time.Time) string { return at.In(location).Format("15:04") }
	var pieces []string
	ranged := false
	for index := 0; index < len(slots); {
		first, last := slots[index], slots[index]
		index++
		for index < len(slots) && slots[index].Start.Equal(last.End) {
			last = slots[index]
			index++
		}
		if last.Start.After(first.Start) {
			pieces = append(pieces, clock(first.Start)+"-"+clock(last.Start))
			ranged = true
			continue
		}
		pieces = append(pieces, clock(first.Start))
	}
	text := strings.Join(pieces, ", ")
	if ranged {
		text += " включительно"
	}
	return text
}

func (w *Worker) managerName(id string) string {
	for _, manager := range w.catalog.Managers {
		if manager.ID == id {
			return manager.Name
		}
	}
	return id
}

func (w *Worker) dateTime(at time.Time) string {
	return qualify.DateTimeText(at, w.catalog.Location)
}
