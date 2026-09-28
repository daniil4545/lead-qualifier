// Package calendar строит свободные окна менеджеров: сетка рабочих часов с
// шагом в длительность встречи минус занятость. Бронь здесь не живёт -
// двойную запись держит уникальность в базе.
package calendar

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/daniil4545/lead-qualifier/internal/config"
)

const (
	// Horizon - дальше этого срока от текущего момента бот не записывает.
	Horizon = 14 * 24 * time.Hour
	// MinLead - минимальный зазор от текущего момента до встречи.
	MinLead = 2 * time.Hour
	// ShowDays - сколько календарных дней с окнами показывается лиду за раз.
	ShowDays = 3
)

// Interval - занятый отрезок менеджера.
type Interval struct {
	Start time.Time
	End   time.Time
}

// Busy - занятость по id менеджера.
type Busy map[string][]Interval

// Slot - свободное окно конкретного менеджера.
type Slot struct {
	ManagerID string
	Start     time.Time
	End       time.Time
}

// Calendar - откуда берутся окна. Локальная реализация считает их по рабочим
// часам каталога; внешний календарь добавил бы свою занятость.
type Calendar interface {
	// Offer - окна менеджера с самым ранним свободным стартом в [from, until),
	// первые ShowDays дней. Нулевые границы - от now+MinLead до now+Horizon.
	Offer(busy Busy, now, from, until time.Time) []Slot
	// Available - свободный менеджер на точный старт.
	Available(busy Busy, now, start time.Time) (Slot, bool)
}

type managerHours struct {
	manager config.Manager
	windows map[time.Weekday]config.Window
}

// Local - календарь по рабочим часам каталога.
type Local struct {
	location *time.Location
	duration time.Duration
	managers []managerHours // по Priority, затем по id
}

// NewLocal собирает календарь каталога. Менеджер без своих часов работает по
// общим часам каталога.
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

// Offer отдаёт набор одного менеджера: лид выбирает время, а не человека.
// Побеждает самый ранний свободный старт, при равенстве меньший Priority,
// затем меньший id - порядок managers уже такой, поэтому равный старт
// следующего менеджера победителя не сменит.
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

// Available ищет свободного на точный старт в порядке Priority, затем id.
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

// managerSlots - сетка рабочих окон менеджера минус занятость, первые
// ShowDays дней, где есть хотя бы одно окно.
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
	// Сетка от начала рабочего дня с шагом в длительность встречи: у всех броней
	// менеджера за день одна сетка, поэтому окна не пересекаются, и уникальности
	// (менеджер, начало) в базе хватает против двойной записи.
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
