package positions

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Position is a derived view over a user's executed orders for a single
// (userId, matchId, marketId) tuple. It is computed on demand from the orders
// collection; nothing is written back to Mongo. This keeps the position state
// always consistent with the underlying order book.
type Position struct {
	ID        string             `json:"_id" bson:"_id,omitempty"`
	UserID    primitive.ObjectID `json:"userId" bson:"userId"`
	MatchID   string             `json:"matchId" bson:"matchId"`
	MarketID  string             `json:"marketId" bson:"marketId"`
	Status    string             `json:"status" bson:"status"` // "open" or "closed"
	Lots      int                `json:"lots" bson:"lots"`     // net absolute quantity (buy - sell)
	BuyPrice  float64            `json:"buyPrice" bson:"buyPrice"`
	SellPrice float64            `json:"sellPrice" bson:"sellPrice"`
	LTP       float64            `json:"ltp" bson:"ltp"`
	PnL       float64            `json:"pnl" bson:"pnl"`
	CreatedAt time.Time          `json:"createdAt" bson:"createdAt"`
	UpdatedAt time.Time          `json:"updatedAt" bson:"updatedAt"`
}
