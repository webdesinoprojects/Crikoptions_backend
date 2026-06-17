package matches

import "time"

type MatchResponse struct {
	ID           string    `json:"_id"`
	TournamentID string    `json:"tournamentId"`
	Format       string    `json:"format"`
	TeamAID      string    `json:"teamAId"`
	TeamBID      string    `json:"teamBId"`
	TeamAName    string    `json:"teamAName"`
	TeamBName    string    `json:"teamBName"`
	TeamALogo    string    `json:"teamALogo"`
	TeamBLogo    string    `json:"teamBLogo"`
	StartTime    time.Time `json:"startTime"`
	Status       string    `json:"status"`
	Innings      int       `json:"innings"`
	CurrentScore int       `json:"currentScore"`
	WicketsLost  int       `json:"wicketsLost"`
	BallsLeft    int       `json:"ballsLeft"`
	TargetScore  int       `json:"targetScore,omitempty"`
	OversText    string    `json:"oversText"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
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

// BallEventRequest records one legal ball for "This over" commentary.
type BallEventRequest struct {
	Runs        int    `json:"runs"`
	IsWicket    bool   `json:"isWicket"`
	BallNumber  int    `json:"ballNumber,omitempty"`
	Description string `json:"description,omitempty"`
}
