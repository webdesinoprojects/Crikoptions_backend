package reconcile

import (
	"strings"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

// NormalizeProviderStatus maps a CricLive match state to a local match status.
// CricLive reports the state on /cricket/live and again in the commentary
// miniscore; both use the same vocabulary ("Preview", "In Progress",
// "Innings Break", "Stumps", "Complete", "Abandon").
func NormalizeProviderStatus(state string) string {
	lower := strings.ToLower(strings.TrimSpace(state))
	switch {
	case lower == "":
		return matches.StatusUpcoming
	case containsAny(lower, "complete", "finish", "won ", " won", "result", "tie"):
		return matches.StatusCompleted
	case containsAny(lower, "aban", "cancel", "cancl", "no result", "washed"):
		return matches.StatusAbandoned
	case containsAny(lower, "innings break", "lunch", "tea", "drinks", "dinner", "stumps", "rain", "delay", "wet ", "bad light"):
		return matches.StatusInningsBreak
	case containsAny(lower, "in progress", "live", "play"):
		return matches.StatusLive
	case containsAny(lower, "preview", "upcoming", "scheduled", "toss", "not started"):
		return matches.StatusUpcoming
	default:
		// An unrecognized state must not silently open trading.
		return matches.StatusUpcoming
	}
}

// IsExplicitTerminalProviderStatus reports states that definitively end a match.
func IsExplicitTerminalProviderStatus(state string) bool {
	lower := strings.ToLower(strings.TrimSpace(state))
	return containsAny(lower, "complete", "finish", "aban", "cancel", "cancl", "no result")
}

// IsTerminalProviderStatus reports whether the provider state should close the
// public match.
func IsTerminalProviderStatus(state string) bool {
	local := NormalizeProviderStatus(state)
	return local == matches.StatusCompleted || local == matches.StatusAbandoned
}

// IsNotStartedStatus is intentionally strict: unknown provider phases remain
// non-tradable but may not create a new public match.
func IsNotStartedStatus(state string) bool {
	lower := strings.ToLower(strings.TrimSpace(state))
	return lower == "" || containsAny(lower, "preview", "upcoming", "scheduled", "not started", "toss", "delay", "postp")
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
