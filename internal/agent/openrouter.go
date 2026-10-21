// Package agent - a model turn: a loop with native tool calling through OpenRouter,
// guard and critic over the text. No side effects: RunTurn returns
// an untrusted Turn, and deterministic code in dialog applies it.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/daniil4545/lead-qualifier/internal/config"
	"github.com/daniil4545/lead-qualifier/internal/knowledge"
	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

// ExternalFailure - a failed model call. Permanent means a retry will not help.
type ExternalFailure struct {
	Kind      string
	Message   string
	Permanent bool
	Status    int
}

func (f *ExternalFailure) Error() string {
	if f.Message == "" {
		return f.Kind
	}
	return f.Kind + ": " + f.Message
}

// ProviderSide reports whether the failure is on the side of the model gateway or the network to it
// (an outage, a rate limit, an empty balance) and not in what the model wrote.
func (f *ExternalFailure) ProviderSide() bool { return strings.HasPrefix(f.Kind, "openrouter_") }

// Client - the agent turn on top of OpenRouter for one catalog.
type Client struct {
	cfg       config.OpenRouter
	http      *http.Client
	prompts   Prompts
	knowledge knowledge.Set
	guard     Guard
	rules     qualify.Rules
	location  *time.Location
	// spent - the cost in USD of every response so far, an empty or failed one included: it was paid.
	spentMu sync.Mutex
	spent   float64
	// meetingAlive - the catalog text about a live booking, {time} and {manager}.
	meetingAlive string
}

// New builds the client. A nil httpClient means the default client; the timeout
// of each call is set by the context from cfg.Timeout.
func New(cfg config.OpenRouter, catalog config.Catalog, prompts Prompts, kb knowledge.Set,
	guard Guard, rules qualify.Rules, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		cfg: cfg, http: httpClient, prompts: prompts, knowledge: kb,
		guard: guard, rules: rules, location: catalog.Location, meetingAlive: catalog.Texts.MeetingAlive,
	}
}

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
	// ReasoningDetails is resent as is: without it some providers
	// reject the tool loop with a 400 error.
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
}

type tool struct {
	Type     string   `json:"type"`
	Function function `json:"function"`
}

type function struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type provider struct {
	RequireParameters bool   `json:"require_parameters"`
	DataCollection    string `json:"data_collection"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type request struct {
	Model          string          `json:"model"`
	Messages       []message       `json:"messages"`
	MaxTokens      int             `json:"max_tokens"`
	Temperature    *float64        `json:"temperature,omitempty"`
	Tools          []tool          `json:"tools,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Provider       provider        `json:"provider"`
	SessionID      string          `json:"session_id,omitempty"`
}

type response struct {
	Choices []struct {
		Message struct {
			Content          string          `json:"content"`
			ToolCalls        []toolCall      `json:"tool_calls"`
			ReasoningDetails json.RawMessage `json:"reasoning_details"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int     `json:"prompt_tokens"`
		CompletionTokens int     `json:"completion_tokens"`
		Cost             float64 `json:"cost"`
	} `json:"usage"`
}

// newRequest - the common part of a request: the provider does not store data, and the parameters
// are required to be supported, otherwise the strict schema is silently not applied.
func (c *Client) newRequest(model string, messages []message, sessionID string) request {
	zero := 0.0
	return request{
		Model: model, Messages: messages, MaxTokens: c.cfg.MaxCompletionTokens,
		Temperature: &zero,
		Provider:    provider{RequireParameters: true, DataCollection: "deny"},
		SessionID:   sessionID,
	}
}

// call performs one HTTP call and decodes the answer. There are no retries inside:
// replaying the turn is managed by dialog.
func (c *Client) call(ctx context.Context, payload request) (response, error) {
	if c.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
		defer cancel()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return response{}, &ExternalFailure{Kind: "openrouter_request", Message: err.Error(), Permanent: true}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return response{}, &ExternalFailure{Kind: "openrouter_request", Message: err.Error(), Permanent: true}
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return response{}, ctx.Err()
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return response{}, &ExternalFailure{Kind: "openrouter_timeout", Message: "OpenRouter timeout"}
		}
		return response{}, &ExternalFailure{Kind: "openrouter_transport", Message: err.Error()}
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Warn("close OpenRouter response body", "error", err)
		}
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// The error body goes into the decision audit, but not the log: the provider may
		// quote the prompt.
		return response{}, &ExternalFailure{
			Kind:      "openrouter_http",
			Message:   fmt.Sprintf("OpenRouter HTTP %d: %s", resp.StatusCode, bodySnippet(resp.Body)),
			Permanent: !retryableStatus(resp.StatusCode),
			Status:    resp.StatusCode,
		}
	}
	var decoded response
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&decoded); err != nil {
		return response{}, &ExternalFailure{Kind: "openrouter_response", Message: "OpenRouter returned invalid JSON"}
	}
	c.addSpent(decoded.Usage.Cost)
	slog.Debug("openrouter call", "session_id", payload.SessionID, "model", payload.Model,
		"duration_ms", time.Since(started).Milliseconds(),
		"prompt_tokens", decoded.Usage.PromptTokens, "completion_tokens", decoded.Usage.CompletionTokens,
		"cost", decoded.Usage.Cost)
	if len(decoded.Choices) == 0 {
		return response{}, &ExternalFailure{Kind: "openrouter_response", Message: "OpenRouter returned no choices"}
	}
	return decoded, nil
}

// retryableStatus: 402 is temporary, money appears after a top-up; 404 from
// OpenRouter means "no provider right now", not a missing resource. 400
// is replayed too: the turn history is rebuilt, and the rejected answer will not
// get into it again.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusRequestTimeout, http.StatusPaymentRequired,
		http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	}
	return false
}

func bodySnippet(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, 512))
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(raw)), " ")
}

// sessionID is the same for all calls of a case: the provider keeps one route.
func sessionID(caseID int64) string {
	return "case:" + strconv.FormatInt(caseID, 10)
}

// MeetingAliveText - the answer about a live booking when a model draft was cut to nothing:
// the time and the manager come from the database, not from the model.
func (c *Client) MeetingAliveText(start time.Time, manager string) string {
	return strings.NewReplacer("{time}", qualify.DateTimeText(start, c.location), "{manager}", manager).Replace(c.meetingAlive)
}

func (c *Client) addSpent(cost float64) {
	c.spentMu.Lock()
	defer c.spentMu.Unlock()
	c.spent += cost
}

// Spent - the cost in USD of all model calls of this client (turns, critic, niche classifier).
// The provider reports it in the response; a provider that does not report it gives zero.
func (c *Client) Spent() float64 {
	c.spentMu.Lock()
	defer c.spentMu.Unlock()
	return c.spent
}
