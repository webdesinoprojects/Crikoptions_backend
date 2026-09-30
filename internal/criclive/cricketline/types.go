package cricketline

import (
	"encoding/json"
	"strings"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
)

// Payload shapes, verified against live CricketLineApi responses (September
// 2026). Numbers arrive as strings on some endpoints and bare numbers on
// others, so counters use client.FlexInt/FlexFloat.
//
// Data-quality notes that shape the adapter:
//   - On /api/live and /api/match/{id}/live a side's short_name always travels
//     with its score, but team_id and flag (and on /api/live, name) can belong
//     to the other side. Scores are attributed by label, ids only break ties.
//   - /api/live's on_strike flag can lag a ball; the striker is tracked from
//     balls faced instead (strike.go).
//   - Innings scores read "148-4", "160/5", "(20.0)" or "Yet To Bat".

type seriesRef struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ShortName string `json:"short_name"`
	Image     string `json:"image"`
}

type teamRef struct {
	Name      string `json:"name"`
	ShortName string `json:"short_name"`
	TeamID    string `json:"team_id"`
	Flag      string `json:"flag"`
	Score     string `json:"score"`
	Overs     string `json:"overs"`
}

// ------------------------------------------------------------------ /api/live

type liveListResponse struct {
	Status  string      `json:"status"`
	Count   int         `json:"count"`
	Matches []liveMatch `json:"matches"`
}

type liveMatch struct {
	MatchID        string          `json:"match_id"`
	MatchName      string          `json:"match_name"`
	MatchNo        string          `json:"match_no"`
	Format         string          `json:"format"`
	Subtitle       string          `json:"subtitle"`
	Status         string          `json:"status"`
	StatusText     string          `json:"status_text"`
	Venue          string          `json:"venue"`
	Series         seriesRef       `json:"series"`
	Team1          teamRef         `json:"team1"`
	Team2          teamRef         `json:"team2"`
	CurrentBatters []liveBatter    `json:"current_batters"`
	LastOver       []liveLastOver  `json:"last_over"`
	Raw            json.RawMessage `json:"-"`
}

type liveBatter struct {
	Name     string         `json:"name"`
	Runs     client.FlexInt `json:"runs"`
	Balls    string         `json:"balls"` // "(7)"
	OnStrike bool           `json:"on_strike"`
}

type liveLastOver struct {
	Over     string         `json:"over"`
	OverInfo []string       `json:"overinfo"`
	Total    client.FlexInt `json:"total"`
}

// ------------------------------------------------------- /api/schedule/range

type scheduleRangeResponse struct {
	Status string                 `json:"status"`
	ByDate map[string]scheduleDay `json:"by_date"`
}

type scheduleDay struct {
	Date    string          `json:"date"`
	Matches []scheduleMatch `json:"matches"`
}

type scheduleMatch struct {
	MatchID    string           `json:"match_id"`
	MatchNo    string           `json:"match_no"`
	Format     string           `json:"format"`
	Subtitle   string           `json:"subtitle"`
	Status     string           `json:"status"`
	StatusText string           `json:"status_text"`
	Result     string           `json:"result"`
	StartTS    client.FlexFloat `json:"start_ts"` // epoch milliseconds
	Venue      string           `json:"venue"`
	Series     seriesRef        `json:"series"`
	Team1      teamRef          `json:"team1"`
	Team2      teamRef          `json:"team2"`
}

// --------------------------------------------------- /api/match/{id}/live

type matchLiveResponse struct {
	Status         string           `json:"status"`
	MatchID        string           `json:"match_id"`
	LiveStatus     string           `json:"livestatus"`
	Title          string           `json:"title"`
	Format         string           `json:"format"`
	StatusText     string           `json:"status_text"`
	BattingTeam    teamRef          `json:"batting_team"`
	BowlingTeam    teamRef          `json:"bowling_team"`
	CRR            string           `json:"crr"`
	RRR            string           `json:"rrr"`
	Batsmen        []tabBatter      `json:"batsmen"`
	Bowler         tabBowler        `json:"bowler"`
	Partnership    tabPartnership   `json:"partnership"`
	LastWicket     tabLastWicket    `json:"last_wicket"`
	RecentOvers    []tabRecentOver  `json:"recent_overs"`
	Probability    tabProbability   `json:"probability"`
	ProjectedScore tabProjectedGrid `json:"projected_score"`
}

type tabBatter struct {
	PlayerID   string           `json:"player_id"`
	Name       string           `json:"name"`
	FullName   string           `json:"full_name"`
	Runs       client.FlexInt   `json:"runs"`
	Balls      client.FlexInt   `json:"balls"`
	Fours      client.FlexInt   `json:"fours"`
	Sixes      client.FlexInt   `json:"sixes"`
	StrikeRate client.FlexFloat `json:"strike_rate"`
}

type tabBowler struct {
	Name    string           `json:"name"`
	Figures string           `json:"figures"` // "1-23": wickets-runs
	Overs   client.FlexFloat `json:"overs"`
	Economy client.FlexFloat `json:"economy"`
}

type tabPartnership struct {
	Runs  client.FlexInt `json:"runs"`
	Balls client.FlexInt `json:"balls"`
}

type tabLastWicket struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

type tabRecentOver struct {
	Over  string         `json:"over"` // "Over 19"
	Balls []string       `json:"balls"`
	Runs  client.FlexInt `json:"runs"`
}

type tabProbability struct {
	BattingTeam tabTeamPercent `json:"batting_team"`
	BowlingTeam tabTeamPercent `json:"bowling_team"`
}

type tabTeamPercent struct {
	ShortName string           `json:"short_name"`
	Percent   client.FlexFloat `json:"percent"`
}

// tabProjectedGrid rows are keyed by run rate; only "over" is read, and its
// largest value is the innings length the match is scheduled for.
type tabProjectedGrid struct {
	Available bool                         `json:"available"`
	Rows      []map[string]json.RawMessage `json:"rows"`
}

// scheduledOvers is the innings length the projection grid is drawn to, or 0
// when the grid is absent.
func (g tabProjectedGrid) scheduledOvers() int {
	if !g.Available {
		return 0
	}
	best := 0
	for _, row := range g.Rows {
		raw, ok := row["over"]
		if !ok {
			continue
		}
		var value client.FlexInt
		if err := json.Unmarshal(raw, &value); err == nil && value.Int() > best {
			best = value.Int()
		}
	}
	return best
}

// errorEnvelope covers both error shapes the API returns:
// {"status":"error","code":"...","message":"..."} and
// {"detail":{"status":"error","message":"..."}}.
type errorEnvelope struct {
	Status  string          `json:"status"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Detail  json.RawMessage `json:"detail"`
}

func (e errorEnvelope) text() (message, code string) {
	message, code = strings.TrimSpace(e.Message), strings.TrimSpace(e.Code)
	if len(e.Detail) == 0 {
		return message, code
	}
	var detail struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(e.Detail, &detail) == nil {
		if message == "" {
			message = strings.TrimSpace(detail.Message)
		}
		if code == "" {
			code = strings.TrimSpace(detail.Code)
		}
		return message, code
	}
	var plain string
	if json.Unmarshal(e.Detail, &plain) == nil && message == "" {
		message = strings.TrimSpace(plain)
	}
	return message, code
}
