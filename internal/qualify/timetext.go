package qualify

import (
	"fmt"
	"strings"
	"time"
)

var weekdays = [...]string{
	time.Sunday:    "воскресенье",
	time.Monday:    "понедельник",
	time.Tuesday:   "вторник",
	time.Wednesday: "среда",
	time.Thursday:  "четверг",
	time.Friday:    "пятница",
	time.Saturday:  "суббота",
}

var monthsGenitive = [...]string{
	time.January:   "января",
	time.February:  "февраля",
	time.March:     "марта",
	time.April:     "апреля",
	time.May:       "мая",
	time.June:      "июня",
	time.July:      "июля",
	time.August:    "августа",
	time.September: "сентября",
	time.October:   "октября",
	time.November:  "ноября",
	time.December:  "декабря",
}

// SlotText - a window time for the lead and for the model context: "Thursday, July 30,
// 15:00" (the format is Russian). One function for all places so the format does not drift.
func SlotText(start time.Time, location *time.Location) string {
	local := start.In(location)
	return fmt.Sprintf("%s, %d %s, %02d:%02d", weekdays[local.Weekday()], local.Day(),
		monthsGenitive[local.Month()], local.Hour(), local.Minute())
}

// DateTimeText - a time inside a phrase: "30 July at 15:00" (Russian format).
func DateTimeText(start time.Time, location *time.Location) string {
	local := start.In(location)
	return fmt.Sprintf("%d %s в %02d:%02d", local.Day(), monthsGenitive[local.Month()], local.Hour(), local.Minute())
}

// DayHeader - a day header in the window list: "Thursday, July 30".
func DayHeader(day time.Time, location *time.Location) string {
	local := day.In(location)
	return fmt.Sprintf("%s, %d %s", weekdays[local.Weekday()], local.Day(), monthsGenitive[local.Month()])
}

// DayFormat - a calendar day as a string: tool arguments and logs.
const DayFormat = "2006-01-02"

// TodayText - the turn moment for the model: date, weekday and hour. Without the hour the model
// does not understand "today at 12" when noon has already passed.
func TodayText(now time.Time, location *time.Location) string {
	local := now.In(location)
	return local.Format(DayFormat) + ", " + weekdays[local.Weekday()] + ", " + local.Format("15:04")
}

// StartOfDay - the start of the local day.
func StartOfDay(at time.Time, location *time.Location) time.Time {
	local := at.In(location)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
}

// Rubles - an amount for people: "12 345 ₽".
func Rubles(amount int64) string {
	digits := fmt.Sprint(amount)
	var text strings.Builder
	for index := range len(digits) {
		if index > 0 && (len(digits)-index)%3 == 0 {
			text.WriteByte(' ')
		}
		text.WriteByte(digits[index])
	}
	return text.String() + " ₽"
}
