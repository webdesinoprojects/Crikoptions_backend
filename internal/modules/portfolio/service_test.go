package portfolio

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/positions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
)

type stubPositions struct {
	open   []positions.Position
	closed []positions.Position
}

func (s stubPositions) GetUserOpenPositions(context.Context, primitive.ObjectID) ([]positions.Position, error) {
	return s.open, nil
}

func (s stubPositions) GetUserClosedPositions(context.Context, primitive.ObjectID) ([]positions.Position, error) {
	return s.closed, nil
}

type stubWallet struct {
	account *wallet.Account
}

func (s stubWallet) GetWallet(context.Context, primitive.ObjectID) (*wallet.Account, error) {
	return s.account, nil
}

type stubMarkets struct {
	items map[string]*markets.Market
}

func (s stubMarkets) GetMarketByID(_ context.Context, id string) (*markets.Market, error) {
	return s.items[id], nil
}

type stubMatches struct {
	items map[string]*matches.Match
}

func (s stubMatches) GetMatchByID(_ context.Context, id string) (*matches.Match, error) {
	return s.items[id], nil
}

func TestSummaryAggregatesPortfolioMetrics(t *testing.T) {
	userID := primitive.NewObjectID()
	marketID := primitive.NewObjectID().Hex()
	now := time.Now()

	svc := NewService(
		stubPositions{
			open: []positions.Position{{
				ID:        "open-1",
				UserID:    userID,
				MatchID:   "match-1",
				MarketID:  marketID,
				Strike:    120,
				Status:    "open",
				Lots:      10,
				BuyPrice:  20,
				LTP:       25,
				PnL:       50,
				CreatedAt: now,
				UpdatedAt: now,
			}},
			closed: []positions.Position{{
				ID:          "closed-1",
				UserID:      userID,
				MatchID:     "match-1",
				MarketID:    marketID,
				Strike:      110,
				Status:      "closed",
				BuyPrice:    10,
				SellPrice:   16,
				PnL:         30,
				RealizedPnL: 30,
				MatchedLots: 5,
				CreatedAt:   now.Add(-time.Hour),
				UpdatedAt:   now,
			}},
		},
		stubWallet{account: &wallet.Account{
			UserID:           userID,
			CashBalance:      1000,
			ReservedBalance:  100,
			AvailableBalance: 900,
		}},
		stubMarkets{items: map[string]*markets.Market{
			marketID: {Title: "CSK Winner"},
		}},
		stubMatches{items: map[string]*matches.Match{
			"match-1": {TeamAName: "CSK", TeamBName: "MI"},
		}},
	)

	summary, err := svc.GetSummary(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}

	if summary.TotalEquity != 1250 {
		t.Fatalf("totalEquity = %.2f, want 1250", summary.TotalEquity)
	}
	if summary.TotalPnL != 80 || summary.DailyPnL != 80 {
		t.Fatalf("pnl/daily = %.2f/%.2f, want 80/80", summary.TotalPnL, summary.DailyPnL)
	}
	if summary.MarginUsagePct != 10 {
		t.Fatalf("marginUsagePct = %.2f, want 10", summary.MarginUsagePct)
	}
	if summary.RiskMetrics.LeverageRatio != 0.2 {
		t.Fatalf("leverage = %.2f, want 0.2", summary.RiskMetrics.LeverageRatio)
	}
	if len(summary.Positions) != 1 || summary.Positions[0].Allocation != 100 {
		t.Fatalf("positions/allocation = %+v, want one 100%% allocation", summary.Positions)
	}
	if len(summary.ClosedTrades) != 1 || summary.WinRate != 100 {
		t.Fatalf("closed/winRate = %d/%.2f, want 1/100", len(summary.ClosedTrades), summary.WinRate)
	}
}
