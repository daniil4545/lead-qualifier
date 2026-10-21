// Package stubmodel - a scripted stand-in for the OpenRouter endpoint, used by the offline
// demo and its test. It answers the three kinds of calls the agent makes (dialog turn with
// tools, critic, niche classifier) with fixed rules over the request, so the whole pipeline
// around the model runs without a key or a network. It is not a language model: the rules
// cover the demo dialog and fall back to a neutral question.
package stubmodel

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Serve starts the stub on a free local port and returns its chat completions URL and a stop
// function.
func Serve() (url string, stop func(), err error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("listen: %w", err)
	}
	server := &http.Server{Handler: Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	return "http://" + listener.Addr().String() + "/chat/completions", func() { _ = server.Close() }, nil
}

// Handler answers one chat completions request.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var req request
		if err := json.Unmarshal(raw, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(respond(req))
	})
}

type request struct {
	Messages       []message `json:"messages"`
	ResponseFormat *struct {
		JSONSchema struct {
			Name string `json:"name"`
		} `json:"json_schema"`
	} `json:"response_format"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name"`
}

type completion struct {
	Choices []choice       `json:"choices"`
	Usage   map[string]any `json:"usage"`
}

type choice struct {
	FinishReason string       `json:"finish_reason"`
	Message      outgoingText `json:"message"`
}

type outgoingText struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func text(content string) completion {
	return completion{
		Choices: []choice{{FinishReason: "stop", Message: outgoingText{Role: "assistant", Content: content}}},
		Usage:   map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "cost": 0},
	}
}

func tool(name string, args any) completion {
	encoded, _ := json.Marshal(args)
	call := toolCall{ID: "call_" + name, Type: "function"}
	call.Function.Name = name
	call.Function.Arguments = string(encoded)
	return completion{
		Choices: []choice{{FinishReason: "tool_calls", Message: outgoingText{Role: "assistant", ToolCalls: []toolCall{call}}}},
		Usage:   map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "cost": 0},
	}
}

func respond(req request) completion {
	if req.ResponseFormat != nil {
		switch req.ResponseFormat.JSONSchema.Name {
		case "critic_verdict":
			return text(`{"ok":true,"problems":[]}`)
		case "niche_verdict":
			return text(nicheVerdict(req.Messages))
		}
	}
	return dialogTurn(req.Messages)
}

// nicheVerdict reads the service from the classifier payload and maps it to a catalog code.
func nicheVerdict(messages []message) string {
	var payload struct {
		Service     string `json:"service"`
		LeadMessage string `json:"lead_message"`
	}
	if len(messages) > 0 {
		_ = json.Unmarshal([]byte(messages[len(messages)-1].Content), &payload)
	}
	subject := strings.ToLower(payload.Service + " " + payload.LeadMessage)
	direction := "unclear"
	switch {
	case containsAny(subject, "магазин", "торгов", "розниц"):
		direction = "retail"
	case containsAny(subject, "производ", "цех", "завод"):
		direction = "manufacturing"
	}
	out, _ := json.Marshal(map[string]string{"direction": direction, "off_profile_reason": "", "quote": ""})
	return string(out)
}

const (
	servicePrefix  = "[служебное сообщение"
	preloadPrefix  = "[выдержка из базы знаний"
	fallbackReply  = "Поняла вас. Подскажите, пожалуйста, чем занимается ваша компания и в каком городе вы работаете?"
	leadTimeAnswer = "Подключение занимает от одной до двух недель: инженер проводит аудит, описывает инфраструктуру и настраивает мониторинг."
)

// turnContext is the trusted context message the agent puts before the lead's last message.
type turnContext struct {
	Today        string         `json:"today"`
	CurrentFacts map[string]any `json:"current_facts"`
	OfferedSlots []string       `json:"offered_slots"`
}

func dialogTurn(messages []message) completion {
	var ctx turnContext
	lastLead := -1
	for index, item := range messages {
		if item.Role != "user" {
			continue
		}
		switch {
		case strings.HasPrefix(item.Content, "{"):
			_ = json.Unmarshal([]byte(item.Content), &ctx)
		case strings.HasPrefix(item.Content, servicePrefix), strings.HasPrefix(item.Content, preloadPrefix):
		default:
			lastLead = index
		}
	}
	if lastLead < 0 {
		return text(fallbackReply)
	}
	lead := messages[lastLead].Content
	var last *message
	for index := len(messages) - 1; index > lastLead; index-- {
		if messages[index].Role == "tool" {
			last = &messages[index]
			break
		}
	}
	if last != nil {
		return afterTool(*last, lead)
	}
	return firstStep(lead, ctx)
}

// firstStep picks the opening tool of a turn from what the lead wrote.
func firstStep(lead string, ctx turnContext) completion {
	lower := strings.ToLower(lead)
	switch {
	case containsAny(lower, "больше не пишите", "не пишите мне"):
		return tool("stop_contact", map[string]any{})
	case containsAny(lower, "не интересно", "не актуально"):
		return tool("refuse", map[string]any{})
	case len(ctx.OfferedSlots) > 0 && hourPattern.MatchString(lower):
		if date, clock, ok := pickSlot(lower, ctx); ok {
			return tool("book_slot", map[string]string{"date": date, "hour": clock})
		}
	case revenuePattern.MatchString(lower):
		return tool("save_facts", map[string]any{"facts": extractFacts(lead)})
	case strings.Contains(lower, "?"):
		return tool("search_kb", map[string]any{"files": []string{"faq.md"}})
	}
	return text(fallbackReply)
}

// afterTool continues the turn once the code has answered the previous tool call.
func afterTool(last message, lead string) completion {
	switch last.Name {
	case "search_kb":
		if containsAny(strings.ToLower(lead), "врем", "срок", "долго", "подключ") {
			return text(leadTimeAnswer)
		}
		return text(fallbackReply)
	case "save_facts":
		if strings.HasPrefix(last.Content, "ok") {
			return tool("complete", map[string]any{})
		}
	case "complete":
		switch {
		case strings.HasPrefix(last.Content, "не хватает фактов"):
			if strings.Contains(last.Content, "city") {
				return text("Спасибо! А в каком городе вы работаете?")
			}
			return text(fallbackReply)
		case strings.HasPrefix(last.Content, "Решение:"):
			// The code owns the reply: a single dash means "nothing to add".
			return text("-")
		}
		return text("Спасибо, передала всё менеджеру.")
	}
	return text(fallbackReply)
}

// millions - the unit of "млн" in the lead's revenue phrase.
const millions = 1e6

var (
	revenuePattern = regexp.MustCompile(`(\d+(?:[.,]\d+)?)\s*млн`)
	hourPattern    = regexp.MustCompile(`\b(\d{1,2})(?::(\d{2}))?\b`)
	cityPattern    = regexp.MustCompile(`(?:^|\s)в ([А-ЯЁ][а-яё-]+)`)
	servicePattern = regexp.MustCompile(`(?i)(?:у нас|занимаемся)\s+(.+?)(?:\s+в\s+[А-ЯЁ]|[,.]|$)`)
	slotPattern    = regexp.MustCompile(`(\d{1,2}) (\p{L}+), (\d{2}):(\d{2})$`)
)

// extractFacts reads the demo phrase "a chain of shops in Berdsk, revenue about 12 million, I am
// the director and the owner". Unknown facts stay null.
func extractFacts(lead string) map[string]any {
	facts := map[string]any{"service": nil, "monthly_revenue": nil, "role": nil, "is_owner": nil, "city": nil}
	lower := strings.ToLower(lead)
	if match := revenuePattern.FindStringSubmatch(lower); match != nil {
		value, err := strconv.ParseFloat(strings.Replace(match[1], ",", ".", 1), 64)
		if err == nil {
			facts["monthly_revenue"] = strconv.FormatInt(int64(value*millions), 10)
		}
	}
	if match := servicePattern.FindStringSubmatch(lead); match != nil {
		facts["service"] = strings.TrimSpace(match[1])
	}
	if match := cityPattern.FindStringSubmatch(lead); match != nil {
		// Prepositional case to nominative for the common "-ске/-ове" endings.
		facts["city"] = strings.TrimSuffix(match[1], "е")
	}
	if containsAny(lower, "владелец", "собственник") {
		facts["is_owner"] = true
	}
	switch {
	case strings.Contains(lower, "генеральный директор"):
		facts["role"] = "генеральный директор"
	case strings.Contains(lower, "директор"):
		facts["role"] = "директор"
	case containsAny(lower, "владелец", "собственник"):
		facts["role"] = "собственник"
	}
	return facts
}

var months = map[string]time.Month{
	"января": time.January, "февраля": time.February, "марта": time.March, "апреля": time.April,
	"мая": time.May, "июня": time.June, "июля": time.July, "августа": time.August,
	"сентября": time.September, "октября": time.October, "ноября": time.November, "декабря": time.December,
}

// pickSlot chooses the offered window whose hour the lead named, else the first one, and turns
// its "weekday, 22 October, 11:00" text into book_slot arguments.
func pickSlot(lead string, ctx turnContext) (date, clock string, ok bool) {
	year := time.Now().Year()
	var today time.Month
	if len(ctx.Today) >= 7 {
		if parsed, err := time.Parse("2006-01-02", ctx.Today[:10]); err == nil {
			year, today = parsed.Year(), parsed.Month()
		}
	}
	wanted := -1
	if match := hourPattern.FindStringSubmatch(lead); match != nil {
		wanted, _ = strconv.Atoi(match[1])
	}
	var fallback string
	for _, slot := range ctx.OfferedSlots {
		match := slotPattern.FindStringSubmatch(slot)
		if match == nil {
			continue
		}
		month, known := months[match[2]]
		if !known {
			continue
		}
		slotYear := year
		if today != 0 && month < today {
			slotYear++
		}
		day, _ := strconv.Atoi(match[1])
		found := fmt.Sprintf("%04d-%02d-%02d %s:%s", slotYear, int(month), day, match[3], match[4])
		if fallback == "" {
			fallback = found
		}
		if hour, _ := strconv.Atoi(match[3]); hour == wanted {
			fallback = found
			break
		}
	}
	if fallback == "" {
		return "", "", false
	}
	parts := strings.SplitN(fallback, " ", 2)
	return parts[0], parts[1], true
}

func containsAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
