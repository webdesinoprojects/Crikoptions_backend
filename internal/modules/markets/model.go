package markets

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Market struct {
	ID                   primitive.ObjectID `json:"_id" bson:"_id,omitempty"`
	MatchID              string             `json:"matchId" bson:"matchId"`
	Title                string             `json:"title" bson:"title"`
	Type                 string             `json:"type" bson:"type"`
	Status               string             `json:"status" bson:"status"`
	Kind                 string             `json:"kind,omitempty" bson:"kind,omitempty"`
	Innings              int                `json:"innings,omitempty" bson:"innings,omitempty"`
	Format               string             `json:"format,omitempty" bson:"format,omitempty"`
	ScheduledBalls       int                `json:"scheduledBalls,omitempty" bson:"scheduledBalls,omitempty"`
	StrikeMin            float64            `json:"strikeMin,omitempty" bson:"strikeMin,omitempty"`
	StrikeMax            float64            `json:"strikeMax,omitempty" bson:"strikeMax,omitempty"`
	StrikeStep           float64            `json:"strikeStep,omitempty" bson:"strikeStep,omitempty"`
	FormulaVersion       string             `json:"formulaVersion,omitempty" bson:"formulaVersion,omitempty"`
	Lifecycle            string             `json:"lifecycle,omitempty" bson:"lifecycle,omitempty"`
	Blockers             []string           `json:"blockers,omitempty" bson:"blockers,omitempty"`
	MatchStateVersion    int64              `json:"matchStateVersion,omitempty" bson:"matchStateVersion,omitempty"`
	TradingVersion       int64              `json:"tradingVersion,omitempty" bson:"tradingVersion,omitempty"`
	FinalScore           int                `json:"finalScore,omitempty" bson:"finalScore,omitempty"`
	FinalRevision        int64              `json:"finalRevision,omitempty" bson:"finalRevision,omitempty"`
	SettlementRevision   int64              `json:"settlementRevision,omitempty" bson:"settlementRevision,omitempty"`
	SettlementStartedAt  *time.Time         `json:"settlementStartedAt,omitempty" bson:"settlementStartedAt,omitempty"`
	TradingGateCheckedAt *time.Time         `json:"-" bson:"tradingGateCheckedAt,omitempty"`
	GateCheckSeq         int64              `json:"-" bson:"gateCheckSeq,omitempty"`
	BuyerPrice           float64            `json:"buyerPrice" bson:"buyerPrice"`
	SellerPrice          float64            `json:"sellerPrice" bson:"sellerPrice"`
	LTP                  float64            `json:"ltp" bson:"ltp"`
	Open                 float64            `json:"open" bson:"open"`
	High                 float64            `json:"high" bson:"high"`
	Low                  float64            `json:"low" bson:"low"`
	QuantityLadder       []LadderEntry      `json:"quantityLadder" bson:"quantityLadder"`
	CreatedAt            time.Time          `json:"createdAt" bson:"createdAt"`
	UpdatedAt            time.Time          `json:"updatedAt" bson:"updatedAt"`
}

const (
	MarketKindInningsScore = "innings_score"

	FormulaVersionInningsScoreV1 = "innings_score_v1"

	MarketLifecyclePending  = "pending"
	MarketLifecycleOpen     = "open"
	MarketLifecycleSettling = "settling"
	MarketLifecycleSettled  = "settled"
	MarketLifecycleVoid     = "void"
)

type LadderEntry struct {
	BuyerQty    int     `json:"buyerQty" bson:"buyerQty"`
	BuyerPrice  float64 `json:"buyerPrice" bson:"buyerPrice"`
	SellerPrice float64 `json:"sellerPrice" bson:"sellerPrice"`
	SellerQty   int     `json:"sellerQty" bson:"sellerQty"`
}
