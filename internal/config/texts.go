package config

import (
	"fmt"
	"reflect"
	"strings"
)

// Texts - готовые тексты, которые пишет код, а не модель. Подстановки в
// фигурных скобках заполняет dialog: {name}, {threshold}, {time}, {manager},
// {duration}.
type Texts struct {
	// Greeting - первое сообщение по заявке: приветствие и вопросы.
	Greeting string `json:"greeting"`
	// Handoff - ответ при передаче человеку (manual_review).
	Handoff string `json:"handoff"`
	// Reschedule - ответ на просьбу перенести назначенную встречу.
	Reschedule string `json:"reschedule"`
	// OffProfile - отказ по нише.
	OffProfile string `json:"off_profile"`
	// TooEarly - отказ по выручке ниже порога, {threshold}.
	TooEarly string `json:"too_early"`
	// Stopped - прощание после просьбы больше не писать.
	Stopped string `json:"stopped"`
	// Refused - прощание после отказа лида.
	Refused string `json:"refused"`
	// SlotsHeadline - строка перед списком окон, когда реплики модели нет.
	SlotsHeadline string `json:"slots_headline"`
	// SlotsFooter - строка после списка окон: пояс и призыв выбрать час.
	SlotsFooter string `json:"slots_footer"`
	// SlotsBeyondRequest - в названном лидом сроке окон нет, показано ближайшее.
	SlotsBeyondRequest string `json:"slots_beyond_request"`
	// SlotUnavailable - названный час занят, {time}.
	SlotUnavailable string `json:"slot_unavailable"`
	// SlotTaken - час заняли, пока шёл ход.
	SlotTaken string `json:"slot_taken"`
	// MeetingConfirmed - подтверждение брони, {time}, {manager}, {duration}.
	MeetingConfirmed string `json:"meeting_confirmed"`
}

// validate требует все тексты: пустой готовый текст оставил бы лида без ответа.
func (t Texts) validate() error {
	value := reflect.ValueOf(t)
	for index := range value.NumField() {
		if strings.TrimSpace(value.Field(index).String()) == "" {
			return fmt.Errorf("texts.json: %s is required", value.Type().Field(index).Tag.Get("json"))
		}
	}
	return nil
}
