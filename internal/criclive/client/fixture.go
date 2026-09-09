package client

import (
	"strconv"
	"strings"
	"time"
)

// liveDateLayouts covers the human-readable start time on /cricket/live, which
// omits the year ("Sep 09, 10:00 GMT"). The year is inferred from the polling
// clock, so a fixture listed in late December for early January still lands on
// the correct side of the year boundary.
var liveDateLayouts = []string{
	"Jan 02, 15:04 MST",
	"Jan 2, 15:04 MST",
	"Jan 02, 15:04",
	"Jan 2, 15:04",
}

// ParseLiveDate reads /cricket/live's `date` field relative to now. It returns
// the zero time when the value is unparseable; callers treat that as "start
// time unknown" rather than as an error, because a missing start time must not
// discard an otherwise valid live fixture.
func ParseLiveDate(value string, now time.Time) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	for _, layout := range liveDateLayouts {
		parsed, err := time.Parse(layout, value)
		if err != nil {
			continue
		}
		// Go defaults a yearless timestamp to year 0; rebuild it against the
		// current year and correct for a wrap across the year boundary.
		candidate := time.Date(now.Year(), parsed.Month(), parsed.Day(),
			parsed.Hour(), parsed.Minute(), 0, 0, parsed.Location())
		switch {
		case candidate.Sub(now) > 300*24*time.Hour:
			candidate = candidate.AddDate(-1, 0, 0)
		case now.Sub(candidate) > 300*24*time.Hour:
			candidate = candidate.AddDate(1, 0, 0)
		}
		return candidate.UTC()
	}
	return time.Time{}
}

// ParseEpochMillis reads the quoted millisecond timestamps used by
// /cricket/schedule.
func ParseEpochMillis(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	millis, err := strconv.ParseInt(value, 10, 64)
	if err != nil || millis <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(millis).UTC()
}

// FixtureFromMatchItem normalizes a /cricket/live entry. Team ordering differs
// between CricLive endpoints, so downstream code must match on team ID rather
// than on local/visitor position.
func FixtureFromMatchItem(item MatchItem, now time.Time) Fixture {
	return Fixture{
		ID:                 item.MatchID,
		SeriesID:           item.SeriesID,
		SeriesName:         item.SeriesName,
		MatchDesc:          item.MatchDesc,
		Format:             item.Format,
		MatchType:          item.MatchType,
		StartingAt:         ParseLiveDate(item.Date, now),
		LocalTeamID:        item.FirstTeam.ID,
		VisitorTeamID:      item.SecondTeam.ID,
		LocalTeamName:      teamDisplayName(item.FirstTeam),
		VisitorTeamName:    teamDisplayName(item.SecondTeam),
		LocalTeamShort:     strings.TrimSpace(item.FirstTeam.Name),
		VisitorTeamShort:   strings.TrimSpace(item.SecondTeam.Name),
		LocalTeamImageID:   item.FirstTeam.ImageID,
		VisitorTeamImageID: item.SecondTeam.ImageID,
		Venue:              item.Venue,
		Status:             strings.TrimSpace(item.State),
		StatusDetail:       strings.TrimSpace(item.StatusDetail),
		State:              strings.TrimSpace(item.State),
		Live:               IsLiveState(item.State),
		Raw:                item.Raw,
	}
}

// FixtureFromScheduleMatch normalizes a /cricket/schedule entry. The schedule
// is the only endpoint with an unambiguous start timestamp, so it is the
// authority for pre-match scheduling.
func FixtureFromScheduleMatch(match ScheduleMatch, series ScheduleSeries) Fixture {
	venue := strings.TrimSpace(match.Ground)
	if city := strings.TrimSpace(match.City); city != "" {
		if venue == "" {
			venue = city
		} else {
			venue = venue + ", " + city
		}
	}
	return Fixture{
		ID:               match.MatchID,
		SeriesID:         series.SeriesID,
		SeriesName:       strings.TrimSpace(series.SeriesName),
		MatchDesc:        strings.TrimSpace(match.MatchDesc),
		Format:           strings.TrimSpace(match.MatchFormat),
		MatchType:        strings.TrimSpace(series.SeriesCategory),
		StartingAt:       ParseEpochMillis(match.StartDate),
		LocalTeamID:      match.Team1ID,
		VisitorTeamID:    match.Team2ID,
		LocalTeamName:    strings.TrimSpace(match.Team1),
		VisitorTeamName:  strings.TrimSpace(match.Team2),
		LocalTeamShort:   strings.TrimSpace(match.Team1Short),
		VisitorTeamShort: strings.TrimSpace(match.Team2Short),
		Venue:            venue,
		Status:           StatePreview,
		State:            StatePreview,
	}
}

func teamDisplayName(team TeamItem) string {
	if full := strings.TrimSpace(team.FullName); full != "" {
		return full
	}
	return strings.TrimSpace(team.Name)
}

// CricLive match states observed on /cricket/live and in the commentary
// miniscore. They are compared case-insensitively because the same state
// appears with different casing across endpoints.
const (
	StatePreview    = "Preview"
	StateInProgress = "In Progress"
	StateInnings    = "Innings Break"
	StateStumps     = "Stumps"
	StateComplete   = "Complete"
	StateAbandon    = "Abandon"
	StateToss       = "Toss"
	StateRain       = "Rain"
)

// IsLiveState reports whether play is under way or paused mid-match, which is
// what makes a fixture worth polling on the fast cycle.
func IsLiveState(state string) bool {
	switch normalizeState(state) {
	case "in progress", "innings break", "toss", "rain", "delay", "drinks", "tea", "lunch", "stumps":
		return true
	default:
		return false
	}
}

// IsTerminalState reports whether CricLive considers the match finished, which
// gates settlement.
func IsTerminalState(state string) bool {
	switch normalizeState(state) {
	case "complete", "abandon", "abandoned", "cancelled", "canceled", "no result":
		return true
	default:
		return false
	}
}

// IsNotStartedState reports a fixture that has not begun play.
func IsNotStartedState(state string) bool {
	switch normalizeState(state) {
	case "preview", "upcoming", "scheduled", "":
		return true
	default:
		return false
	}
}

func normalizeState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}
