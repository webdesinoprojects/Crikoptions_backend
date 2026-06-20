package matches

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Match struct {
	ID           primitive.ObjectID `json:"_id" bson:"_id,omitempty"`
	TournamentID string             `json:"tournamentId" bson:"tournamentId"`
	Format       string             `json:"format" bson:"format"`
	TeamAID      string             `json:"teamAId" bson:"teamAId"`
	TeamBID      string             `json:"teamBId" bson:"teamBId"`
	TeamAName    string             `json:"teamAName" bson:"teamAName"`
	TeamBName    string             `json:"teamBName" bson:"teamBName"`
	TeamALogo    string             `json:"teamALogo" bson:"teamALogo"`
	TeamBLogo    string             `json:"teamBLogo" bson:"teamBLogo"`
	StartTime    time.Time          `json:"startTime" bson:"startTime"`
	Status       string             `json:"status" bson:"status"`
	Innings      int                `json:"innings" bson:"innings"`
	CurrentScore int                `json:"currentScore" bson:"currentScore"`
	WicketsLost  int                `json:"wicketsLost" bson:"wicketsLost"`
	BallsLeft    int                `json:"ballsLeft" bson:"ballsLeft"`
	TargetScore  int                `json:"targetScore,omitempty" bson:"targetScore,omitempty"`
	OversText    string             `json:"oversText" bson:"oversText"`
	LiveContext  *LiveMatchContext  `json:"liveContext,omitempty" bson:"liveContext,omitempty"`
	CreatedAt    time.Time          `json:"createdAt" bson:"createdAt"`
	UpdatedAt    time.Time          `json:"updatedAt" bson:"updatedAt"`
}

type ScoreUpdate struct {
	Innings      int               `json:"innings"`
	CurrentScore int               `json:"currentScore"`
	WicketsLost  int               `json:"wicketsLost"`
	BallsLeft    int               `json:"ballsLeft"`
	TargetScore  int               `json:"targetScore,omitempty"`
	Status       string            `json:"status"`
	LiveContext  *LiveMatchContext `json:"-"`
}

type BatterStats struct {
	Name  string `json:"name" bson:"name"`
	Runs  int    `json:"runs" bson:"runs"`
	Balls int    `json:"balls" bson:"balls"`
}

type BowlerStats struct {
	Name            string `json:"name" bson:"name"`
	Balls           int    `json:"balls" bson:"balls"`
	Maidens         int    `json:"maidens" bson:"maidens"`
	Runs            int    `json:"runs" bson:"runs"`
	Wickets         int    `json:"wickets" bson:"wickets"`
	CurrentOverRuns int    `json:"currentOverRuns,omitempty" bson:"currentOverRuns,omitempty"`
}

type PartnershipStats struct {
	Runs  int `json:"runs" bson:"runs"`
	Balls int `json:"balls" bson:"balls"`
}

type LiveMatchContext struct {
	Striker     BatterStats      `json:"striker" bson:"striker"`
	NonStriker  BatterStats      `json:"nonStriker" bson:"nonStriker"`
	Bowler      BowlerStats      `json:"bowler" bson:"bowler"`
	Partnership PartnershipStats `json:"partnership" bson:"partnership"`
}
