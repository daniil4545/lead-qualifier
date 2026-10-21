package calendar

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/daniil4545/lead-qualifier/internal/config"
)

var zone = time.FixedZone("UTC+7", 7*3600)

// local - a moment in the catalog's local time; October 5, 2026 is a Monday.
func local(day, hour, minute int) time.Time {
	return time.Date(2026, time.October, day, hour, minute, 0, 0, zone)
}

func newCalendar(t *testing.T, managers ...config.Manager) *Local {
	t.Helper()
	return newCalendarWith(t, "10:00", "18:00", 60, managers...)
}

// newCalendarWith - a calendar with common hours open-close on weekdays and a meeting
// of the given length.
func newCalendarWith(t *testing.T, open, closing string, minutes int, managers ...config.Manager) *Local {
	t.Helper()
	hours := config.WorkHours{}
	for _, day := range []string{"mon", "tue", "wed", "thu", "fri"} {
		hours[day] = [2]string{open, closing}
	}
	cal, err := NewLocal(config.Catalog{
		Timezone:       "Asia/Novosibirsk",
		Location:       zone,
		WorkHours:      hours,
		MeetingMinutes: minutes,
		Managers:       managers,
	})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return cal
}

func busyAt(start time.Time) []Interval {
	return []Interval{{Start: start, End: start.Add(time.Hour)}}
}

// Manager: the earliest window, on a tie the smaller Priority, then id.
func TestOffer_ManagerChoice(t *testing.T) {
	t.Parallel()
	now := local(5, 7, 30)
	cases := []struct {
		name      string
		managers  []config.Manager
		busy      Busy
		wantID    string
		wantStart time.Time
	}{
		{"lower priority wins tie", []config.Manager{{ID: "b", Priority: 2}, {ID: "a", Priority: 1}}, nil, "a", local(5, 10, 0)},
		{"earliest start beats priority", []config.Manager{{ID: "a", Priority: 1}, {ID: "b", Priority: 2}},
			Busy{"a": busyAt(local(5, 10, 0))}, "b", local(5, 10, 0)},
		{"equal priority falls to id", []config.Manager{{ID: "b", Priority: 1}, {ID: "a", Priority: 1}}, nil, "a", local(5, 10, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			slots := newCalendar(t, tc.managers...).Offer(tc.busy, now, time.Time{}, time.Time{})
			if len(slots) == 0 {
				t.Fatal("Offer returned no slots")
			}
			if !slots[0].Start.Equal(tc.wantStart) {
				t.Errorf("first slot = %v, want %v", slots[0].Start, tc.wantStart)
			}
			for _, slot := range slots {
				if slot.ManagerID != tc.wantID {
					t.Fatalf("slot %v of manager %q, want only %q", slot.Start, slot.ManagerID, tc.wantID)
				}
			}
		})
	}
}

func TestAvailable(t *testing.T) {
	t.Parallel()
	managers := []config.Manager{{ID: "b", Priority: 2}, {ID: "a", Priority: 1}}
	now := local(5, 7, 30)
	cases := []struct {
		name   string
		busy   Busy
		now    time.Time
		start  time.Time
		wantID string // empty means no window
	}{
		{"free hour goes to priority", nil, now, local(5, 12, 0), "a"},
		{"busy manager passes to next", Busy{"a": busyAt(local(5, 12, 0))}, now, local(5, 12, 0), "b"},
		{"all busy", Busy{"a": busyAt(local(5, 12, 0)), "b": busyAt(local(5, 12, 0))}, now, local(5, 12, 0), ""},
		{"outside work hours", nil, now, local(5, 19, 0), ""},
		{"weekend", nil, now, local(10, 12, 0), ""},
		{"closer than min lead", nil, local(5, 10, 30), local(5, 12, 0), ""},
		{"beyond horizon", nil, now, local(20, 12, 0), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			slot, ok := newCalendar(t, managers...).Available(tc.busy, tc.now, tc.start)
			got := ""
			if ok {
				got = slot.ManagerID
			}
			if got != tc.wantID {
				t.Fatalf("Available(%v) manager = %q, want %q", tc.start, got, tc.wantID)
			}
		})
	}
}

// Window grid: the first window is the start of the working day, the step is the meeting duration,
// the last window ends no later than the end of the day.
func TestOffer_GridFromOpeningByMeetingLength(t *testing.T) {
	t.Parallel()
	cases := []struct {
		open, closing string
		minutes       int
		first, last   time.Time // first and last start on Monday
	}{
		{"09:30", "17:30", 30, local(5, 9, 30), local(5, 17, 0)},
		{"10:00", "18:00", 60, local(5, 10, 0), local(5, 17, 0)},
		{"09:30", "17:00", 60, local(5, 9, 30), local(5, 15, 30)},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s-%s by %d", tc.open, tc.closing, tc.minutes), func(t *testing.T) {
			t.Parallel()
			step := time.Duration(tc.minutes) * time.Minute
			slots := newCalendarWith(t, tc.open, tc.closing, tc.minutes, config.Manager{ID: "a", Priority: 1}).
				Offer(nil, local(5, 7, 0), time.Time{}, time.Time{})
			var monday []time.Time
			for _, slot := range slots {
				if slot.End.Sub(slot.Start) != step {
					t.Errorf("slot %v-%v, want length %v", slot.Start, slot.End, step)
				}
				if slot.Start.In(zone).Day() == 5 {
					monday = append(monday, slot.Start)
				}
			}
			var want []time.Time
			for start := tc.first; !start.After(tc.last); start = start.Add(step) {
				want = append(want, start)
			}
			if !slices.EqualFunc(monday, want, time.Time.Equal) {
				t.Errorf("monday starts = %v, want %v", monday, want)
			}
		})
	}
}

func TestAvailable_GridByMeetingLength(t *testing.T) {
	t.Parallel()
	cal := newCalendarWith(t, "09:30", "17:30", 30, config.Manager{ID: "a", Priority: 1})
	now := local(5, 7, 0)
	cases := []struct {
		start time.Time
		want  bool
	}{
		{local(5, 9, 30), true},
		{local(5, 10, 0), true},
		{local(5, 17, 0), true},
		{local(5, 9, 0), false},
		{local(5, 9, 45), false},
		{local(5, 17, 30), false},
	}
	for _, tc := range cases {
		if _, ok := cal.Available(nil, now, tc.start); ok != tc.want {
			t.Errorf("Available(%s) = %v, want %v", tc.start.Format("15:04"), ok, tc.want)
		}
	}
}
