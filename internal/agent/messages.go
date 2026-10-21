package agent

import (
	"encoding/json"
	"fmt"

	"github.com/daniil4545/lead-qualifier/internal/knowledge"
	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

// trustedContext - what the code knows about the case, as one user message before
// the lead's last message. The code prints time and price, the model only repeats them.
type trustedContext struct {
	Today string `json:"today"`
	// LeadName is always present, even as an empty string: without the key the model invents a name.
	LeadName     string         `json:"lead_name"`
	CaseStatus   qualify.Status `json:"case_status"`
	CurrentFacts qualify.Facts  `json:"current_facts"`
	MissingFacts []string       `json:"missing_facts"`
	Tariff       string         `json:"tariff,omitempty"`
	TariffPrice  string         `json:"tariff_price,omitempty"`
	Meeting      string         `json:"meeting,omitempty"`
	Manager      string         `json:"manager,omitempty"`
	OfferedSlots []string       `json:"offered_slots,omitempty"`
}

// buildMessages assembles the turn prompt: system, a knowledge base section issued
// in advance, history and trusted context before the last incoming message. System and the excerpt
// are the same for all leads, so the prefix is cached by the provider.
func (c *Client) buildMessages(input qualify.DecisionInput) ([]message, error) {
	facts := input.Case.Facts.Known()
	for _, hidden := range []string{"niche", "niche_off", "niche_of"} {
		delete(facts, hidden)
	}
	qualify.DeriveOwnerRole(facts)
	missing := facts.Missing()
	if missing == nil {
		missing = []string{}
	}
	trusted := trustedContext{
		Today:        qualify.TodayText(input.Now, c.location),
		LeadName:     input.Lead.Name,
		CaseStatus:   input.Case.Status,
		CurrentFacts: facts,
		MissingFacts: missing,
	}
	if tariff, ok := c.rules.TariffFor(facts); ok {
		trusted.Tariff = tariff.Name
		trusted.TariffPrice = qualify.Rubles(tariff.Price)
	}
	if input.Meeting != nil {
		trusted.Meeting = qualify.SlotText(input.Meeting.Start, c.location)
		trusted.Manager = input.ManagerName
	}
	for _, slot := range input.OfferedSlots {
		trusted.OfferedSlots = append(trusted.OfferedSlots, qualify.SlotText(slot.Start, c.location))
	}
	contextJSON, err := json.Marshal(trusted)
	if err != nil {
		return nil, fmt.Errorf("encode trusted context: %w", err)
	}

	messages := []message{{Role: "system", Content: c.prompts.System}}
	if content, ok := c.knowledge.Files[knowledge.PreloadFile]; ok {
		messages = append(messages, message{Role: "user",
			Content: preloadPrefix + "\n# " + knowledge.PreloadFile + "\n" + content})
	}
	lastIncoming := -1
	for index, item := range input.Messages {
		if item.Direction == qualify.DirectionIn {
			lastIncoming = index
		}
	}
	for index, item := range input.Messages {
		if index == lastIncoming {
			messages = append(messages, message{Role: "user", Content: string(contextJSON)})
		}
		role := "assistant"
		if item.Direction == qualify.DirectionIn {
			role = "user"
		}
		messages = append(messages, message{Role: role, Content: item.Text})
	}
	if lastIncoming == -1 {
		messages = append(messages, message{Role: "user", Content: string(contextJSON)})
	}
	return messages, nil
}
