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

// A no-result phrase contains "result", so an ordering slip in
// NormalizeProviderStatus silently settles a match that must be voided. These
// are the real phrases CricketLineApi emits.
func TestNormalizeProviderStatusPrefersAbandonmentOverResult(t *testing.T) {
	cases := map[string]string{
		"No Result":                   matches.StatusAbandoned,
		"No Result/due to rain":       matches.StatusAbandoned,
		"Match abandoned due to rain": matches.StatusAbandoned,
		"Match cancelled":             matches.StatusAbandoned,
		"Washed out":                  matches.StatusAbandoned,
		// Genuine results must still settle.
		"India won by 5 wickets": matches.StatusCompleted,
		"Match tied":             matches.StatusCompleted,
		"Complete":               matches.StatusCompleted,
	}
	for phase, want := range cases {
		if got := NormalizeProviderStatus(phase); got != want {
			t.Fatalf("phase %q: got %q want %q", phase, got, want)
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

// providerPhase carries raw provider text, so this matcher decides whether the
// stuck-phase reaper may close a match on it. The in-progress cases below are
// the ones that matter: cricketline's TestMatchStateReadsOnlyWholePhrases reads
// every one of them as still in play, and a substring match on "cancel" or
// "no result" would end a live game.
func TestIsConfirmedAbandonmentPhase(t *testing.T) {
	abandoned := []string{
		"No Result",
		"No Result/due to rain",
		"Match abandoned due to rain",
		"Match Abandoned",
		"Match cancelled",
		"Match canceled",
		"Abandoned",
		"Called off",
		"Washed out",
	}
	for _, phase := range abandoned {
		if !IsConfirmedAbandonmentPhase(phase) {
			t.Fatalf("phase %q should be a confirmed abandonment phase", phase)
		}
	}

	inPlay := []string{
		"",
		"   ",
		// Live ticker lines that name an umpire event, not the match ending.
		"Review Cancelled",
		"No Result - Review",
		"Play called off",
		"Team Review",
		"DRS Review - no result",
		// Ordinary in-play and break text.
		"Rocks need 13 runs in 7 balls",
		"Rain stopped play",
		"Innings Break",
		"Stumps",
		"Tea",
		"In Progress",
		"Preview",
		// A result is not closed by this reaper: "won" appears in every toss
		// line and a finished match settles through its own path.
		"India won by 5 wickets",
		"Rocks won the toss and chose to bowl",
		"Match tied",
		// Completion is out of scope too. This reaper only voids, and voiding
		// the markets of a match that genuinely finished would be worse than
		// leaving its card up for reconciliation to settle.
		"Complete",
		"Completed",
		"Finished",
	}
	for _, phase := range inPlay {
		if IsConfirmedAbandonmentPhase(phase) {
			t.Fatalf("phase %q must not be treated as an abandonment", phase)
		}
	}
}
