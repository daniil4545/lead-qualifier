package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

// CriticVerdict - the critic's verdict on a draft. Problem quotes go only into the
// decision audit, not the log.
type CriticVerdict struct {
	OK       bool
	Problems []CriticProblem
}

type CriticProblem struct {
	Category string
	Sentence string
}

var criticCategories = []string{"invented_fact", "price_or_terms", "promise_or_guarantee",
	"instruction_leak", "manipulation", "tone", "contact", "unanswered_question"}

var criticSchema = json.RawMessage(`{
	"type": "object",
	"additionalProperties": false,
	"properties": {
		"ok": {"type": "boolean"},
		"problems": {
			"type": "array",
			"items": {
				"type": "object",
				"additionalProperties": false,
				"properties": {
					"category": {"type": "string", "enum": ["` + strings.Join(criticCategories, `", "`) + `"]},
					"sentence": {"type": "string"}
				},
				"required": ["category", "sentence"]
			}
		}
	},
	"required": ["ok", "problems"]
}`)

// criticPayload - the critic's input. TariffPrice and MeetingTimes were computed by the code, so
// repeating them in the draft is legitimate.
type criticPayload struct {
	Candidate       string   `json:"candidate"`
	KBFragments     []string `json:"kb_fragments"`
	LastUserMessage string   `json:"last_user_message"`
	TariffPrice     string   `json:"tariff_price,omitempty"`
	MeetingTimes    []string `json:"meeting_times,omitempty"`
}

// maxRawCriticRunes - how much of a broken critic answer the audit keeps.
const maxRawCriticRunes = 2000

// CriticFormatError - the critic answered with something that is not the verdict JSON. The raw
// answer goes into the decision audit (never the log): without it a critic_error can be
// explained only by guessing what the model wrote.
type CriticFormatError struct {
	Raw string
	Err error
}

func (e *CriticFormatError) Error() string { return "decode critic verdict: " + e.Err.Error() }
func (e *CriticFormatError) Unwrap() error { return e.Err }

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// criticCheck - one strict-JSON critic call. A transport failure goes up as
// ExternalFailure (the turn is replayed), an unintelligible answer as an ordinary
// error (critic_error).
func (c *Client) criticCheck(ctx context.Context, caseID int64, payload criticPayload) (CriticVerdict, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return CriticVerdict{}, fmt.Errorf("encode critic payload: %w", err)
	}
	req := c.newRequest(c.cfg.CriticModel, []message{
		{Role: "system", Content: c.prompts.Critic},
		{Role: "user", Content: string(body)},
	}, sessionID(caseID))
	req.ResponseFormat = &responseFormat{Type: "json_schema",
		JSONSchema: jsonSchema{Name: "critic_verdict", Strict: true, Schema: criticSchema}}
	resp, err := c.call(ctx, req)
	if err != nil {
		return CriticVerdict{}, err
	}
	var wire struct {
		OK       bool `json:"ok"`
		Problems []struct {
			Category string `json:"category"`
			Sentence string `json:"sentence"`
		} `json:"problems"`
	}
	if err := json.Unmarshal([]byte(trimJSONFence(resp.Choices[0].Message.Content)), &wire); err != nil {
		return CriticVerdict{}, &CriticFormatError{Raw: truncateRunes(resp.Choices[0].Message.Content, maxRawCriticRunes), Err: err}
	}
	verdict := CriticVerdict{}
	for _, problem := range wire.Problems {
		category := problem.Category
		if !slices.Contains(criticCategories, category) {
			category = "other"
		}
		verdict.Problems = append(verdict.Problems, CriticProblem{Category: category, Sentence: problem.Sentence})
	}
	// OK is derived from the emptiness of the list: a contradictory model answer does not let
	// a draft with problems through.
	verdict.OK = len(verdict.Problems) == 0
	return verdict, nil
}

// NicheInput - the classifier input: the lead's service and their latest messages.
type NicheInput struct {
	CaseID      int64
	Service     string
	LeadMessage string
}

var nicheSchema = json.RawMessage(`{
	"type": "object",
	"additionalProperties": false,
	"properties": {
		"direction": {"type": "string"},
		"off_profile_reason": {"type": "string"},
		"quote": {"type": "string"}
	},
	"required": ["direction", "off_profile_reason", "quote"]
}`)

type nicheCode struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// ClassifyNiche maps the lead's service to catalog codes. A separate call rather than
// a save_facts field: the dialog model edits facts every turn and would have rewritten
// the code to fit the conversation. The answer is untrusted input, Rules checks the codes.
func (c *Client) ClassifyNiche(ctx context.Context, input NicheInput) (qualify.NicheVerdict, error) {
	payload := struct {
		Service     string      `json:"service"`
		LeadMessage string      `json:"lead_message"`
		Directions  []nicheCode `json:"directions"`
		Reasons     []nicheCode `json:"off_profile_reasons"`
	}{Service: input.Service, LeadMessage: input.LeadMessage}
	for _, segment := range c.rules.Segments() {
		payload.Directions = append(payload.Directions, nicheCode{Code: segment.Code, Name: segment.Name})
	}
	for _, reason := range c.rules.Reasons() {
		payload.Reasons = append(payload.Reasons, nicheCode{Code: reason.Code, Name: reason.Name})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return qualify.NicheVerdict{}, fmt.Errorf("encode niche payload: %w", err)
	}
	req := c.newRequest(c.cfg.CriticModel, []message{
		{Role: "system", Content: c.prompts.Niche},
		{Role: "user", Content: string(body)},
	}, sessionID(input.CaseID))
	req.ResponseFormat = &responseFormat{Type: "json_schema",
		JSONSchema: jsonSchema{Name: "niche_verdict", Strict: true, Schema: nicheSchema}}
	resp, err := c.call(ctx, req)
	if err != nil {
		return qualify.NicheVerdict{}, err
	}
	var wire struct {
		Direction        string `json:"direction"`
		OffProfileReason string `json:"off_profile_reason"`
	}
	if err := json.Unmarshal([]byte(trimJSONFence(resp.Choices[0].Message.Content)), &wire); err != nil {
		return qualify.NicheVerdict{}, fmt.Errorf("decode niche verdict: %w", err)
	}
	return qualify.NicheVerdict{
		Direction:        strings.TrimSpace(wire.Direction),
		OffProfileReason: strings.TrimSpace(wire.OffProfileReason),
	}, nil
}

// NicheClassifier - whatever maps the lead's service to catalog codes.
type NicheClassifier interface {
	ClassifyNiche(ctx context.Context, input NicheInput) (qualify.NicheVerdict, error)
}

// EnsureNiche - the "learn the lead's profile" step: the verdict goes into the turn facts and
// is computed once per service value. A repeatable failure goes up and
// replays the turn; a permanent or unintelligible answer yields an unconfirmed
// profile: a refusal because of a classifier failure is forbidden.
func EnsureNiche(ctx context.Context, classifier NicheClassifier, rules qualify.Rules,
	caseID int64, facts qualify.Facts, leadMessage string) error {
	if qualify.NicheKnown(facts) {
		return nil
	}
	verdict, err := classifier.ClassifyNiche(ctx, NicheInput{
		CaseID: caseID, Service: facts.Value(qualify.FactService), LeadMessage: leadMessage,
	})
	if err != nil {
		var failure *ExternalFailure
		if errors.As(err, &failure) && !failure.Permanent {
			return err
		}
		slog.Warn("niche classify failed", "case_id", caseID, "error", err)
		verdict = qualify.NicheVerdict{}
	}
	rules.ApplyVerdict(facts, verdict)
	direction, reason := rules.Segment(facts)
	slog.Info("niche classified", "case_id", caseID, "direction", direction, "reason", reason)
	return nil
}

// trimJSONFence strips a markdown wrapper: the strict schema forbids it, but judge
// models occasionally answer with a ```json block.
func trimJSONFence(content string) string {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "```") {
		return trimmed
	}
	trimmed = strings.TrimPrefix(trimmed, "```")
	if index := strings.IndexByte(trimmed, '\n'); index >= 0 && !strings.Contains(trimmed[:index], "{") {
		trimmed = trimmed[index+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(trimmed), "```"))
}
