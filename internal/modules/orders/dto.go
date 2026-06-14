package orders

type CreateOrderRequest struct {
	ClientOrderID string  `json:"clientOrderId"`
	MatchID       string  `json:"matchId"`
	MarketID      string  `json:"marketId"`
	Strike        float64 `json:"strike"`
	Side          string  `json:"side"`
	Type          string  `json:"type"`
	Quantity      int     `json:"quantity"`
	Price         float64 `json:"price"`
}

type OrderResponse struct {
	ID                string  `json:"_id"`
	UserID            string  `json:"userId"`
	MatchID           string  `json:"matchId"`
	MarketID          string  `json:"marketId"`
	Strike            float64 `json:"strike"`
	Side              string  `json:"side"`
	Type              string  `json:"type"`
	Quantity          int     `json:"quantity"`
	Price             float64 `json:"price"`
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
