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

// CriticVerdict - вердикт критика по черновику. Цитаты проблем идут только в
// аудит решения, не в лог.
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

// criticPayload - вход критика. TariffPrice и MeetingTimes посчитал код, их
// повторение в черновике легитимно.
type criticPayload struct {
	Candidate       string   `json:"candidate"`
	KBFragments     []string `json:"kb_fragments"`
	LastUserMessage string   `json:"last_user_message"`
	TariffPrice     string   `json:"tariff_price,omitempty"`
	MeetingTimes    []string `json:"meeting_times,omitempty"`
}

// criticCheck - один strict-JSON вызов критика. Транспортный сбой уходит
// наверх ExternalFailure (ход переигрывается), непонятный ответ - обычной
// ошибкой (critic_error).
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
		return CriticVerdict{}, fmt.Errorf("decode critic verdict: %w", err)
	}
	verdict := CriticVerdict{}
	for _, problem := range wire.Problems {
		category := problem.Category
		if !slices.Contains(criticCategories, category) {
			category = "other"
		}
		verdict.Problems = append(verdict.Problems, CriticProblem{Category: category, Sentence: problem.Sentence})
	}
	// OK выводится из пустоты списка: противоречивый ответ модели не пропускает
	// черновик с проблемами.
	verdict.OK = len(verdict.Problems) == 0
	return verdict, nil
}

// NicheInput - вход классификатора: услуга лида и его последние реплики.
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

// ClassifyNiche раскладывает услугу лида в коды каталога. Отдельный вызов, а
// не поле save_facts: модель диалога правит факты каждым ходом и переписала
// бы код под контекст беседы. Ответ - недоверенный ввод, коды сверяет Rules.
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

// NicheClassifier - кто раскладывает услугу лида в коды каталога.
type NicheClassifier interface {
	ClassifyNiche(ctx context.Context, input NicheInput) (qualify.NicheVerdict, error)
}

// EnsureNiche - шаг «узнать профиль лида»: вердикт ложится в факты хода и
// считается один раз на значение услуги. Повторяемый сбой уходит наверх и
// переигрывает ход; постоянный и непонятный ответ дают неподтверждённый
// профиль - отказ по сбою классификатора запрещён.
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

// trimJSONFence снимает markdown-обёртку: strict-схема её запрещает, но модели
// судьи изредка отвечают блоком ```json.
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
