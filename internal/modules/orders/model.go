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

	PositionEffectAuto  = "AUTO"
	PositionEffectOpen  = "OPEN"
	PositionEffectClose = "CLOSE"
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
	PositionEffect    string             `json:"positionEffect" bson:"positionEffect,omitempty"`
	PositionIntent    string             `json:"positionIntent" bson:"positionIntent,omitempty"`
	Quantity          int                `json:"quantity" bson:"quantity"`
	Price             float64            `json:"price" bson:"price"`
	ReservedAmount    float64            `json:"reservedAmount" bson:"reservedAmount,omitempty"`
	ReservedQuantity  int                `json:"reservedQuantity" bson:"reservedQuantity,omitempty"`
	FilledQuantity    int                `json:"filledQuantity" bson:"filledQuantity"`
	RemainingQuantity int                `json:"remainingQuantity" bson:"remainingQuantity"`
	AverageFillPrice  float64            `json:"averageFillPrice" bson:"averageFillPrice"`
	Status            string             `json:"status" bson:"status"`
	RejectionReason   string             `json:"rejectionReason,omitempty" bson:"rejectionReason,omitempty"`
	CreatedAt         time.Time          `json:"createdAt" bson:"createdAt"`
	UpdatedAt         time.Time          `json:"updatedAt" bson:"updatedAt"`
}

func (o Order) RemainingReservedAmount() float64 {
	if o.RemainingQuantity <= 0 {
		return 0
	}
	if o.ReservedAmount > 0 && o.ReservedQuantity > 0 {
		qty := o.RemainingQuantity
		if qty > o.ReservedQuantity {
			qty = o.ReservedQuantity
		}
		return round2(o.ReservedAmount * float64(qty) / float64(o.ReservedQuantity))
	}
	if o.Side != "buy" {
		return 0
	}
	return round2(o.Price * float64(o.RemainingQuantity))
}

func (o Order) ReservedReleaseForFill(fillQty int) float64 {
	if fillQty <= 0 {
		return 0
	}
	if o.ReservedAmount > 0 && o.ReservedQuantity > 0 {
		reservedBefore := o.RemainingReservedAmount()
		after := o
		after.RemainingQuantity -= fillQty
		if after.RemainingQuantity < 0 {
			after.RemainingQuantity = 0
		}
		return round2(reservedBefore - after.RemainingReservedAmount())
	}
	if o.Side != "buy" {
		return 0
	}
	return round2(o.Price * float64(fillQty))
}

func (o Order) ReservedReleaseForQuantity(qty int) float64 {
	if qty <= 0 {
		return 0
	}
	if o.ReservedAmount > 0 && o.ReservedQuantity > 0 {
		remainingReservedQty := o.RemainingQuantity
		if remainingReservedQty > o.ReservedQuantity {
			remainingReservedQty = o.ReservedQuantity
		}
		if qty > remainingReservedQty {
			qty = remainingReservedQty
		}
		return round2(o.ReservedAmount * float64(qty) / float64(o.ReservedQuantity))
	}
	if o.Side != "buy" {
		return 0
	}
	return round2(o.Price * float64(qty))
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
