package matches

import "time"

type MatchResponse struct {
	ID           string            `json:"_id"`
	TournamentID string            `json:"tournamentId"`
	Format       string            `json:"format"`
	TeamAID      string            `json:"teamAId"`
	TeamBID      string            `json:"teamBId"`
	TeamAName    string            `json:"teamAName"`
	TeamBName    string            `json:"teamBName"`
	TeamALogo    string            `json:"teamALogo"`
	TeamBLogo    string            `json:"teamBLogo"`
	StartTime    time.Time         `json:"startTime"`
	Status       string            `json:"status"`
	Innings      int               `json:"innings"`
	CurrentScore int               `json:"currentScore"`
	WicketsLost  int               `json:"wicketsLost"`
	BallsLeft    int               `json:"ballsLeft"`
	TargetScore  int               `json:"targetScore,omitempty"`
	OversText    string            `json:"oversText"`
	LiveContext  *LiveMatchContext `json:"liveContext,omitempty"`
	CreatedAt    time.Time         `json:"createdAt"`
	UpdatedAt    time.Time         `json:"updatedAt"`
}

type CreateMatchRequest struct {
	TournamentID string    `json:"tournamentId"`
	Format       string    `json:"format"`
	TeamAID      string    `json:"teamAId"`
	TeamBID      string    `json:"teamBId"`
	TeamAName    string    `json:"teamAName"`
	TeamBName    string    `json:"teamBName"`
	TeamALogo    string    `json:"teamALogo"`
	TeamBLogo    string    `json:"teamBLogo"`
	StartTime    time.Time `json:"startTime"`
}

type UpdateScoreRequest struct {
	Innings      int    `json:"innings"`
	CurrentScore int    `json:"currentScore"`
	WicketsLost  int    `json:"wicketsLost"`
	BallsLeft    int    `json:"ballsLeft"`
	TargetScore  *int   `json:"targetScore,omitempty"`
	Status       string `json:"status"`
}

type UpdateLiveContextRequest struct {
	Striker     BatterStats      `json:"striker"`
	NonStriker  BatterStats      `json:"nonStriker"`
	Bowler      BowlerStats      `json:"bowler"`
	Partnership PartnershipStats `json:"partnership"`
}

// BallEventRequest records one delivery for "This over" commentary.
// Extra is null/"" for a legal delivery, or "wide"/"noball" for an illegal one
// (which does not consume a legal ball).
type BallEventRequest struct {
	Runs           int    `json:"runs"`
	IsWicket       bool   `json:"isWicket"`
	Extra          string `json:"extra,omitempty"`
	BallNumber     int    `json:"ballNumber,omitempty"`
	Description    string `json:"description,omitempty"`
	NextBatterName string `json:"nextBatterName,omitempty"`
}

// BallEventResponse is one item in GET /api/v1/matches/{id}/events.
type BallEventResponse struct {
	Innings  int     `json:"innings"`
	Over     int     `json:"over"`
	Ball     int     `json:"ball"`
	Runs     int     `json:"runs"`
	IsWicket bool    `json:"isWicket"`
	Extra    *string `json:"extra"`
}
