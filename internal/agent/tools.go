package agent

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

// tools - the static tool contract. The code decides whether a call is allowed
// with a tool answer, not by rebuilding the schema: the model gets an explanation and
// continues the turn.
func tools() []tool {
	return []tool{
		{Type: "function", Function: function{
			Name:        "search_kb",
			Description: "Прочитать разделы базы знаний по именам файлов из оглавления. От 1 до 3 файлов за раз.",
			Strict:      true,
			Parameters: json.RawMessage(`{"type": "object", "additionalProperties": false,
				"properties": {"files": {"type": "array", "items": {"type": "string"}, "minItems": 1, "maxItems": 3}},
				"required": ["files"]}`),
		}},
		{Type: "function", Function: function{
			Name:        "save_facts",
			Description: "Сохранить факты, явно названные лидом. Неизвестные факты передавай как null.",
			Strict:      true,
			Parameters: json.RawMessage(`{"type": "object", "additionalProperties": false,
				"properties": {"facts": {"type": "object", "additionalProperties": false,
					"properties": {
						"service": {"type": ["string", "null"], "description": "Чем занимается компания лида."},
						"monthly_revenue": {"type": ["string", "null"], "description": "Среднемесячная выручка компании в полных рублях, только цифрами: «около 4 млн» -> «4000000». Диапазон - «мин-макс»."},
						"role": {"type": ["string", "null"], "description": "Роль человека в компании."},
						"is_owner": {"type": ["boolean", "null"], "description": "Является ли человек собственником."},
						"city": {"type": ["string", "null"], "description": "Город компании."}
					},
					"required": ["service", "monthly_revenue", "role", "is_owner", "city"]}},
				"required": ["facts"]}`),
		}},
		{Type: "function", Function: function{
			Name:        "escalate",
			Description: "Передать диалог человеку: просьба лида, перенос назначенной встречи, агрессия или иная причина.",
			Strict:      true,
			Parameters: json.RawMessage(`{"type": "object", "additionalProperties": false,
				"properties": {"reason": {"type": "string", "enum": ["user_request", "reschedule", "aggression", "other"]}},
				"required": ["reason"]}`),
		}},
		{Type: "function", Function: function{
			Name:        "complete",
			Description: "Завершить сбор: все пять фактов собраны. Решение о встрече примет код.",
			Strict:      true,
			Parameters:  json.RawMessage(`{"type": "object", "additionalProperties": false, "properties": {}, "required": []}`),
		}},
		{Type: "function", Function: function{
			Name:        "offer_slots",
			Description: "Предложить лиду свободные времена встречи. Времена подбирает и присылает код, не сочиняй их сам.",
			Strict:      true,
			Parameters: json.RawMessage(`{"type": "object", "additionalProperties": false,
				"properties": {
					"not_earlier_than": {"type": "string", "description": "Дата YYYY-MM-DD, раньше которой лид встречаться не готов; пустая строка, если срока нет."},
					"not_later_than": {"type": "string", "description": "Дата YYYY-MM-DD, позже которой лид встречаться не готов; пустая строка, если срока нет."}
				},
				"required": ["not_earlier_than", "not_later_than"]}`),
		}},
		{Type: "function", Function: function{
			Name:        "book_slot",
			Description: "Записать лида на конкретное время, которое он назвал. Свободно ли оно, проверит код.",
			Strict:      true,
			Parameters: json.RawMessage(`{"type": "object", "additionalProperties": false,
				"properties": {
					"date": {"type": "string", "description": "Дата встречи YYYY-MM-DD, считай её от today из контекста."},
					"hour": {"type": "string", "description": "Время начала встречи HH:MM: «в 15» -> «15:00», «в половине десятого» -> «09:30»."}
				},
				"required": ["date", "hour"]}`),
		}},
		{Type: "function", Function: function{
			Name:        "stop_contact",
			Description: "Лид прямо просит больше ему не писать. Диалог закрывается, встреча снимается кодом.",
			Strict:      true,
			Parameters:  json.RawMessage(`{"type": "object", "additionalProperties": false, "properties": {}, "required": []}`),
		}},
		{Type: "function", Function: function{
			Name:        "refuse",
			Description: "Лид прямо отказался от встречи и услуг: «не интересно», «не актуально». Прощание и снятие встречи делает код.",
			Strict:      true,
			Parameters:  json.RawMessage(`{"type": "object", "additionalProperties": false, "properties": {}, "required": []}`),
		}},
	}
}

// Tool answers on branches where the code made the decision and will append its own
// text. The model has one job left: answer the lead's question, if there was one.
const (
	replyRoleLine = `Твоя реплика: спросил ли лид о чём-нибудь - ответь на его вопрос по базе знаний, ` +
		`по существу, одним-тремя предложениями. Вопроса не было - ответь одним символом «-». ` +
		`Решение не пересказывай, своего вопроса не задавай.`
	slotsReplyRequest = `Решение: встречу предлагаем. Список свободных времён подберу и допечатаю сам после твоего ответа. ` +
		replyRoleLine + ` Ни дат, ни часов, ни дней недели не называй.`
	replyOnlyRequest = `Факты уже собраны, решение принято раньше - ничего прикладывать к твоему ответу я не буду. ` +
		`Ответь лиду текстом на то, что он написал. Конкретных дат и часов не называй.`
)

func decidedToolResult(decision string) string {
	return "Решение: " + decision + ". За твоей репликой лид получит готовый текст от кода.\n" + replyRoleLine
}

// dashOnlyReply - a reply of a single hyphen: the model declined to write text where
// the decision carries a ready code text.
func dashOnlyReply(text string) bool {
	trimmed := strings.TrimSpace(strings.Trim(strings.TrimSpace(text), "«»\"'"))
	return trimmed == "-" || trimmed == "–" || trimmed == "—"
}

// clockPattern - the book_slot time: strictly HH:MM or a bare hour.
var clockPattern = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?$`)

// parseRequestedAt assembles the time the lead named from the book_slot arguments in
// the catalog time zone: HH:MM or a bare hour. Untrusted input: anything unrecognized
// is not a booking but an answer to the model.
func parseRequestedAt(date, clock string, location *time.Location) (time.Time, bool) {
	day, err := time.ParseInLocation(qualify.DayFormat, strings.TrimSpace(date), location)
	if err != nil {
		return time.Time{}, false
	}
	hour, minute, ok := ParseClock(clock)
	if !ok {
		return time.Time{}, false
	}
	return time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, location), true
}

// ParseClock parses the book_slot time: strictly HH:MM or a bare hour. One
// function for the agent and the golden evaluation so the contract does not drift.
func ParseClock(clock string) (hour, minute int, ok bool) {
	match := clockPattern.FindStringSubmatch(strings.TrimSpace(clock))
	if match == nil {
		return 0, 0, false
	}
	// The pattern guarantees digits, so a parse error cannot happen.
	hour, _ = strconv.Atoi(match[1])
	if match[2] != "" {
		minute, _ = strconv.Atoi(match[2])
	}
	if hour > 23 || minute > 59 {
		return 0, 0, false
	}
	return hour, minute, true
}

// parseSlotRange reads the lead's time window from the offer_slots arguments: start of day
// of each bound. Garbage yields a zero bound and the selection starts from the nearest windows.
func parseSlotRange(args string, location *time.Location) (from, until time.Time) {
	var wire struct {
		NotEarlierThan string `json:"not_earlier_than"`
		NotLaterThan   string `json:"not_later_than"`
	}
	if err := json.Unmarshal([]byte(args), &wire); err != nil {
		return time.Time{}, time.Time{}
	}
	parse := func(value string) time.Time {
		day, err := time.ParseInLocation(qualify.DayFormat, strings.TrimSpace(value), location)
		if err != nil {
			return time.Time{}
		}
		return day
	}
	return parse(wire.NotEarlierThan), parse(wire.NotLaterThan)
}
