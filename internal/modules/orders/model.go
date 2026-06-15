package orders

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	StatusOpen            = "open"
	StatusPartiallyFilled = "partially_filled"
	StatusExecuted        = "executed"
	StatusCancelled       = "cancelled"
	StatusRejected        = "rejected"

	OrderTypeLimit  = "LIMIT"
	OrderTypeMarket = "MARKET"
)

type Order struct {
	ID                primitive.ObjectID `json:"_id" bson:"_id,omitempty"`
	ClientOrderID     string             `json:"clientOrderId,omitempty" bson:"clientOrderId,omitempty"`
	UserID            primitive.ObjectID `json:"userId" bson:"userId"`
	MatchID           string             `json:"matchId" bson:"matchId"`
	MarketID          string             `json:"marketId" bson:"marketId"`
	Strike            float64            `json:"strike" bson:"strike"`
	Side              string             `json:"side" bson:"side"`
	Type              string             `json:"type" bson:"type"`
	Quantity          int                `json:"quantity" bson:"quantity"`
	Price             float64            `json:"price" bson:"price"`
	FilledQuantity    int                `json:"filledQuantity" bson:"filledQuantity"`
	RemainingQuantity int                `json:"remainingQuantity" bson:"remainingQuantity"`
	AverageFillPrice  float64            `json:"averageFillPrice" bson:"averageFillPrice"`
	Status            string             `json:"status" bson:"status"`
	RejectionReason   string             `json:"rejectionReason,omitempty" bson:"rejectionReason,omitempty"`
	CreatedAt         time.Time          `json:"createdAt" bson:"createdAt"`
	UpdatedAt         time.Time          `json:"updatedAt" bson:"updatedAt"`
}

func (o Order) ReservedAmount() float64 {
	if o.Side != "buy" {
		return 0
	}
	return round2(o.Price * float64(o.RemainingQuantity))
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
