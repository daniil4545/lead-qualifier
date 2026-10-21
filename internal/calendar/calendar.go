// Package calendar builds the free windows of managers: a grid of working hours with
// a step of the meeting duration minus occupancy. The booking does not live here -
// a double entry is held by uniqueness in the database.
package calendar

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/daniil4545/lead-qualifier/internal/config"
)

const (
	// Horizon - the bot does not book beyond this period from the current moment.
	Horizon = 14 * 24 * time.Hour
	// MinLead - the minimum gap from the current moment to a meeting.
	MinLead = 2 * time.Hour
	// ShowDays - how many calendar days with windows are shown to the lead at a time.
	ShowDays = 3
)

// Interval - a busy interval of a manager.
type Interval struct {
	Start time.Time
	End   time.Time
}

// Busy - occupancy by manager id.
type Busy map[string][]Interval

// Slot - a free window of a specific manager.
type Slot struct {
	ManagerID string
	Start     time.Time
	End       time.Time
}

// Calendar - where windows come from. The local implementation computes them from the catalog
// working hours; an external calendar would add its own occupancy.
type Calendar interface {
	// Offer - windows of the manager with the earliest free start in [from, until),
	// the first ShowDays days. Zero bounds mean from now+MinLead to now+Horizon.
	Offer(busy Busy, now, from, until time.Time) []Slot
	// Available - a free manager for an exact start.
	Available(busy Busy, now, start time.Time) (Slot, bool)
}

type managerHours struct {
	manager config.Manager
	windows map[time.Weekday]config.Window
}

// Local - a calendar by the catalog working hours.
type Local struct {
	location *time.Location
	duration time.Duration
	managers []managerHours // by Priority, then by id
}

// NewLocal builds the catalog calendar. A manager without own hours works by the
// common catalog hours.
func NewLocal(catalog config.Catalog) (*Local, error) {
	local := &Local{
		location: catalog.Location,
		duration: time.Duration(catalog.MeetingMinutes) * time.Minute,
	}
	for _, manager := range catalog.Managers {
		hours := manager.WorkHours
		if len(hours) == 0 {
			hours = catalog.WorkHours
		}
		windows, err := config.ParseWindows(hours)
		if err != nil {
			return nil, fmt.Errorf("manager %s work hours: %w", manager.ID, err)
		}
		local.managers = append(local.managers, managerHours{manager: manager, windows: windows})
	}
	slices.SortFunc(local.managers, func(a, b managerHours) int {
		if a.manager.Priority != b.manager.Priority {
			return cmp.Compare(a.manager.Priority, b.manager.Priority)
		}
		return cmp.Compare(a.manager.ID, b.manager.ID)
	})
	return local, nil
}

// Offer returns the set of one manager: the lead chooses a time, not a person.
// The earliest free start wins, on a tie the smaller Priority, then
// the smaller id - the managers order is already so, so an equal start
// of the next manager does not change the winner.
func (l *Local) Offer(busy Busy, now, from, until time.Time) []Slot {
	var best []Slot
	for _, manager := range l.managers {
		slots := l.managerSlots(manager, busy, now, from, until)
		if len(slots) == 0 {
			continue
		}
		if len(best) == 0 || slots[0].Start.Before(best[0].Start) {
			best = slots
		}
	}
	return best
}

// Available looks for a free manager for an exact start in order of Priority, then id.
func (l *Local) Available(busy Busy, now, start time.Time) (Slot, bool) {
	dayStart := startOfDay(start, l.location)
	for _, manager := range l.managers {
		for _, slot := range l.managerSlots(manager, busy, now, dayStart, dayStart.AddDate(0, 0, 1)) {
			if slot.Start.Equal(start) {
				return slot, true
			}
		}
	}
	return Slot{}, false
}

// managerSlots - the manager's grid of working windows minus occupancy, the first
// ShowDays days that have at least one window.
func (l *Local) managerSlots(manager managerHours, busy Busy, now, from, until time.Time) []Slot {
	limit := now.Add(Horizon)
	if !until.IsZero() && until.Before(limit) {
		limit = until
	}
	minStart := now.Add(MinLead)
	if from.After(minStart) {
		minStart = from
	}
	var slots []Slot
	days := 0
	for day := startOfDay(minStart, l.location); day.Before(limit) && days < ShowDays; day = day.AddDate(0, 0, 1) {
		window, working := manager.windows[day.Weekday()]
		if !working {
			continue
		}
		daySlots := l.daySlots(manager.manager.ID, window, day, busy[manager.manager.ID], minStart, limit)
		if len(daySlots) > 0 {
			slots = append(slots, daySlots...)
			days++
		}
	}
	return slots
}

func (l *Local) daySlots(managerID string, window config.Window, day time.Time, busy []Interval, minStart, limit time.Time) []Slot {
	windowEnd := time.Date(day.Year(), day.Month(), day.Day(), window.EndHour, window.EndMinute, 0, 0, l.location)
	// A grid from the start of the working day with a step of the meeting duration: all bookings
	// of a manager in a day share one grid, so windows do not overlap, and the uniqueness of
	// (manager, start) in the database suffices against a double entry.
	var slots []Slot
	for start := time.Date(day.Year(), day.Month(), day.Day(), window.StartHour, window.StartMinute, 0, 0, l.location); ; start = start.Add(l.duration) {
		end := start.Add(l.duration)
		if end.After(windowEnd) || !start.Before(limit) {
			break
		}
		if start.Before(minStart) || overlaps(busy, start, end) {
			continue
		}
		slots = append(slots, Slot{ManagerID: managerID, Start: start, End: end})
	}
	return slots
}

func overlaps(busy []Interval, start, end time.Time) bool {
	for _, interval := range busy {
		if start.Before(interval.End) && interval.Start.Before(end) {
			return true
		}
	}
	return false
}

func startOfDay(at time.Time, location *time.Location) time.Time {
	local := at.In(location)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
}
