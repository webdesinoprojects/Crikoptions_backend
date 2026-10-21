package matches

import (
	"strings"
	"time"
)

// fixtureListingKey requires exact provider identity and kickoff time. Names
// and logos are display data: matching them alone could merge distinct teams,
// competitions or fixtures in a double-header.
type fixtureListingKey struct {
	provider       string
	league, season int64
	teamA, teamB   int64
	format         string
	start          time.Time
}

func listingKey(match Match) (fixtureListingKey, bool) {
	format := strings.ToUpper(strings.TrimSpace(match.Format))
	if match.DataSource != DataSourceCricLive || match.Provider == "" ||
		match.ProviderFixtureID <= 0 || match.ProviderLeagueID <= 0 ||
		match.ProviderTeamAID <= 0 || match.ProviderTeamBID <= 0 ||
		match.ProviderTeamAID == match.ProviderTeamBID || match.StartTime.IsZero() ||
		(format != "T20" && format != "ODI") {
		return fixtureListingKey{}, false
	}
	a, b := match.ProviderTeamAID, match.ProviderTeamBID
	if a > b {
		a, b = b, a
	}
	return fixtureListingKey{
		provider: match.Provider, league: match.ProviderLeagueID, season: match.ProviderSeasonID,
		teamA: a, teamB: b, format: format, start: match.StartTime.UTC(),
	}, true
}

// deduplicateUpcomingFixtures suppresses duplicate upcoming cards only. It
// never rewrites IDs, hides stored records or changes polling/settlement. If a
// different provider key starts receiving live data, the live card takes over
// and the upcoming alias disappears from the listing. Multiple live records
// are deliberately retained: they may have independent trades and histories.
func deduplicateUpcomingFixtures(input []Match, now time.Time) []Match {
	live := make(map[fixtureListingKey]bool)
	upcoming := make(map[fixtureListingKey]int)
	for i, match := range input {
		if match.Hidden || LiveFeedExpired(&match, now) {
			continue
		}
		key, ok := listingKey(match)
		if !ok {
			continue
		}
		switch NormalizeStatus(match.Status) {
		case StatusLive, StatusInningsBreak:
			live[key] = true
		case StatusUpcoming:
			if staleUpcoming(&match, now) {
				continue
			}
			previous, exists := upcoming[key]
			if !exists || preferUpcomingFixture(match, input[previous]) {
				upcoming[key] = i
			}
		}
	}
	out := make([]Match, 0, len(input))
	for i, match := range input {
		if NormalizeStatus(match.Status) == StatusUpcoming {
			if key, ok := listingKey(match); ok {
				if live[key] {
					continue
				}
				if winner, exists := upcoming[key]; exists && winner != i {
					continue
				}
			}
		}
		out = append(out, match)
	}
	return out
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
