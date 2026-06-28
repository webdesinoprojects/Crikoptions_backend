package portfolio

import "github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"

type PortfolioPosition struct {
	ID                string  `json:"id"`
	MarketID          string  `json:"marketId"`
	Symbol            string  `json:"symbol"`
	MatchName         string  `json:"matchName"`
	Strike            string  `json:"strike,omitempty"`
	Side              string  `json:"side"`
	Quantity          int     `json:"quantity"`
	AverageEntryPrice float64 `json:"averageEntryPrice"`
	CurrentPrice      float64 `json:"currentPrice"`
	UnrealizedPnL     float64 `json:"unrealizedPnL"`
	UnrealizedPnLPct  float64 `json:"unrealizedPnLPct"`
	Notional          float64 `json:"notional"`
	Allocation        float64 `json:"allocation"`
	OpenedAt          string  `json:"openedAt"`
}

type ClosedTrade struct {
	OrderID         string  `json:"orderId"`
	MarketID        string  `json:"marketId"`
	Symbol          string  `json:"symbol"`
	MatchName       string  `json:"matchName"`
	Side            string  `json:"side"`
	Quantity        int     `json:"quantity"`
	EntryPrice      float64 `json:"entryPrice"`
	ExitPrice       float64 `json:"exitPrice"`
	RealizedPnL     float64 `json:"realizedPnL"`
	RealizedPnLPct  float64 `json:"realizedPnLPct"`
	OpenedAt        string  `json:"openedAt"`
	ClosedAt        string  `json:"closedAt"`
	HoldingPeriodMs int64   `json:"holdingPeriodMs"`
}

type EquityCurvePoint struct {
	Timestamp int64   `json:"timestamp"`
	Equity    float64 `json:"equity"`
	Drawdown  float64 `json:"drawdown"`
}

type RiskMetrics struct {
	MaxConcentration     float64 `json:"maxConcentration"`
	DiversificationScore float64 `json:"diversificationScore"`
	LeverageRatio        float64 `json:"leverageRatio"`
	PortfolioVolatility  float64 `json:"portfolioVolatility"`
	StressTestLoss       float64 `json:"stressTestLoss"`
}

type PortfolioSummary struct {
	TotalEquity        float64             `json:"totalEquity"`
	BaseCapital        float64             `json:"baseCapital"`
	TotalUnrealizedPnL float64             `json:"totalUnrealizedPnL"`
	TotalRealizedPnL   float64             `json:"totalRealizedPnL"`
	TotalPnL           float64             `json:"totalPnL"`
	TotalPnLPct        float64             `json:"totalPnLPct"`
	DailyPnL           float64             `json:"dailyPnL"`
	DailyPnLPct        float64             `json:"dailyPnLPct"`
	OpenPositionsCount int                 `json:"openPositionsCount"`
	ClosedTradesCount  int                 `json:"closedTradesCount"`
	WinRate            float64             `json:"winRate"`
	AvgWin             float64             `json:"avgWin"`
	AvgLoss            float64             `json:"avgLoss"`
	ProfitFactor       float64             `json:"profitFactor"`
	AvailableMargin    float64             `json:"availableMargin"`
	UsedMargin         float64             `json:"usedMargin"`
	MarginUsagePct     float64             `json:"marginUsagePct"`
	Wallet             wallet.Account      `json:"wallet"`
	Positions          []PortfolioPosition `json:"positions"`
	ClosedTrades       []ClosedTrade       `json:"closedTrades"`
	EquityCurve        []EquityCurvePoint  `json:"equityCurve"`
	RiskMetrics        RiskMetrics         `json:"riskMetrics"`
}

type DailyPnLResponse struct {
	DailyPnL    float64 `json:"dailyPnL"`
	DailyPnLPct float64 `json:"dailyPnLPct"`
}

type RiskSummaryResponse struct {
	RiskMetrics     RiskMetrics `json:"riskMetrics"`
	MarginUsagePct  float64     `json:"marginUsagePct"`
	UsedMargin      float64     `json:"usedMargin"`
	AvailableMargin float64     `json:"availableMargin"`
}

type MarketPnLResponse struct {
	MarketID  string  `json:"marketId"`
	OpenPnL   float64 `json:"openPnl"`
	ClosedPnL float64 `json:"closedPnl"`
	TotalPnL  float64 `json:"totalPnl"`
}
