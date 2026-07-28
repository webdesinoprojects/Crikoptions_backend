package matches

import (
	"strings"
	"time"
)

// StaleLiveGrace is how long a provider match may go without a successful poll
// before we stop presenting it as in-play. Healthy fixtures poll every few
// seconds (see adaptivePollInterval), so this only trips on a genuinely dead
// feed, never on a slow tick or a long innings break.
const StaleLiveGrace = 15 * time.Minute

// DeadLiveMatchAfter is the wall-clock deadline for a provider match that still
// claims to be in play. Nothing legitimate stays live this long after its
// scheduled start — even a rain-hit Test day is bounded — so a match past this
// with a dead feed is an orphan the provider will never close for us.
const DeadLiveMatchAfter = 12 * time.Hour

// isProviderSourced reports whether the match is fed by Sportmonks rather than
// a simulator replay. Demo matches are driven locally and have no poll clock,
// so staleness never applies to them.
func isProviderSourced(match *Match) bool {
	return strings.EqualFold(strings.TrimSpace(match.DataSource), DataSourceSportmonks) ||
		strings.EqualFold(strings.TrimSpace(match.Provider), DataSourceSportmonks)
}

// lastFeedContact returns the best available "we last heard from the provider"
// timestamp.
//
// StartTime is deliberately NOT a fallback: every live match is older than the
// grace window minutes after its start, so using it would hide healthy fixtures.
// A zero result means "unknown", which callers treat as fresh — hiding a real
// live match is far worse than leaving a dead one up for the reaper to close.
func lastFeedContact(match *Match) time.Time {
	if match.LastSuccessfulPollAt != nil && !match.LastSuccessfulPollAt.IsZero() {
		return match.LastSuccessfulPollAt.UTC()
	}
	if match.LastFeedReceivedAt != nil && !match.LastFeedReceivedAt.IsZero() {
		return match.LastFeedReceivedAt.UTC()
	}
	// The document is rewritten on every applied poll, so its write time is a
	// sound proxy when the explicit poll clocks are absent.
	return match.UpdatedAt.UTC()
}

// LiveFeedExpired reports whether a provider match still claims live/innings
// break status but has had no successful poll inside the grace window.
//
// These are zombies: the feed stopped without Sportmonks ever reporting a
// terminal phase, so CompleteStuckTerminalMatches (which keys off providerPhase)
// can never clear them and the frozen scoreboard would otherwise sit on the home
// feed forever.
func LiveFeedExpired(match *Match, now time.Time) bool {
	if match == nil || !isProviderSourced(match) {
		return false
	}
	switch NormalizeStatus(match.Status) {
	case StatusLive, StatusInningsBreak:
	default:
		return false
	}
	contact := lastFeedContact(match)
	if contact.IsZero() {
		return false
	}
	return now.UTC().Sub(contact) > StaleLiveGrace
}
