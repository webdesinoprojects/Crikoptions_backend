package matches

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Match struct {
	ID primitive.ObjectID `json:"_id" bson:"_id,omitempty"`
	// DataSource owns score mutations for this match. Existing records without
	// this field are treated as manual by callers for backwards compatibility.
	DataSource string `json:"dataSource,omitempty" bson:"dataSource,omitempty"`
	Provider   string `json:"provider,omitempty" bson:"provider,omitempty"`
	Hidden     bool   `json:"-" bson:"hidden,omitempty"`

	// Provider identifiers deliberately live alongside the local identifiers:
	// provider IDs are stable ingestion keys, while local IDs remain the public
	// API references used by the rest of the application.
	ProviderFixtureID int64             `json:"providerFixtureId,omitempty" bson:"providerFixtureId,omitempty"`
	ProviderLeagueID  int64             `json:"providerLeagueId,omitempty" bson:"providerLeagueId,omitempty"`
	ProviderSeasonID  int64             `json:"providerSeasonId,omitempty" bson:"providerSeasonId,omitempty"`
	ProviderTeamAID   int64             `json:"providerTeamAId,omitempty" bson:"providerTeamAId,omitempty"`
	ProviderTeamBID   int64             `json:"providerTeamBId,omitempty" bson:"providerTeamBId,omitempty"`
	TournamentID      string            `json:"tournamentId" bson:"tournamentId"`
	Format            string            `json:"format" bson:"format"`
	TeamAID           string            `json:"teamAId" bson:"teamAId"`
	TeamBID           string            `json:"teamBId" bson:"teamBId"`
	TeamAName         string            `json:"teamAName" bson:"teamAName"`
	TeamBName         string            `json:"teamBName" bson:"teamBName"`
	TeamALogo         string            `json:"teamALogo" bson:"teamALogo"`
	TeamBLogo         string            `json:"teamBLogo" bson:"teamBLogo"`
	StartTime         time.Time         `json:"startTime" bson:"startTime"`
	Status            string            `json:"status" bson:"status"`
	Innings           int               `json:"innings" bson:"innings"`
	CurrentScore      int               `json:"currentScore" bson:"currentScore"`
	WicketsLost       int               `json:"wicketsLost" bson:"wicketsLost"`
	BallsLeft         int               `json:"ballsLeft" bson:"ballsLeft"`
	TargetScore       int               `json:"targetScore,omitempty" bson:"targetScore,omitempty"`
	OversText         string            `json:"oversText" bson:"oversText"`
	LiveContext       *LiveMatchContext `json:"liveContext,omitempty" bson:"liveContext,omitempty"`

	ProviderPhase         string           `json:"providerPhase,omitempty" bson:"providerPhase,omitempty"`
	ScheduledBalls        int              `json:"scheduledBalls,omitempty" bson:"scheduledBalls,omitempty"`
	ProviderBattingTeamID int64            `json:"providerBattingTeamId,omitempty" bson:"providerBattingTeamId,omitempty"`
	InningsSummaries      []InningsSummary `json:"inningsSummaries,omitempty" bson:"inningsSummaries,omitempty"`

	// StateVersion versions authoritative score/event state. TradingVersion is
	// advanced whenever tradability changes, allowing order creation to fence
	// against a concurrent feed suspension.
	StateVersion         int64      `json:"stateVersion" bson:"stateVersion"`
	TradingVersion       int64      `json:"tradingVersion" bson:"tradingVersion"`
	FeedState            string     `json:"feedState,omitempty" bson:"feedState,omitempty"`
	TradingState         string     `json:"tradingState,omitempty" bson:"tradingState,omitempty"`
	TradingBlockers      []string   `json:"tradingBlockers,omitempty" bson:"tradingBlockers,omitempty"`
	TradingGateCheckedAt *time.Time `json:"-" bson:"tradingGateCheckedAt,omitempty"`
	GateCheckSeq         int64      `json:"-" bson:"gateCheckSeq,omitempty"`

	LastProviderUpdateAt *time.Time      `json:"lastProviderUpdateAt,omitempty" bson:"lastProviderUpdateAt,omitempty"`
	LastFeedReceivedAt   *time.Time      `json:"lastFeedReceivedAt,omitempty" bson:"lastFeedReceivedAt,omitempty"`
	LastSuccessfulPollAt *time.Time      `json:"lastSuccessfulPollAt,omitempty" bson:"lastSuccessfulPollAt,omitempty"`
	HealthySnapshotCount int             `json:"-" bson:"healthySnapshotCount,omitempty"`
	LastSnapshotHash     string          `json:"-" bson:"lastSnapshotHash,omitempty"`
	LastStateChangeAt    *time.Time      `json:"-" bson:"lastStateChangeAt,omitempty"`
	FeedValidUntil       *time.Time      `json:"feedValidUntil,omitempty" bson:"feedValidUntil,omitempty"`
	FinalCandidate       *FinalCandidate `json:"finalCandidate,omitempty" bson:"finalCandidate,omitempty"`
	CreatedAt            time.Time       `json:"createdAt" bson:"createdAt"`
	UpdatedAt            time.Time       `json:"updatedAt" bson:"updatedAt"`
}

const (
	DataSourceManual     = "manual"
	DataSourceSimulator  = "simulator"
	DataSourceSportmonks = "sportmonks"

	FeedStateWarming      = "warming"
	FeedStateHealthy      = "healthy"
	FeedStateReconciling  = "reconciling"
	FeedStateStale        = "stale"
	FeedStateQuotaLimited = "quota_limited"
	FeedStateFinalizing   = "finalizing"
	FeedStateTerminal     = "terminal"
	FeedStateUnsupported  = "unsupported"
)

// InningsSummary is the compact authoritative projection retained when the
// live aggregate moves to another innings.
type InningsSummary struct {
	Innings          int             `json:"innings" bson:"innings"`
	BattingTeamID    int64           `json:"battingTeamId,omitempty" bson:"battingTeamId,omitempty"`
	Runs             int             `json:"runs" bson:"runs"`
	Wickets          int             `json:"wickets" bson:"wickets"`
	LegalBalls       int             `json:"legalBalls" bson:"legalBalls"`
	ScheduledBalls   int             `json:"scheduledBalls,omitempty" bson:"scheduledBalls,omitempty"`
	Target           int             `json:"target,omitempty" bson:"target,omitempty"`
	Complete         bool            `json:"complete" bson:"complete"`
	Revision         int64           `json:"revision" bson:"revision"`
	FinalCandidate   *FinalCandidate `json:"finalCandidate,omitempty" bson:"finalCandidate,omitempty"`
	SettlementReady  bool            `json:"settlementReady,omitempty" bson:"settlementReady,omitempty"`
	FinalDisposition string          `json:"finalDisposition,omitempty" bson:"finalDisposition,omitempty"`
}

const (
	FinalDispositionSettle = "settle"
	FinalDispositionVoid   = "void"
)

// FinalCandidate records the exact projection revision being held before
// settlement. Any provider correction replaces it and restarts the hold.
type FinalCandidate struct {
	Revision       int64     `json:"revision" bson:"revision"`
	SnapshotHash   string    `json:"snapshotHash" bson:"snapshotHash"`
	IdenticalPolls int       `json:"identicalPolls" bson:"identicalPolls"`
	FirstSeenAt    time.Time `json:"firstSeenAt" bson:"firstSeenAt"`
	LastSeenAt     time.Time `json:"lastSeenAt" bson:"lastSeenAt"`
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
