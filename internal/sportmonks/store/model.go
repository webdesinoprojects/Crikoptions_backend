package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/reconcile"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const ProviderName = matches.DataSourceSportmonks

type League struct {
	ID         int64     `bson:"_id" json:"id"`
	Name       string    `bson:"name" json:"name"`
	Code       string    `bson:"code,omitempty" json:"code,omitempty"`
	Entitled   bool      `bson:"entitled" json:"entitled"`
	Enabled    bool      `bson:"enabled" json:"enabled"`
	LastSeenAt time.Time `bson:"lastSeenAt" json:"lastSeenAt"`
	UpdatedAt  time.Time `bson:"updatedAt" json:"updatedAt"`
}

type Status struct {
	EnabledLeagues       int64            `json:"enabledLeagues"`
	EligibleFixtures     int64            `json:"eligibleFixtures"`
	LeasedFixtures       int64            `json:"leasedFixtures"`
	PendingSettlements   int64            `json:"pendingSettlements"`
	PendingCancellations int64            `json:"pendingCancellations"`
	FeedStates           map[string]int64 `json:"feedStates"`
	LastSuccessfulPoll   *time.Time       `json:"lastSuccessfulPoll,omitempty"`
	GlobalTradingKilled  bool             `json:"globalTradingKilled"`
	RequestsThisHour     map[string]int   `json:"requestsThisHour"`
}

type PayloadDiagnostic struct {
	Valid      bool            `json:"valid"`
	Error      string          `json:"error,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"`
	ReceivedAt time.Time       `json:"receivedAt"`
}

type FixtureDiagnostics struct {
	Target        FixtureTarget          `json:"target"`
	Match         *matches.Match         `json:"match,omitempty"`
	Shadow        *ShadowProjection      `json:"shadow,omitempty"`
	Reports       []ReconciliationReport `json:"reports"`
	LatestPayload *PayloadDiagnostic     `json:"latestPayload,omitempty"`
}

type TradingControl struct {
	Killed    bool      `bson:"killed" json:"killed"`
	UpdatedAt time.Time `bson:"updatedAt" json:"updatedAt"`
}

type FixtureTarget struct {
	ID                  int64      `bson:"_id"`
	LeagueID            int64      `bson:"leagueId"`
	SeasonID            int64      `bson:"seasonId"`
	LocalTeamID         int64      `bson:"localTeamId"`
	VisitorTeamID       int64      `bson:"visitorTeamId"`
	Format              string     `bson:"format"`
	ScheduledBalls      int        `bson:"scheduledBalls"`
	ProviderStatus      string     `bson:"providerStatus"`
	StartTime           time.Time  `bson:"startTime"`
	Eligible            bool       `bson:"eligible"`
	Supported           bool       `bson:"supported"`
	NextPollAt          time.Time  `bson:"nextPollAt"`
	LastPollAt          *time.Time `bson:"lastPollAt,omitempty"`
	LastSuccessAt       *time.Time `bson:"lastSuccessAt,omitempty"`
	LastSuccessMode     string     `bson:"lastSuccessMode,omitempty"`
	LastSnapshotHash    string     `bson:"lastSnapshotHash,omitempty"`
	LastError           string     `bson:"lastError,omitempty"`
	ConsecutiveFailures int        `bson:"consecutiveFailures,omitempty"`
	LeaseOwner          string     `bson:"leaseOwner,omitempty"`
	LeaseToken          string     `bson:"leaseToken,omitempty"`
	LeaseUntil          *time.Time `bson:"leaseUntil,omitempty"`
	CreatedAt           time.Time  `bson:"createdAt"`
	UpdatedAt           time.Time  `bson:"updatedAt"`
}

type ApplyOptions struct {
	Mode                    string
	LeaseOwner              string
	LeaseToken              string
	AllowCorrections        bool
	AllowMidMatchAdmission  bool
	InningsFinalizationHold time.Duration
	MatchFinalizationHold   time.Duration
	FeedValidity            time.Duration
	RawPayloadTTL           time.Duration
}

type ApplyResult struct {
	MatchID         string
	StateVersion    int64
	TradingVersion  int64
	FeedState       string
	Applied         bool
	CorrectionCount int
	TombstoneCount  int
	Reconciling     bool
}

type EventRevision struct {
	ID                primitive.ObjectID `bson:"_id,omitempty"`
	Provider          string             `bson:"provider"`
	ProviderFixtureID int64              `bson:"providerFixtureId"`
	ProviderEventID   string             `bson:"providerEventId"`
	Revision          int64              `bson:"revision"`
	Event             matches.BallEvent  `bson:"event"`
	ObservedAt        time.Time          `bson:"observedAt"`
}

type SettlementJob struct {
	ID             string     `bson:"_id" json:"id"`
	MatchID        string     `bson:"matchId" json:"matchId"`
	Innings        int        `bson:"innings" json:"innings"`
	FinalScore     int        `bson:"finalScore" json:"finalScore"`
	FinalRevision  int64      `bson:"finalRevision" json:"finalRevision"`
	SnapshotHash   string     `bson:"snapshotHash" json:"snapshotHash"`
	FormulaVersion string     `bson:"formulaVersion" json:"formulaVersion"`
	Action         string     `bson:"action,omitempty" json:"action,omitempty"`
	Status         string     `bson:"status" json:"status"`
	Attempts       int        `bson:"attempts" json:"attempts"`
	LeaseOwner     string     `bson:"leaseOwner,omitempty" json:"-"`
	LeaseUntil     *time.Time `bson:"leaseUntil,omitempty" json:"-"`
	NextAttemptAt  *time.Time `bson:"nextAttemptAt,omitempty" json:"nextAttemptAt,omitempty"`
	LastError      string     `bson:"lastError,omitempty" json:"lastError,omitempty"`
	CreatedAt      time.Time  `bson:"createdAt" json:"createdAt"`
	UpdatedAt      time.Time  `bson:"updatedAt" json:"updatedAt"`
}

type TradingGateJob struct {
	ID             string     `bson:"_id" json:"id"`
	MatchID        string     `bson:"matchId" json:"matchId"`
	TradingVersion int64      `bson:"tradingVersion" json:"tradingVersion"`
	Status         string     `bson:"status" json:"status"`
	Attempts       int        `bson:"attempts" json:"attempts"`
	LeaseOwner     string     `bson:"leaseOwner,omitempty" json:"-"`
	LeaseUntil     *time.Time `bson:"leaseUntil,omitempty" json:"-"`
	NextAttemptAt  *time.Time `bson:"nextAttemptAt,omitempty" json:"nextAttemptAt,omitempty"`
	LastError      string     `bson:"lastError,omitempty" json:"lastError,omitempty"`
	CreatedAt      time.Time  `bson:"createdAt" json:"createdAt"`
	UpdatedAt      time.Time  `bson:"updatedAt" json:"updatedAt"`
}

type ShadowProjection struct {
	FixtureID  int64                `bson:"_id" json:"fixtureId"`
	Version    int64                `bson:"version" json:"version"`
	Projection reconcile.Projection `bson:"projection" json:"projection"`
	ReceivedAt time.Time            `bson:"receivedAt" json:"receivedAt"`
	UpdatedAt  time.Time            `bson:"updatedAt" json:"updatedAt"`
}

type ReconciliationReport struct {
	ID                        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	FixtureID                 int64              `bson:"fixtureId" json:"fixtureId"`
	Version                   int64              `bson:"version" json:"version"`
	SnapshotHash              string             `bson:"snapshotHash" json:"snapshotHash"`
	DeliveryCount             int                `bson:"deliveryCount" json:"deliveryCount"`
	CorrectionCount           int                `bson:"correctionCount" json:"correctionCount"`
	MissingDeliveryCount      int                `bson:"missingDeliveryCount" json:"missingDeliveryCount"`
	ProviderUpdateToReceiveMS int64              `bson:"providerUpdateToReceiveMs,omitempty" json:"providerUpdateToReceiveMs,omitempty"`
	Reconciled                bool               `bson:"reconciled" json:"reconciled"`
	ReceivedAt                time.Time          `bson:"receivedAt" json:"receivedAt"`
}

type Incident struct {
	ID           string    `bson:"_id" json:"id"`
	FixtureID    int64     `bson:"fixtureId" json:"fixtureId"`
	MatchID      string    `bson:"matchId,omitempty" json:"matchId,omitempty"`
	Kind         string    `bson:"kind" json:"kind"`
	Status       string    `bson:"status" json:"status"`
	SnapshotHash string    `bson:"snapshotHash" json:"snapshotHash"`
	Message      string    `bson:"message" json:"message"`
	CreatedAt    time.Time `bson:"createdAt" json:"createdAt"`
	UpdatedAt    time.Time `bson:"updatedAt" json:"updatedAt"`
}

type OutboxEvent struct {
	ID             primitive.ObjectID `bson:"_id,omitempty" json:"_id"`
	EventID        string             `bson:"eventId" json:"eventId"`
	Topic          string             `bson:"topic" json:"topic"`
	Type           string             `bson:"type" json:"type"`
	MatchID        string             `bson:"matchId" json:"matchId"`
	StateVersion   int64              `bson:"stateVersion" json:"stateVersion"`
	TradingVersion int64              `bson:"tradingVersion" json:"tradingVersion"`
	Sequence       int64              `bson:"sequence" json:"sequence"`
	OccurredAt     time.Time          `bson:"occurredAt" json:"occurredAt"`
	Payload        any                `bson:"payload" json:"payload"`
	CreatedAt      time.Time          `bson:"createdAt" json:"createdAt"`
}

type MarketSnapshot struct {
	ID                 string    `bson:"_id" json:"id"`
	MatchID            string    `bson:"matchId" json:"matchId"`
	MarketID           string    `bson:"marketId" json:"marketId"`
	Innings            int       `bson:"innings" json:"innings"`
	Lifecycle          string    `bson:"lifecycle" json:"lifecycle"`
	Blockers           []string  `bson:"blockers" json:"blockers"`
	FeedState          string    `bson:"feedState" json:"feedState"`
	TradingState       string    `bson:"tradingState" json:"tradingState"`
	StateVersion       int64     `bson:"stateVersion" json:"stateVersion"`
	TradingVersion     int64     `bson:"tradingVersion" json:"tradingVersion"`
	CurrentScore       int       `bson:"currentScore" json:"currentScore"`
	WicketsLost        int       `bson:"wicketsLost" json:"wicketsLost"`
	BallsLeft          int       `bson:"ballsLeft" json:"ballsLeft"`
	TargetScore        int       `bson:"targetScore" json:"targetScore"`
	FinalScore         int       `bson:"finalScore,omitempty" json:"finalScore,omitempty"`
	FinalRevision      int64     `bson:"finalRevision,omitempty" json:"finalRevision,omitempty"`
	FormulaVersion     string    `bson:"formulaVersion" json:"formulaVersion"`
	ProviderSnapshotID string    `bson:"providerSnapshotId,omitempty" json:"providerSnapshotId,omitempty"`
	CreatedAt          time.Time `bson:"createdAt" json:"createdAt"`
}

type payloadRecord struct {
	ID         primitive.ObjectID `bson:"_id,omitempty"`
	FixtureID  int64              `bson:"fixtureId"`
	Mode       string             `bson:"mode"`
	Valid      bool               `bson:"valid"`
	Error      string             `bson:"error,omitempty"`
	Raw        json.RawMessage    `bson:"raw"`
	ReceivedAt time.Time          `bson:"receivedAt"`
	ExpiresAt  time.Time          `bson:"expiresAt"`
}

// MarketProjector is satisfied by markets.Service. Its methods accept a
// mongo.SessionContext, keeping provider market creation/gating in the same
// transaction as the authoritative match projection.
type MarketProjector interface {
	ListMarketsByMatchID(ctx context.Context, matchID string) ([]markets.Market, error)
	EnsureProviderInningsMarket(ctx context.Context, spec markets.ProviderInningsMarketSpec) error
	SetProviderMarketGate(ctx context.Context, matchID string, innings int, lifecycle string, blockers []string, finalScore *int, finalRevision int64) error
	SetProviderManualBlocker(ctx context.Context, id primitive.ObjectID, blocked bool) (*markets.Market, error)
}
