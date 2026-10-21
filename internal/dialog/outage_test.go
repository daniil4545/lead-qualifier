package dialog

import (
	"context"
	"testing"

	"github.com/daniil4545/lead-qualifier/internal/agent"
	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

// A provider outage does not hand the lead to a human after three attempts: the turn keeps
// being deferred for 24 hours from the lead's message, and only then the handoff happens.
func TestProcessCase_ProviderOutageWindow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	caseID := h.start("lead")
	outage := result{err: &agent.ExternalFailure{Kind: "openrouter_http", Message: "upstream 503", Status: 503}}
	h.say("lead", "у нас сеть магазинов", outage)
	for range 6 {
		h.agent.queue(caseID, outage)
		h.elapseBackoff()
		h.turnOf(caseID)
	}
	if got := h.status(caseID); got != qualify.StatusCollecting {
		t.Fatalf("status during the outage = %s, want collecting: the lead is still waiting for the model", got)
	}

	// The lead's message is now older than the window.
	if _, err := h.db.Exec(context.Background(),
		"UPDATE messages SET created_at = created_at - interval '25 hours' WHERE case_id = $1", caseID); err != nil {
		t.Fatalf("age messages: %v", err)
	}
	h.agent.queue(caseID, outage)
	h.elapseBackoff()
	h.turnOf(caseID)
	if got := h.status(caseID); got != qualify.StatusManualReview {
		t.Fatalf("status after the window = %s, want manual_review", got)
	}
}
