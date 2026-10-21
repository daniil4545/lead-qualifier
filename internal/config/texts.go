package config

import (
	"fmt"
	"reflect"
	"strings"
)

// Texts - the ready texts written by the code, not the model. Substitutions in
// curly braces are filled by dialog: {name}, {threshold}, {time}, {manager},
// {duration}.
type Texts struct {
	// Greeting - the first message on a request: a greeting and questions.
	Greeting string `json:"greeting"`
	// Handoff - the reply on handoff to a human (manual_review).
	Handoff string `json:"handoff"`
	// Reschedule - the reply to a request to reschedule an appointed meeting.
	Reschedule string `json:"reschedule"`
	// OffProfile - a refusal by niche.
	OffProfile string `json:"off_profile"`
	// TooEarly - a refusal for revenue below the threshold, {threshold}.
	TooEarly string `json:"too_early"`
	// Stopped - the farewell after a request not to write anymore.
	Stopped string `json:"stopped"`
	// Refused - the farewell after the lead's refusal.
	Refused string `json:"refused"`
	// SlotsHeadline - the line before the window list when there is no model reply.
	SlotsHeadline string `json:"slots_headline"`
	// SlotsFooter - the line after the window list: the time zone and a call to choose an hour.
	SlotsFooter string `json:"slots_footer"`
	// SlotsBeyondRequest - there are no windows in the lead's named period, the nearest is shown.
	SlotsBeyondRequest string `json:"slots_beyond_request"`
	// SlotUnavailable - the named hour is busy, {time}.
	SlotUnavailable string `json:"slot_unavailable"`
	// SlotTaken - the hour was taken during the turn.
	SlotTaken string `json:"slot_taken"`
	// MeetingConfirmed - booking confirmation, {time}, {manager}, {duration}.
	MeetingConfirmed string `json:"meeting_confirmed"`
}

// validate requires all texts: an empty ready text would leave the lead without an answer.
func (t Texts) validate() error {
	value := reflect.ValueOf(t)
	for index := range value.NumField() {
		if strings.TrimSpace(value.Field(index).String()) == "" {
			return fmt.Errorf("texts.json: %s is required", value.Type().Field(index).Tag.Get("json"))
		}
	}
	return nil
}
