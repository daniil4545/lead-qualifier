package stubmodel

import "testing"

func TestExtractFacts(t *testing.T) {
	facts := extractFacts("У нас сеть магазинов стройматериалов в Бердске, выручка около 12 млн в месяц. Я директор и владелец.")
	want := map[string]any{
		"service": "сеть магазинов стройматериалов", "monthly_revenue": "12000000",
		"role": "директор", "is_owner": true, "city": "Бердск",
	}
	for key, value := range want {
		if facts[key] != value {
			t.Errorf("%s = %v, want %v", key, facts[key], value)
		}
	}
}

func TestPickSlot(t *testing.T) {
	ctx := turnContext{
		Today:        "2026-12-30, среда, 10:00",
		OfferedSlots: []string{"четверг, 31 декабря, 10:00", "пятница, 8 января, 11:00"},
	}
	for _, tc := range []struct {
		lead, date, clock string
	}{
		{"давайте в 11", "2027-01-08", "11:00"},
		{"давайте в 15", "2026-12-31", "10:00"},
	} {
		date, clock, ok := pickSlot(tc.lead, ctx)
		if !ok || date != tc.date || clock != tc.clock {
			t.Errorf("pickSlot(%q) = %q %q %v, want %q %q", tc.lead, date, clock, ok, tc.date, tc.clock)
		}
	}
	if _, _, ok := pickSlot("в 11", turnContext{}); ok {
		t.Error("no offered slots: want not ok")
	}
}

func TestRespondByRequestKind(t *testing.T) {
	critic := request{ResponseFormat: &struct {
		JSONSchema struct {
			Name string `json:"name"`
		} `json:"json_schema"`
	}{}}
	critic.ResponseFormat.JSONSchema.Name = "critic_verdict"
	if got := respond(critic).Choices[0].Message.Content; got != `{"ok":true,"problems":[]}` {
		t.Errorf("critic verdict = %q", got)
	}
	refusal := respond(request{Messages: []message{{Role: "user", Content: "Спасибо, нам это не интересно."}}})
	if calls := refusal.Choices[0].Message.ToolCalls; len(calls) != 1 || calls[0].Function.Name != "refuse" {
		t.Errorf("refusal tool calls = %+v", calls)
	}
}
