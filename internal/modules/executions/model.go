package executions

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	LiquiditySystemMarketMaker   = "SYSTEM_MARKET_MAKER"
	LiquidityProviderVoidReverse = "PROVIDER_VOID_REVERSAL"
)

type Execution struct {
	ID              primitive.ObjectID `json:"_id" bson:"_id,omitempty"`
	UserID          primitive.ObjectID `json:"userId" bson:"userId"`
	OrderID         primitive.ObjectID `json:"orderId" bson:"orderId"`
	MatchID         string             `json:"matchId" bson:"matchId"`
	MarketID        string             `json:"marketId" bson:"marketId"`
	Strike          float64            `json:"strike" bson:"strike"`
	Side            string             `json:"side" bson:"side"`
	Price           float64            `json:"price" bson:"price"`
	Quantity        int                `json:"quantity" bson:"quantity"`
	LiquiditySource string             `json:"liquiditySource" bson:"liquiditySource"`
	CreatedAt       time.Time          `json:"createdAt" bson:"createdAt"`

	// Match clock at the moment of this fill. Opening fills are the open
	// snapshot; closing fills are the close snapshot. Daily challenges must
	// not re-read live overs later.
	OversText  string    `json:"oversText,omitempty" bson:"oversText,omitempty"`
	LegalBalls int       `json:"legalBalls,omitempty" bson:"legalBalls,omitempty"`
	Innings    int       `json:"innings,omitempty" bson:"innings,omitempty"`
	Format     string    `json:"format,omitempty" bson:"format,omitempty"`
	ClockAt    time.Time `json:"clockAt,omitempty" bson:"clockAt,omitempty"`
}

func (e Execution) Clock() MatchClock {
	matchID := e.MatchID
	return MatchClock{
		OversText:  e.OversText,
		LegalBalls: e.LegalBalls,
		Innings:    e.Innings,
		Format:     e.Format,
		MatchID:    matchID,
		At:         e.ClockAt,
	}
}

func (e *Execution) SetClock(c MatchClock) {
	if e == nil {
		return
	}
	e.OversText = c.OversText
	e.LegalBalls = c.LegalBalls
	e.Innings = c.Innings
	e.Format = c.Format
	e.ClockAt = c.At
}

type Filter struct {
	UserID                 primitive.ObjectID
	MatchID                string
	MarketID               string
	OrderID                primitive.ObjectID
	ExcludeLiquiditySource string
	Limit                  int64
}
