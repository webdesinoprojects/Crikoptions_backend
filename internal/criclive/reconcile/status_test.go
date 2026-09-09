package reconcile

import (
	"testing"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

func TestIsExplicitTerminalProviderStatus(t *testing.T) {
	cases := map[string]bool{
		"Complete":    true,
		"Abandon":     true,
		"No Result":   true,
		"In Progress": false,
		"Preview":     false,
		"Stumps":      false,
	}
	for state, want := range cases {
		if got := IsExplicitTerminalProviderStatus(state); got != want {
			t.Fatalf("state %q: got %v want %v", state, got, want)
		}
	}
}

func TestNormalizeProviderStatusMapsCricLiveStates(t *testing.T) {
	cases := map[string]string{
		"Complete":      matches.StatusCompleted,
		"Abandon":       matches.StatusAbandoned,
		"In Progress":   matches.StatusLive,
		"Innings Break": matches.StatusInningsBreak,
		"Stumps":        matches.StatusInningsBreak,
		"Rain":          matches.StatusInningsBreak,
		"Preview":       matches.StatusUpcoming,
		"Toss":          matches.StatusUpcoming,
		"":              matches.StatusUpcoming,
		// An unrecognized phase must never open trading by default.
		"some-new-phase": matches.StatusUpcoming,
	}
	for state, want := range cases {
		if got := NormalizeProviderStatus(state); got != want {
			t.Fatalf("state %q: got %q want %q", state, got, want)
		}
	}
}

func TestIsNotStartedStatus(t *testing.T) {
	for _, state := range []string{"", "Preview", "Scheduled", "Toss", "Not Started"} {
		if !IsNotStartedStatus(state) {
			t.Fatalf("state %q should be not-started", state)
		}
	}
	for _, state := range []string{"In Progress", "Innings Break", "Complete", "Stumps"} {
		if IsNotStartedStatus(state) {
			t.Fatalf("state %q should not be not-started", state)
		}
	}
}
