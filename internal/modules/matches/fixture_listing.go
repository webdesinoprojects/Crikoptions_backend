package matches

import (
	"sort"
	"strings"
	"time"
)

// listingStartTolerance is how far apart two kickoffs may be and still describe
// the same fixture. The provider sometimes revises a scheduled start by a few
// minutes and reissues the fixture under a fresh id, leaving two records whose
// times no longer agree to the second.
//
// Kept deliberately tight: the drift observed in production was ten minutes
// (Zambia v Nigeria, 13:00 against 12:50), so fifteen covers it with margin.
// Widening it is not free — the wider the window, the more chance of hiding a
// fixture that is genuinely separate.
const listingStartTolerance = 15 * time.Minute

// fixtureIdentityKey is everything about a fixture that cannot drift. Kickoff
// is deliberately absent and compared separately, within the tolerance above.
// Names and logos are display data: matching on those could merge distinct
// teams, competitions or fixtures in a double-header.
type fixtureIdentityKey struct {
	provider       string
	league, season int64
	teamA, teamB   int64
	format         string
}

func listingKey(match Match) (fixtureIdentityKey, bool) {
	format := strings.ToUpper(strings.TrimSpace(match.Format))
	if match.DataSource != DataSourceCricLive || match.Provider == "" ||
		match.ProviderFixtureID <= 0 || match.ProviderLeagueID <= 0 ||
		match.ProviderTeamAID <= 0 || match.ProviderTeamBID <= 0 ||
		match.ProviderTeamAID == match.ProviderTeamBID || match.StartTime.IsZero() ||
		(format != "T20" && format != "ODI") {
		return fixtureIdentityKey{}, false
	}
	a, b := match.ProviderTeamAID, match.ProviderTeamBID
	if a > b {
		a, b = b, a
	}
	return fixtureIdentityKey{
		provider: match.Provider, league: match.ProviderLeagueID, season: match.ProviderSeasonID,
		teamA: a, teamB: b, format: format,
	}, true
}

func withinListingTolerance(a, b time.Time) bool {
	delta := a.Sub(b)
	if delta < 0 {
		delta = -delta
	}
	return delta <= listingStartTolerance
}

// deduplicateUpcomingFixtures suppresses duplicate upcoming cards only. It
// never rewrites ids, hides stored records or changes polling/settlement. If a
// different provider key starts receiving live data, the live card takes over
// and the upcoming alias disappears from the listing. Multiple live records are
// deliberately retained: they may have independent trades and histories.
func deduplicateUpcomingFixtures(input []Match, now time.Time) []Match {
	liveStarts := make(map[fixtureIdentityKey][]time.Time)
	candidates := make(map[fixtureIdentityKey][]int)
	for i := range input {
		match := input[i]
		if match.Hidden || LiveFeedExpired(&match, now) {
			continue
		}
		key, ok := listingKey(match)
		if !ok {
			continue
		}
		switch NormalizeStatus(match.Status) {
		case StatusLive, StatusInningsBreak:
			liveStarts[key] = append(liveStarts[key], match.StartTime.UTC())
		case StatusUpcoming:
			if staleUpcoming(&match, now) {
				continue
			}
			candidates[key] = append(candidates[key], i)
		}
	}

	suppressed := make(map[int]bool)
	for key, indices := range candidates {
		// A live record for this fixture already has a card, so every upcoming
		// alias near its kickoff is redundant.
		remaining := indices[:0:0]
		for _, idx := range indices {
			if nearAnyStart(liveStarts[key], input[idx].StartTime.UTC()) {
				suppressed[idx] = true
				continue
			}
			remaining = append(remaining, idx)
		}
		// Cluster what is left by kickoff proximity and keep one per cluster.
		// Each member is compared with the cluster's first kickoff rather than
		// its predecessor, so a long chain of small gaps cannot drift a cluster
		// across genuinely distinct fixtures.
		sort.SliceStable(remaining, func(x, y int) bool {
			return input[remaining[x]].StartTime.Before(input[remaining[y]].StartTime)
		})
		anchor, winner := time.Time{}, -1
		for _, idx := range remaining {
			start := input[idx].StartTime.UTC()
			if winner < 0 || !withinListingTolerance(anchor, start) {
				anchor, winner = start, idx
				continue
			}
			if preferUpcomingFixture(input[idx], input[winner]) {
				suppressed[winner] = true
				winner = idx
			} else {
				suppressed[idx] = true
			}
		}
	}

	out := make([]Match, 0, len(input))
	for i := range input {
		if suppressed[i] {
			continue
		}
		out = append(out, input[i])
	}
	return out
}

func nearAnyStart(starts []time.Time, start time.Time) bool {
	for _, candidate := range starts {
		if withinListingTolerance(candidate, start) {
			return true
		}
	}
	return false
}

func preferUpcomingFixture(candidate, current Match) bool {
	// Successful observation outranks schedule-only records. Without one,
	// prefer the most recently refreshed publication. This is a display choice,
	// not proof that either provider key is invalid.
	var candidatePoll, currentPoll time.Time
	if candidate.LastSuccessfulPollAt != nil {
		candidatePoll = *candidate.LastSuccessfulPollAt
	}
	if current.LastSuccessfulPollAt != nil {
		currentPoll = *current.LastSuccessfulPollAt
	}
	if !candidatePoll.Equal(currentPoll) {
		return candidatePoll.After(currentPoll)
	}
	if !candidate.UpdatedAt.Equal(current.UpdatedAt) {
		return candidate.UpdatedAt.After(current.UpdatedAt)
	}
	if !candidate.CreatedAt.Equal(current.CreatedAt) {
		return candidate.CreatedAt.After(current.CreatedAt)
	}
	return candidate.ID.Hex() < current.ID.Hex()
}
