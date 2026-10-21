package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chatResponse - a chat completions answer in the OpenRouter shape with a single text.
func chatResponse(content string) string {
	body, _ := json.Marshal(map[string]any{
		"id": "gen-test", "object": "chat.completion", "model": "example/chat-model",
		"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": content}}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15, "cost": 0},
	})
	return string(body)
}

// mockOpenRouter: healthy means the model answers with text and the critic approves;
// otherwise every call gets 503.
func mockOpenRouter(t *testing.T, healthy bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !healthy {
			http.Error(w, `{"error":{"code":503,"message":"no provider"}}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch text := string(body); {
		case strings.Contains(text, `"problems"`):
			_, _ = io.WriteString(w, chatResponse(`{"ok":true,"problems":[]}`))
		case strings.Contains(text, `"off_profile_reason"`):
			_, _ = io.WriteString(w, chatResponse(`{"direction":"unclear","off_profile_reason":"","quote":""}`))
		default:
			_, _ = io.WriteString(w, chatResponse("Хорошо, спасибо за ответ."))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// The "no technical failures" criterion: the report counts transport_error runs,
// and any such run makes the prompteval exit code non-zero.
// Not parallel: the model address comes through the environment.
func TestRun_CountsTransportErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		healthy bool
	}{{"model answers", true}, {"model unavailable", false}} {
		t.Run(tc.name, func(t *testing.T) {
			server := mockOpenRouter(t, tc.healthy)
			t.Setenv("OPENROUTER_API_KEY", "test-key")
			t.Setenv("OPENROUTER_URL", server.URL)
			out := filepath.Join(t.TempDir(), "report.json")
			err := run(context.Background(), options{catalogDir: "../../data/example",
				dir: "../../data/example/golden", runs: 2, out: out, maxUSD: 5})
			if (err == nil) != tc.healthy {
				t.Fatalf("run error = %v, want failure only when transport fails", err)
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("read report: %v", err)
			}
			var report Report
			if err := json.Unmarshal(data, &report); err != nil {
				t.Fatalf("parse report: %v", err)
			}
			want := 0
			if !tc.healthy {
				want = report.Fixtures * report.Runs
			}
			if report.Fixtures != 15 || report.Runs != 2 || report.TransportErrors != want {
				t.Fatalf("report fixtures %d, runs %d, transport errors %d; want 15, 2, %d",
					report.Fixtures, report.Runs, report.TransportErrors, want)
			}
			if tc.healthy && report.ClassCounts[ClassTransport] != 0 {
				t.Errorf("healthy model: class counts %v, want no %s", report.ClassCounts, ClassTransport)
			}
		})
	}
}

// A run that reaches the spending ceiling stops before the next turn, writes the report of what
// was done and fails with the ceiling error (exit code 3 in main).
func TestRun_StopsOnSpendingCeiling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		answer := map[string]any{"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": "Хорошо, спасибо за ответ."}}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.4}}
		switch text := string(body); {
		case strings.Contains(text, `"problems"`):
			answer["choices"].([]map[string]any)[0]["message"].(map[string]any)["content"] = `{"ok":true,"problems":[]}`
		case strings.Contains(text, `"off_profile_reason"`):
			answer["choices"].([]map[string]any)[0]["message"].(map[string]any)["content"] =
				`{"direction":"unclear","off_profile_reason":"","quote":""}`
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(server.Close)
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("OPENROUTER_URL", server.URL)
	out := filepath.Join(t.TempDir(), "report.json")
	err := run(context.Background(), options{catalogDir: "../../data/example",
		dir: "../../data/example/golden", runs: 2, out: out, maxUSD: 1})
	if !errors.Is(err, errBudgetCeiling) {
		t.Fatalf("run error = %v, want the spending ceiling error", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if !report.BudgetStop || report.MaxUSD != 1 || report.CostUSD < 1 || report.Fixtures == 0 || report.Fixtures >= 15 {
		t.Fatalf("report stop=%v max=%v cost=%v fixtures=%d, want a partial report over the ceiling",
			report.BudgetStop, report.MaxUSD, report.CostUSD, report.Fixtures)
	}
	if err := run(context.Background(), options{catalogDir: "../../data/example", runs: 1, maxUSD: 0}); err == nil {
		t.Fatal("max-usd 0 must be refused")
	}
}
