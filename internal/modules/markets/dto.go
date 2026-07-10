package markets

import "time"

type MarketResponse struct {
	ID             string        `json:"_id"`
	MatchID        string        `json:"matchId"`
	Title          string        `json:"title"`
	Type           string        `json:"type"`
	Status         string        `json:"status"`
	BuyerPrice     float64       `json:"buyerPrice"`
	SellerPrice    float64       `json:"sellerPrice"`
	LTP            float64       `json:"ltp"`
	Open           float64       `json:"open"`
	High           float64       `json:"high"`
	Low            float64       `json:"low"`
	QuantityLadder []LadderEntry `json:"quantityLadder"`
	CreatedAt      time.Time     `json:"createdAt"`
	UpdatedAt      time.Time     `json:"updatedAt"`
}

type PriceResponse struct {
	BuyerPrice  float64         `json:"buyerPrice"`
	SellerPrice float64         `json:"sellerPrice"`
	LTP         float64         `json:"ltp"`
	Open        float64         `json:"open"`
	High        float64         `json:"high"`
	Low         float64         `json:"low"`
	StrikeStep  float64         `json:"strikeStep,omitempty"`
	MaxStrike   float64         `json:"maxStrike,omitempty"`
	ProjectedS0 float64         `json:"projectedS0,omitempty"`
	OptionChain []StrikePremium `json:"optionChain,omitempty"`
}

type OptionChainHistoryResponse struct {
	MarketID  string                    `json:"marketId"`
	MatchID   string                    `json:"matchId"`
	Innings   int                       `json:"innings"`
	StartedAt int64                     `json:"startedAt"`
	Points    []OptionChainHistoryPoint `json:"points"`
}

type OptionChainHistoryPoint struct {
	MarketID  string  `json:"marketId"`
	Timestamp int64   `json:"timestamp"`
	Strike    float64 `json:"strike"`
	Premium   float64 `json:"premium"`
	Bid       float64 `json:"bid"`
	Ask       float64 `json:"ask"`
	BidQty    int     `json:"bidQty"`
	AskQty    int     `json:"askQty"`
	Moneyness string  `json:"moneyness"`
}

type StrikePremium struct {
	Strike  float64 `json:"strike"`
	Premium float64 `json:"premium"`
}

type CreateMarketRequest struct {
	MatchID        string        `json:"matchId"`
	Title          string        `json:"title"`
	Type           string        `json:"type"`
	Status         string        `json:"status"`
	BuyerPrice     float64       `json:"buyerPrice"`
	SellerPrice    float64       `json:"sellerPrice"`
	LTP            float64       `json:"ltp"`
	Open           float64       `json:"open"`
	High           float64       `json:"high"`
	Low            float64       `json:"low"`
	QuantityLadder []LadderEntry `json:"quantityLadder"`
}
