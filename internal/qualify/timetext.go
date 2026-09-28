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

// SlotText - время окна для лида и для контекста модели: «четверг, 30 июля,
// 15:00». Одна функция на все места, чтобы формат не разъехался.
func SlotText(start time.Time, location *time.Location) string {
	local := start.In(location)
	return fmt.Sprintf("%s, %d %s, %02d:%02d", weekdays[local.Weekday()], local.Day(),
		monthsGenitive[local.Month()], local.Hour(), local.Minute())
}

// DateTimeText - время внутри фразы: «30 июля в 15:00».
func DateTimeText(start time.Time, location *time.Location) string {
	local := start.In(location)
	return fmt.Sprintf("%d %s в %02d:%02d", local.Day(), monthsGenitive[local.Month()], local.Hour(), local.Minute())
}

// DayHeader - заголовок дня в списке окон: «четверг, 30 июля».
func DayHeader(day time.Time, location *time.Location) string {
	local := day.In(location)
	return fmt.Sprintf("%s, %d %s", weekdays[local.Weekday()], local.Day(), monthsGenitive[local.Month()])
}

// DayFormat - календарный день строкой: аргументы инструментов и логи.
const DayFormat = "2006-01-02"

// TodayText - момент хода для модели: дата, день недели и час. Без часа модель
// не поймёт «сегодня в 12», когда полдень уже прошёл.
func TodayText(now time.Time, location *time.Location) string {
	local := now.In(location)
	return local.Format(DayFormat) + ", " + weekdays[local.Weekday()] + ", " + local.Format("15:04")
}

// StartOfDay - начало местных суток.
func StartOfDay(at time.Time, location *time.Location) time.Time {
	local := at.In(location)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
}

// Rubles - сумма для людей: «12 345 ₽».
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
