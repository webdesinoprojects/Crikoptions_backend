package orders

import "github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"

type CreateOrderRequest struct {
	ClientOrderID   string                         `json:"clientOrderId"`
	MatchID         string                         `json:"matchId"`
	MarketID        string                         `json:"marketId"`
	Strike          float64                        `json:"strike"`
	Side            string                         `json:"side"`
	Type            string                         `json:"type"`
	PositionEffect  string                         `json:"positionEffect,omitempty"`
	Quantity        int                            `json:"quantity"`
	Price           float64                        `json:"price"`
	PricingSnapshot *markets.PriceCalculationInput `json:"pricingSnapshot,omitempty"`
}

type OrderResponse struct {
	ID                string  `json:"_id"`
	UserID            string  `json:"userId"`
	MatchID           string  `json:"matchId"`
	MarketID          string  `json:"marketId"`
	Strike            float64 `json:"strike"`
	Side              string  `json:"side"`
	Type              string  `json:"type"`
	PositionEffect    string  `json:"positionEffect,omitempty"`
	PositionIntent    string  `json:"positionIntent,omitempty"`
	Quantity          int     `json:"quantity"`
	Price             float64 `json:"price"`
	ReservedAmount    float64 `json:"reservedAmount,omitempty"`
	ReservedQuantity  int     `json:"reservedQuantity,omitempty"`
	FilledQuantity    int     `json:"filledQuantity"`
	RemainingQuantity int     `json:"remainingQuantity"`
	AverageFillPrice  float64 `json:"averageFillPrice"`
	Status            string  `json:"status"`
	RejectionReason   string  `json:"rejectionReason,omitempty"`
	CreatedAt         string  `json:"createdAt"`
	UpdatedAt         string  `json:"updatedAt"`
}

type CancelOrderRequest struct {
	Reason string `json:"reason"`
}

type OrderPreviewResponse struct {
	MatchID           string  `json:"matchId"`
	MarketID          string  `json:"marketId"`
	Strike            float64 `json:"strike"`
	Side              string  `json:"side"`
	Type              string  `json:"type"`
	PositionEffect    string  `json:"positionEffect"`
	PositionIntent    string  `json:"positionIntent"`
	Quantity          int     `json:"quantity"`
	RequestedPrice    float64 `json:"requestedPrice"`
	OrderPrice        float64 `json:"orderPrice"`
	ExecutablePrice   float64 `json:"executablePrice"`
	Bid               float64 `json:"bid"`
	Ask               float64 `json:"ask"`
	Notional          float64 `json:"notional"`
	MarginRequired    float64 `json:"marginRequired"`
	NetLotsBefore     int     `json:"netLotsBefore"`
	ProjectedLots     int     `json:"projectedLots"`
	AvailableBalance  float64 `json:"availableBalance"`
	SufficientBalance bool    `json:"sufficientBalance"`
	WillExecuteNow    bool    `json:"willExecuteNow"`
	Message           string  `json:"message"`
}

// ClosePositionRequest is the body for POST /api/v1/positions/{id}/close.
// Quantity is optional (defaults to the full open lots). Price is required for
// LIMIT exits.
type ClosePositionRequest struct {
	Type     string  `json:"type"`
	Quantity int     `json:"quantity"`
	Price    float64 `json:"price"`
}

// CloseAllPositionsRequest is the body for POST /api/v1/positions/close-all.
// Bulk exits are market exits so every open strike can use its own live bid.
type CloseAllPositionsRequest struct {
	Type string `json:"type"`
}

type CloseAllPositionFailure struct {
	MatchID  string  `json:"matchId"`
	MarketID string  `json:"marketId"`
	Strike   float64 `json:"strike"`
	Quantity int     `json:"quantity"`
	Message  string  `json:"message"`
}

type CloseAllPositionsResponse struct {
	Requested int                       `json:"requested"`
	Submitted int                       `json:"submitted"`
	Failed    int                       `json:"failed"`
	Orders    []*Order                  `json:"orders"`
	Failures  []CloseAllPositionFailure `json:"failures"`
}
