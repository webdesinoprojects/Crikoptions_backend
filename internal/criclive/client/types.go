package client

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// FlexInt and FlexFloat absorb CricLive's habit of returning the same numeric
// field as a bare number in one payload and a quoted string in the next. A
// decode failure yields the zero value rather than aborting a whole snapshot,
// because a single malformed stat must not blank an otherwise healthy feed.
type FlexInt int

func (i *FlexInt) UnmarshalJSON(data []byte) error {
	*i = FlexInt(flexNumber(data))
	return nil
}

func (i FlexInt) Int() int { return int(i) }

type FlexFloat float64

func (f *FlexFloat) UnmarshalJSON(data []byte) error {
	*f = FlexFloat(flexNumber(data))
	return nil
}

func (f FlexFloat) Float64() float64 { return float64(f) }

// flexNumber accepts a JSON number, a quoted number, or a sentinel such as
// "$undefined" (which CricLive emits for absent values) and returns 0 for
// anything it cannot read as a number.
func flexNumber(data []byte) float64 {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return 0
	}
	if value, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return value
	}
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		text = strings.TrimSpace(text)
		if value, err := strconv.ParseFloat(text, 64); err == nil {
			return value
		}
	}
	return 0
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

// Fixture is the provider-neutral discovery record. Both /cricket/live and
// /cricket/schedule normalize into this single shape so the store keeps one
// fixture-target path instead of one per discovery endpoint.
type Fixture struct {
	ID              int64
	SeriesID        int64
	SeriesName      string
	MatchDesc       string
	Format          string
	MatchType       string
	StartingAt      time.Time
	LocalTeamID     int64
	VisitorTeamID   int64
	LocalTeamName   string
	VisitorTeamName string
	LocalTeamShort  string
	VisitorTeamShort string
	LocalTeamImageID  int64
	VisitorTeamImageID int64
	Venue           string
	Status          string
	StatusDetail    string
	State           string
	Live            bool
	Raw             json.RawMessage
}

// Series is a CricLive competition. It fills the role the league directory
// used to: the unit an operator enables or disables to control what publishes.
// CricLive has no entitlement tiers, so every discovered series is entitled.
type Series struct {
	ID       int64
	Name     string
	Category string
}

// Snapshot is one fully polled match: the commentary miniscore (authoritative
// live state, including who is at the crease), the scorecard, and the recent
// overs. The reducer consumes exactly this and nothing else.
type Snapshot struct {
	MatchID    int64
	Fixture    Fixture
	Commentary CommentaryData
	Scorecard  ScorecardData
	Overs      OversData
	ReceivedAt time.Time
	Raw        json.RawMessage
}

// ---------------------------------------------------------------- /cricket/live

type LiveResponse struct {
	Success  bool            `json:"success"`
	Count    int             `json:"count"`
	Message  string          `json:"message,omitempty"`
	Data     []MatchItem     `json:"data"`
	CachedAt string          `json:"cached_at,omitempty"`
	Raw      json.RawMessage `json:"-"`
}

type MatchItem struct {
	MatchID       int64           `json:"match_id"`
	SeriesID      int64           `json:"series_id"`
	SeriesName    string          `json:"series_name"`
	MatchDesc     string          `json:"match_desc"`
	Title         string          `json:"title"`
	Format        string          `json:"format"`
	MatchType     string          `json:"match_type"`
	Date          string          `json:"date"`
	EndDate       string          `json:"end_date"`
	Venue         string          `json:"venue"`
	VenueTimezone string          `json:"venue_timezone"`
	VenueID       int64           `json:"venue_id"`
	StatusDetail  string          `json:"status_detail"`
	ShortStatus   string          `json:"short_status"`
	State         string          `json:"state"`
	Slug          string          `json:"slug"`
	FirstTeam     TeamItem        `json:"first_team"`
	SecondTeam    TeamItem        `json:"second_team"`
	Status        string          `json:"status"`
	Raw           json.RawMessage `json:"-"`
}

func (m *MatchItem) UnmarshalJSON(data []byte) error {
	type alias MatchItem
	var value alias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*m = MatchItem(value)
	m.Raw = append(m.Raw[:0], data...)
	return nil
}

type TeamItem struct {
	ID       int64         `json:"id"`
	Name     string        `json:"name"`
	FullName string        `json:"full_name"`
	ImageID  int64         `json:"image_id,omitempty"`
	Score    string        `json:"score"`
	Innings  []InningsItem `json:"innings"`
}

type InningsItem struct {
	InningsID     int       `json:"innings_id"`
	Runs          FlexInt   `json:"runs"`
	Wickets       FlexInt   `json:"wickets"`
	Overs         FlexFloat `json:"overs"`
	RunRate       string    `json:"run_rate,omitempty"`
	Target        *int      `json:"target,omitempty"`
	IsDeclared    bool      `json:"is_declared,omitempty"`
	IsFollowingOn bool      `json:"is_following_on,omitempty"`
}

// ------------------------------------------------------------ /cricket/schedule

type ScheduleResponse struct {
	Success bool          `json:"success"`
	Data    []ScheduleDay `json:"data"`
}

type ScheduleDay struct {
	Date     string           `json:"date"`
	LongDate FlexInt          `json:"long_date"`
	Series   []ScheduleSeries `json:"series"`
}

type ScheduleSeries struct {
	SeriesName     string          `json:"series_name"`
	SeriesID       int64           `json:"series_id"`
	SeriesCategory string          `json:"series_category"`
	Matches        []ScheduleMatch `json:"matches"`
}

// ScheduleMatch carries epoch-millisecond timestamps as strings, which is the
// only place CricLive exposes an unambiguous (year-bearing) start time.
type ScheduleMatch struct {
	MatchID     int64  `json:"match_id"`
	MatchDesc   string `json:"match_desc"`
	MatchFormat string `json:"match_format"`
	StartDate   string `json:"start_date"`
	EndDate     string `json:"end_date"`
	Team1       string `json:"team1"`
	Team1Short  string `json:"team1_short"`
	Team1ID     int64  `json:"team1_id"`
	Team2       string `json:"team2"`
	Team2Short  string `json:"team2_short"`
	Team2ID     int64  `json:"team2_id"`
	Ground      string `json:"ground"`
	City        string `json:"city"`
	Country     string `json:"country"`
	Timezone    string `json:"timezone"`
}

// ---------------------------------------------------------- /cricket/commentary

type CommentaryResponse struct {
	Success bool            `json:"success"`
	Data    CommentaryData  `json:"data"`
	Raw     json.RawMessage `json:"-"`
}

type CommentaryData struct {
	MatchID        int64            `json:"match_id"`
	MiniScore      MiniScore        `json:"miniscore"`
	MatchHeader    MatchHeader      `json:"match_header"`
	WinProbability *WinProbability  `json:"win_probability,omitempty"`
	Commentary     []CommentaryItem `json:"commentary"`
}

// MiniScore is the authoritative live state. striker/non_striker/bowler_striker
// carry the on-field player names the trading UI renders; they are the reason
// this endpoint is polled every cycle rather than the scorecard.
type MiniScore struct {
	InningsID      int           `json:"innings_id"`
	BatTeamID      int64         `json:"bat_team_id"`
	BatTeamScore   FlexInt       `json:"bat_team_score"`
	BatTeamWickets FlexInt       `json:"bat_team_wickets"`
	Status         string        `json:"status"`
	Overs          FlexFloat     `json:"overs"`
	Target         *int          `json:"target,omitempty"`
	CurrentRunRate FlexFloat     `json:"crr"`
	RequiredRate   FlexFloat     `json:"rrr"`
	RecentOvers    string        `json:"recent_overs"`
	LastWicket     string        `json:"last_wicket"`
	Last10Overs    PhaseSummary  `json:"last_10_overs"`
	Partnership    Partnership   `json:"partnership"`
	Striker        BattingLine   `json:"striker"`
	NonStriker     BattingLine   `json:"non_striker"`
	BowlerStriker  BowlingLine   `json:"bowler_striker"`
	BowlerNonStrik BowlingLine   `json:"bowler_non_striker"`
	InningsScores  []InningsScore `json:"innings_scores"`
	MatchFormat    string        `json:"match_format"`
	CustomStatus   string        `json:"custom_status"`
	State          string        `json:"state"`
}

type PhaseSummary struct {
	Runs    FlexInt `json:"runs"`
	Wickets FlexInt `json:"wickets"`
	Overs   string  `json:"overs"`
}

type Partnership struct {
	Runs  FlexInt `json:"runs"`
	Balls FlexInt `json:"balls"`
}

type BattingLine struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Runs       FlexInt   `json:"runs"`
	Balls      FlexInt   `json:"balls"`
	Fours      FlexInt   `json:"fours"`
	Sixes      FlexInt   `json:"sixes"`
	StrikeRate FlexFloat `json:"strike_rate"`
	URL        string    `json:"url,omitempty"`
}

type BowlingLine struct {
	ID      int64     `json:"id"`
	Name    string    `json:"name"`
	Overs   FlexFloat `json:"overs"`
	Maidens FlexInt   `json:"maidens"`
	Runs    FlexInt   `json:"runs"`
	Wickets FlexInt   `json:"wickets"`
	Economy FlexFloat `json:"economy"`
	URL     string    `json:"url,omitempty"`
}

type InningsScore struct {
	InningsID  int       `json:"innings_id"`
	BatTeam    string    `json:"bat_team"`
	Score      FlexInt   `json:"score"`
	Wickets    FlexInt   `json:"wickets"`
	Overs      FlexFloat `json:"overs"`
	IsDeclared bool      `json:"is_declared"`
	IsFollowOn bool      `json:"is_follow_on"`
}

type MatchHeader struct {
	MatchID        int64      `json:"match_id"`
	Description    string     `json:"description"`
	Format         string     `json:"format"`
	Status         string     `json:"status"`
	State          string     `json:"state"`
	StartTimeGMT   string     `json:"start_time_gmt"`
	StartTimeIST   string     `json:"start_time_ist"`
	StartTimeLocal string     `json:"start_time_local"`
	TossWinner     string     `json:"toss_winner"`
	TossDecision   string     `json:"toss_decision"`
	Team1          HeaderTeam `json:"team1"`
	Team2          HeaderTeam `json:"team2"`
	SeriesName     string     `json:"series_name"`
	Venue          string     `json:"venue"`
}

type HeaderTeam struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Short string `json:"short"`
}

type WinProbability struct {
	Team1            HeaderTeamOdds `json:"team1"`
	Team2            HeaderTeamOdds `json:"team2"`
	DrawTiePercent   FlexFloat      `json:"draw_tie_percent"`
	LastUpdatedOver  FlexFloat      `json:"last_updated_over"`
}

type HeaderTeamOdds struct {
	ID      int64     `json:"id"`
	Name    string    `json:"name"`
	Short   string    `json:"short"`
	Percent FlexFloat `json:"percent"`
}

// CommentaryItem text may contain HTML and `$NN` placeholder tokens; it is
// display data only. BallMetric is nil on non-delivery entries (analysis,
// session summaries), which is how deliveries are told apart from prose.
type CommentaryItem struct {
	Timestamp     int64        `json:"timestamp"`
	Text          string       `json:"text"`
	BallMetric    *FlexFloat   `json:"ball_metric"`
	InningsID     *int         `json:"innings_id"`
	OverSeparator *OverSummary `json:"over_separator,omitempty"`
}

type OverSummary struct {
	OverNumber  FlexInt `json:"over_number"`
	OverSummary string  `json:"over_summary"`
	OverRuns    FlexInt `json:"over_runs"`
}

// ----------------------------------------------------------- /cricket/scorecard

type ScorecardResponse struct {
	Success bool            `json:"success"`
	Data    ScorecardData   `json:"data"`
	Raw     json.RawMessage `json:"-"`
}

type ScorecardData struct {
	MatchID     int64              `json:"match_id"`
	Innings     []ScorecardInnings `json:"innings"`
	MatchHeader MatchHeader        `json:"match_header"`
}

type ScorecardInnings struct {
	InningsID     int              `json:"innings_id"`
	BatTeam       string           `json:"bat_team"`
	BatTeamShort  string           `json:"bat_team_short"`
	BowlTeam      string           `json:"bowl_team"`
	Score         string           `json:"score"`
	RunRate       FlexFloat        `json:"run_rate"`
	Batsmen       []ScorecardBatsman `json:"batsmen"`
	YetToBat      []string         `json:"yet_to_bat"`
	Bowlers       []ScorecardBowler `json:"bowlers"`
	Extras        string           `json:"extras"`
	ExtrasDetail  ExtrasDetail     `json:"extras_detail"`
	FallOfWickets []FallOfWicket   `json:"fall_of_wickets"`
	Partnerships  []ScorePartnership `json:"partnerships"`
	IsDeclared    bool             `json:"is_declared"`
	IsFollowingOn bool             `json:"is_following_on"`
}

type ScorecardBatsman struct {
	Name       string    `json:"name"`
	ShortName  string    `json:"short_name"`
	IsCaptain  bool      `json:"is_captain"`
	IsKeeper   bool      `json:"is_keeper"`
	Runs       FlexInt   `json:"runs"`
	Balls      FlexInt   `json:"balls"`
	Fours      FlexInt   `json:"fours"`
	Sixes      FlexInt   `json:"sixes"`
	StrikeRate FlexFloat `json:"strike_rate"`
	OutDesc    string    `json:"out_desc"`
	WicketCode string    `json:"wicket_code"`
	Mins       FlexInt   `json:"mins"`
	Dots       FlexInt   `json:"dots"`
}

type ScorecardBowler struct {
	Name      string    `json:"name"`
	ShortName string    `json:"short_name"`
	IsCaptain bool      `json:"is_captain"`
	Overs     FlexFloat `json:"overs"`
	Maidens   FlexInt   `json:"maidens"`
	Runs      FlexInt   `json:"runs"`
	Wickets   FlexInt   `json:"wickets"`
	NoBalls   FlexInt   `json:"no_balls"`
	Wides     FlexInt   `json:"wides"`
	Economy   FlexFloat `json:"economy"`
	Dots      FlexInt   `json:"dots"`
}

type ExtrasDetail struct {
	Total   FlexInt `json:"total"`
	Byes    FlexInt `json:"byes"`
	LegByes FlexInt `json:"leg_byes"`
	Wides   FlexInt `json:"wides"`
	NoBalls FlexInt `json:"no_balls"`
	Penalty FlexInt `json:"penalty"`
}

type FallOfWicket struct {
	WktNum FlexInt   `json:"wkt_num"`
	Player string    `json:"player"`
	Runs   FlexInt   `json:"runs"`
	Over   FlexFloat `json:"over"`
}

type ScorePartnership struct {
	Bat1Name   string  `json:"bat1_name"`
	Bat1Runs   FlexInt `json:"bat1_runs"`
	Bat2Name   string  `json:"bat2_name"`
	Bat2Runs   FlexInt `json:"bat2_runs"`
	TotalRuns  FlexInt `json:"total_runs"`
	TotalBalls FlexInt `json:"total_balls"`
}

// --------------------------------------------------------------- /cricket/overs

type OversResponse struct {
	Success bool            `json:"success"`
	Data    OversData       `json:"data"`
	Raw     json.RawMessage `json:"-"`
}

type OversData struct {
	MatchID     string      `json:"matchId"`
	Innings     int         `json:"innings"`
	FiltersList []OverFilter `json:"filtersList"`
	Overs       []OverItem  `json:"overs"`
}

type OverFilter struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// OverItem.Balls holds one entry per delivery bowled in the over, including
// extras, as display tokens ("0", "4", "1w", "W").
type OverItem struct {
	InningsID          int       `json:"inningsId"`
	OverNumber         FlexFloat `json:"overNumber"`
	Runs               FlexInt   `json:"runs"`
	Score              FlexInt   `json:"score"`
	Wickets            FlexInt   `json:"wickets"`
	OverSummary        string    `json:"ovrSummary"`
	Balls              []string  `json:"balls"`
	BatTeamName        string    `json:"batTeamName"`
	BatStrikerNames    []string  `json:"batStrikerNames"`
	BatStrikerRuns     FlexInt   `json:"batStrikerRuns"`
	BatStrikerBalls    FlexInt   `json:"batStrikerBalls"`
	BatNonStrikerNames []string  `json:"batNonStrikerNames"`
	BatNonStrikerRuns  FlexInt   `json:"batNonStrikerRuns"`
	BatNonStrikerBalls FlexInt   `json:"batNonStrikerBalls"`
	BowlNames          []string  `json:"bowlNames"`
	BowlOvers          FlexFloat `json:"bowlOvers"`
	BowlMaidens        FlexInt   `json:"bowlMaidens"`
	BowlRuns           FlexInt   `json:"bowlRuns"`
	BowlWickets        FlexInt   `json:"bowlWickets"`
}

// --------------------------------------------------------------- /cricket/squads

type SquadsResponse struct {
	Success bool        `json:"success"`
	Data    []SquadTeam `json:"data"`
}

type SquadTeam struct {
	TeamID       int64        `json:"team_id"`
	TeamName     string       `json:"team_name"`
	TeamShort    string       `json:"team_short"`
	PlayingXI    []SquadPlayer `json:"playing_xi"`
	Bench        []SquadPlayer `json:"bench"`
	SupportStaff []SquadPlayer `json:"support_staff"`
}

type SquadPlayer struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	ShortName      string `json:"short_name"`
	Role           string `json:"role"`
	BattingStyle   string `json:"batting_style"`
	BowlingStyle   string `json:"bowling_style"`
	IsCaptain      bool   `json:"is_captain"`
	IsKeeper       bool   `json:"is_keeper"`
	ProfileURL     string `json:"profile_url"`
	ImageID        string `json:"image_id"`
	PlayerImageURL string `json:"player_image_url"`
}
