package client

import (
	"bytes"
	"encoding/json"
	"time"
)

// Envelope is the common Sportmonks response wrapper. Raw preserves the
// complete provider response for shadow-contract inspection without weakening
// the typed Data field used by normal callers.
type Envelope[T any] struct {
	Data      T               `json:"data"`
	Meta      Meta            `json:"meta,omitempty"`
	RateLimit RateLimit       `json:"-"`
	Raw       json.RawMessage `json:"-"`
}

func (e *Envelope[T]) UnmarshalJSON(data []byte) error {
	type wireEnvelope struct {
		Data T    `json:"data"`
		Meta Meta `json:"meta,omitempty"`
	}
	var wire wireEnvelope
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	e.Data = wire.Data
	e.Meta = wire.Meta
	e.Raw = append(e.Raw[:0], data...)
	return nil
}

type Meta struct {
	Pagination   *Pagination     `json:"pagination,omitempty"`
	Subscription json.RawMessage `json:"subscription,omitempty"`
}

type Pagination struct {
	Total       int                        `json:"total"`
	Count       int                        `json:"count"`
	PerPage     int                        `json:"per_page"`
	CurrentPage int                        `json:"current_page"`
	TotalPages  int                        `json:"total_pages"`
	Links       map[string]json.RawMessage `json:"links,omitempty"`
}

func (p Pagination) NextPage() (int, bool) {
	if p.CurrentPage > 0 && p.CurrentPage < p.TotalPages {
		return p.CurrentPage + 1, true
	}
	return 0, false
}

// RateLimit is parsed from response headers. Pointer-valued counters preserve
// the distinction between a real zero and an absent provider header.
type RateLimit struct {
	Limit      *int
	Remaining  *int
	ResetAt    *time.Time
	ResetAfter time.Duration
	RetryAfter time.Duration
}

type League struct {
	Resource  string          `json:"resource,omitempty"`
	ID        int64           `json:"id"`
	SeasonID  int64           `json:"season_id,omitempty"`
	CountryID int64           `json:"country_id,omitempty"`
	Name      string          `json:"name"`
	Code      string          `json:"code,omitempty"`
	Type      string          `json:"type,omitempty"`
	ImagePath string          `json:"image_path,omitempty"`
	UpdatedAt string          `json:"updated_at,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

func (l *League) UnmarshalJSON(data []byte) error {
	type leagueAlias League
	var value leagueAlias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*l = League(value)
	l.Raw = append(l.Raw[:0], data...)
	return nil
}

// Score describes a delivery score classification returned by /scores.
// Raw retains future provider fields so unknown taxonomy changes can be
// quarantined and inspected rather than silently discarded.
type Score struct {
	Resource   string          `json:"resource,omitempty"`
	ID         int64           `json:"id"`
	Name       string          `json:"name"`
	Runs       int             `json:"runs"`
	Four       bool            `json:"four"`
	Six        bool            `json:"six"`
	Bye        int             `json:"bye"`
	LegBye     int             `json:"leg_bye"`
	NoBall     int             `json:"noball"`
	NoBallRuns int             `json:"noball_runs"`
	IsWicket   bool            `json:"is_wicket"`
	Ball       bool            `json:"ball"`
	Out        bool            `json:"out"`
	Raw        json.RawMessage `json:"-"`
}

func (s *Score) UnmarshalJSON(data []byte) error {
	type scoreAlias Score
	var value scoreAlias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = Score(value)
	s.Raw = append(s.Raw[:0], data...)
	return nil
}

// Fixture models stable fixture fields and leaves included relations as raw
// JSON. DecodeRelation can decode either Sportmonks' {"data": ...} relation
// wrapper or a direct embedded value, covering both shapes seen in v2 payloads.
type Fixture struct {
	Resource         string          `json:"resource,omitempty"`
	ID               int64           `json:"id"`
	LeagueID         int64           `json:"league_id"`
	SeasonID         int64           `json:"season_id"`
	StageID          int64           `json:"stage_id"`
	Round            string          `json:"round,omitempty"`
	LocalTeamID      int64           `json:"localteam_id"`
	VisitorTeamID    int64           `json:"visitorteam_id"`
	StartingAt       string          `json:"starting_at"`
	Type             string          `json:"type"`
	Live             json.RawMessage `json:"live,omitempty"`
	Status           string          `json:"status"`
	LastPeriod       json.RawMessage `json:"last_period,omitempty"`
	Note             string          `json:"note,omitempty"`
	VenueID          int64           `json:"venue_id,omitempty"`
	TossWonTeamID    int64           `json:"toss_won_team_id,omitempty"`
	WinnerTeamID     int64           `json:"winner_team_id,omitempty"`
	TotalOversPlayed json.RawMessage `json:"total_overs_played,omitempty"`
	Elected          string          `json:"elected,omitempty"`
	SuperOver        bool            `json:"super_over,omitempty"`
	FollowOn         bool            `json:"follow_on,omitempty"`
	RPCOvers         json.RawMessage `json:"rpc_overs,omitempty"`
	RPCTarget        json.RawMessage `json:"rpc_target,omitempty"`

	Balls       json.RawMessage `json:"balls,omitempty"`
	Runs        json.RawMessage `json:"runs,omitempty"`
	Scoreboards json.RawMessage `json:"scoreboards,omitempty"`
	Batting     json.RawMessage `json:"batting,omitempty"`
	Bowling     json.RawMessage `json:"bowling,omitempty"`
	Lineup      json.RawMessage `json:"lineup,omitempty"`
	LocalTeam   json.RawMessage `json:"localteam,omitempty"`
	VisitorTeam json.RawMessage `json:"visitorteam,omitempty"`
	League      json.RawMessage `json:"league,omitempty"`
	Season      json.RawMessage `json:"season,omitempty"`
	Stage       json.RawMessage `json:"stage,omitempty"`
	Venue       json.RawMessage `json:"venue,omitempty"`

	Raw json.RawMessage `json:"-"`
}

func (f *Fixture) UnmarshalJSON(data []byte) error {
	type fixtureAlias Fixture
	var value fixtureAlias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*f = Fixture(value)
	f.Raw = append(f.Raw[:0], data...)
	return nil
}

// DecodeRelation handles both a direct relation and Sportmonks' conventional
// relation wrapper. A missing or JSON-null relation decodes to T's zero value.
func DecodeRelation[T any](raw json.RawMessage) (T, error) {
	var zero T
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return zero, nil
	}
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var wrapper struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(trimmed, &wrapper); err != nil {
			return zero, err
		}
		if len(wrapper.Data) > 0 {
			trimmed = wrapper.Data
		}
	}
	var value T
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return zero, err
	}
	return value, nil
}
