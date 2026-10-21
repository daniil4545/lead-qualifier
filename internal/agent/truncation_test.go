package agent

import (
	"context"
	"testing"

	"github.com/daniil4545/lead-qualifier/internal/qualify"
)

// A reply cut by the token cap is not a reply: the turn repeats the call once and the stump is
// never shown to the lead or checked by the critic as if it were whole.
func TestRunTurn_TruncatedReply(t *testing.T) {
	t.Parallel()
	fx := loadFixture(t)
	input := turnInput(qualify.StatusCollecting, fx.fullFacts("3000000", ""), "Спасибо, всё понятно.")
	t.Run("repeat succeeds", func(t *testing.T) {
		t.Parallel()
		turn, err := fx.client(t, chat("Рада, что", "length"), chat("Рада, что всё понятно.", "stop")).
			RunTurn(context.Background(), input)
		if err != nil {
			t.Fatalf("RunTurn: %v", err)
		}
		if turn.FinalText != "Рада, что всё понятно." {
			t.Fatalf("text = %q, want the repeated reply and not the stump", turn.FinalText)
		}
	})
	t.Run("second stump is a technical outcome", func(t *testing.T) {
		t.Parallel()
		turn, err := fx.client(t, chat("Рада, что", "length"), chat("Рада, что всё по", "length")).
			RunTurn(context.Background(), input)
		if err != nil {
			t.Fatalf("RunTurn: %v", err)
		}
		if turn.Terminal != TerminalManualReview || turn.Reason != "empty_content" || turn.FinalText != "" {
			t.Fatalf("terminal=%s reason=%q text=%q, want manual_review empty_content", turn.Terminal, turn.Reason, turn.FinalText)
		}
		if !TechnicalReasons[turn.Reason] {
			t.Fatal("empty_content must be a technical reason so that dialog defers and replays the turn")
		}
	})
}

// A lone hyphen line is the model's "no text" mark. Next to real text it is removed; alone it
// stays the refusal to write, which the code text after the reply covers.
func TestDropDashLines(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, in, want string }{
		{"no dash", "Рада помочь.", "Рада помочь."},
		{"dash after text", "Рада помочь.\n-", "Рада помочь."},
		{"dash before and after", "-\nРада помочь.\n–\n", "Рада помочь."},
		{"only dash", " - ", ""},
		{"list item is kept", "Есть два варианта:\n- первый\n- второй", "Есть два варианта:\n- первый\n- второй"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := dropDashLines(tc.in); got != tc.want {
				t.Fatalf("dropDashLines(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
