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
		nil,
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

func TestSummaryHandlesShortPositionsAndClosedShortTrades(t *testing.T) {
	userID := primitive.NewObjectID()
	marketID := primitive.NewObjectID().Hex()
	now := time.Now()

	svc := NewService(
		stubPositions{
			open: []positions.Position{{
				ID:        "short-open",
				UserID:    userID,
				MatchID:   "match-1",
				MarketID:  marketID,
				Strike:    120,
				Status:    "open",
				Side:      "SELL",
				Lots:      -10,
				SellPrice: 50,
				LTP:       45,
				PnL:       50,
				CreatedAt: now,
				UpdatedAt: now,
			}},
			closed: []positions.Position{{
				ID:          "short-closed",
				UserID:      userID,
				MatchID:     "match-1",
				MarketID:    marketID,
				Strike:      110,
				Status:      "closed",
				Side:        "SELL",
				BuyPrice:    40,
				SellPrice:   50,
				PnL:         100,
				RealizedPnL: 100,
				MatchedLots: 10,
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
			marketID: {Title: "RCB 120"},
		}},
		stubMatches{items: map[string]*matches.Match{
			"match-1": {TeamAName: "RCB", TeamBName: "KKR"},
		}},
		nil,
	)

	summary, err := svc.GetSummary(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if len(summary.Positions) != 1 {
		t.Fatalf("positions = %d, want 1", len(summary.Positions))
	}
	open := summary.Positions[0]
	if open.Side != "SELL" || open.Quantity != 10 || open.AverageEntryPrice != 50 || open.UnrealizedPnL != 50 {
		t.Fatalf("open short = %+v, want SELL qty 10 entry 50 pnl 50", open)
	}
	if summary.TotalEquity != 550 {
		t.Fatalf("totalEquity = %.2f, want 550", summary.TotalEquity)
	}
	if len(summary.ClosedTrades) != 1 {
		t.Fatalf("closed trades = %d, want 1", len(summary.ClosedTrades))
	}
	closed := summary.ClosedTrades[0]
	if closed.Side != "SELL" || closed.EntryPrice != 50 || closed.ExitPrice != 40 || closed.RealizedPnL != 100 {
		t.Fatalf("closed short = %+v, want SELL entry 50 exit 40 pnl 100", closed)
	}
	if summary.TotalPnL != 150 {
		t.Fatalf("totalPnL = %.2f, want 150", summary.TotalPnL)
	}
}
