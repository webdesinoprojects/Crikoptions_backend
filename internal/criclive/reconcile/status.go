package reconcile

import (
	"regexp"
	"strings"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

var (
	// confirmedAbandonmentPhrase matches phase text that says the match was
	// called off. It is anchored on purpose. ProviderPhase holds raw provider
	// text (ReduceSnapshot fills it from MiniScore.CustomStatus, which the
	// cricketline adapter sets to the live ticker or status line verbatim), and
	// that text reads "Review Cancelled" or "Play called off" while a chase is
	// still running. Substring matching on it closes live matches.
	//
	// Completion and result words are both deliberately absent. "won" appears
	// in every toss line, and a match that genuinely finished settles through
	// reconciliation; voiding its markets on phase text alone would be worse
	// than leaving the card up.
	confirmedAbandonmentPhrase = regexp.MustCompile(`(?i)^(the\s+)?(match\s+)?(abandoned|cancelled|canceled|called\s+off|washed\s+out|no\s+result)\b|\bmatch\s+(abandoned|cancelled|canceled|called\s+off|washed\s+out)\b`)

	// reviewNoise marks phase text naming an in-play umpire or DRS event rather
	// than the end of the match. "No Result - Review" is a review outcome, not
	// an abandonment, which is why the adapter reads it as in-progress.
	reviewNoise = regexp.MustCompile(`(?i)\breview\b|\bdrs\b|\bumpire\b|\bappeal\b`)
)

// IsConfirmedAbandonmentPhase reports whether raw provider phase text states
// that the match was called off. Unlike IsExplicitTerminalProviderStatus it is
// safe to run on free text, but it is not sufficient on its own: a caller
// acting on it must also require that the phrase has persisted, because the
// provider can emit an abandonment-looking line for a single poll. See
// Store.ClosePhaseTerminalMatches.
func IsConfirmedAbandonmentPhase(phase string) bool {
	lower := strings.ToLower(strings.TrimSpace(phase))
	if lower == "" || reviewNoise.MatchString(lower) {
		return false
	}
	return confirmedAbandonmentPhrase.MatchString(lower)
}

// NormalizeProviderStatus maps a CricLive match state to a local match status.
// CricLive reports the state on /cricket/live and again in the commentary
// miniscore; both use the same vocabulary ("Preview", "In Progress",
// "Innings Break", "Stumps", "Complete", "Abandon").
func NormalizeProviderStatus(state string) string {
	lower := strings.ToLower(strings.TrimSpace(state))
	switch {
	case lower == "":
		return matches.StatusUpcoming
	// Abandonment is tested before completion and must stay that way: "no
	// result" contains "result", so the completed arm would otherwise swallow
	// every no-result phrase and settle a match that should have been voided.
	case containsAny(lower, "aban", "cancel", "cancl", "no result", "washed"):
		return matches.StatusAbandoned
	case containsAny(lower, "complete", "finish", "won ", " won", "result", "tie"):
		return matches.StatusCompleted
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
//
// It substring-matches, so it is only safe on a canonical provider state
// ("Complete", "Aban.") or an exact stored phrase. Never call it on free phase
// text — "Review Cancelled" would read as terminal. Use
// IsConfirmedAbandonmentPhase for that.
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
