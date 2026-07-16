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
}

type Filter struct {
	UserID                 primitive.ObjectID
	MatchID                string
	MarketID               string
	OrderID                primitive.ObjectID
	ExcludeLiquiditySource string
	Limit                  int64
}
