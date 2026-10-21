package dialog

import (
	"strings"

	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

// render substitutes values into a catalog text: pairs of "{key}", value.
func render(template string, pairs ...string) string {
	return strings.NewReplacer(pairs...).Replace(template)
}

// withReply puts the model's reply before the ready code text.
func withReply(reply, text string) string {
	if strings.TrimSpace(reply) == "" {
		return text
	}
	return reply + "\n\n" + text
}

func joinLines(first, second string) string {
	if first == "" {
		return second
	}
	return first + "\n" + second
}

// leadNote - a note on the deal: what happened and what is known about the lead.
func (w *Worker) leadNote(input qualify.DecisionInput, event string) string {
	return "Квалификатор: " + event + ". " + w.factsSummary(input.Case.Facts)
}

// factsSummary - the collected facts for the manager. Niche codes are replaced with catalog
// names: Latin codes in a manager card say nothing.
func (w *Worker) factsSummary(facts qualify.Facts) string {
	value := func(key string) string {
		if text := facts.Value(key); text != "" {
			return text
		}
		return "не указано"
	}
	owner := "не указано"
	if isOwner, ok := facts[qualify.FactIsOwner].(bool); ok {
		owner = map[bool]string{true: "да", false: "нет"}[isOwner]
	}
	segment, reason := w.rules.Segment(facts)
	niche := w.rules.SegmentName(segment)
	if reason != "" {
		niche = "вне профиля: " + w.rules.ReasonName(reason)
	}
	return "Услуга: " + value(qualify.FactService) + "; выручка: " + value(qualify.FactRevenue) +
		"; роль: " + value(qualify.FactRole) + "; собственник: " + owner +
		"; город: " + value(qualify.FactCity) + "; направление: " + niche + "."
}
