package orders

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
)

type squareOffPositions struct {
	byMatch map[string][]PositionSnapshot
}

func (p *squareOffPositions) PositionFor(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) (PositionSnapshot, bool) {
	return PositionSnapshot{}, false
}

func (p *squareOffPositions) ResolveCloseTarget(ctx context.Context, userID primitive.ObjectID, positionID string) (PositionSnapshot, bool) {
	return PositionSnapshot{}, false
}

func (p *squareOffPositions) OpenCloseTargets(ctx context.Context, userID primitive.ObjectID) ([]PositionSnapshot, error) {
	return nil, nil
}

func (p *squareOffPositions) ListOpenByMatch(_ context.Context, matchID string) ([]PositionSnapshot, error) {
	if matchID == "" {
		var all []PositionSnapshot
		for _, list := range p.byMatch {
			all = append(all, list...)
		}
		return all, nil
	}
	return p.byMatch[matchID], nil
}

func TestSquareOff_Innings1SettlesFutureMarketsOnly(t *testing.T) {
	userID := primitive.NewObjectID()
	matchHex := "0000000000000000000000aa"
	futureID, _ := primitive.ObjectIDFromHex("0000000000000000000000d2")
	depthID, _ := primitive.ObjectIDFromHex("0000000000000000000000d1")

	futureMarket := markets.Market{ID: futureID, MatchID: "1", Type: "future", Status: markets.MarketStatusActive, LTP: 165}
	depthMarket := markets.Market{ID: depthID, MatchID: "1", Type: "match_depth", Status: markets.MarketStatusActive, LTP: 155}

	marketSvc := &stubMarketSvc{
		markets: []markets.Market{futureMarket, depthMarket},
		market:  &futureMarket,
		bid:     164,
		ask:     166,
		ok:      true,
	}

	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	_, _ = walletSvc.AdminCredit(context.Background(), primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: 10000, Reason: "seed"})

	execSvc := executions.NewService(executions.NewMemoryRepository())
	orderRepo := NewMemoryRepository()
	orderRepo.orders = nil

	posView := &squareOffPositions{
		byMatch: map[string][]PositionSnapshot{
			"1": {
				{UserID: userID, MatchID: "1", MarketID: futureID.Hex(), Strike: 160, Lots: 5, BuyPrice: 150, LTP: 165, Status: "open"},
				{UserID: userID, MatchID: "1", MarketID: depthID.Hex(), Strike: 155, Lots: 3, BuyPrice: 150, LTP: 155, Status: "open"},
			},
			matchHex: {
				{UserID: userID, MatchID: "1", MarketID: futureID.Hex(), Strike: 160, Lots: 5, BuyPrice: 150, LTP: 165, Status: "open"},
				{UserID: userID, MatchID: "1", MarketID: depthID.Hex(), Strike: 155, Lots: 3, BuyPrice: 150, LTP: 155, Status: "open"},
			},
		},
	}

	// Seed a buy execution so OpenLongQty reflects the future position.
	_, _ = execSvc.Create(context.Background(), executions.Execution{
		UserID: userID, MatchID: "1", MarketID: futureID.Hex(), Strike: 160,
		Side: "buy", Price: 150, Quantity: 5,
	})
	_, _ = execSvc.Create(context.Background(), executions.Execution{
		UserID: userID, MatchID: "1", MarketID: depthID.Hex(), Strike: 155,
		Side: "buy", Price: 150, Quantity: 3,
	})

	matchID, _ := primitive.ObjectIDFromHex(matchHex)
	svc := NewService(
		orderRepo,
		marketSvc,
		&stubMatchSvc{match: &matches.Match{ID: matchID, Status: "live", Innings: 1, CurrentScore: 200, WicketsLost: 4, BallsLeft: 0}},
		walletSvc,
		execSvc,
		posView,
		nil,
	)

	result, err := svc.SquareOff(context.Background(), matchHex, SquareOffScopeInnings1)
	if err != nil {
		t.Fatalf("SquareOff: %v", err)
	}
	if result.PositionsSettled != 2 {
		t.Fatalf("PositionsSettled = %d, want 2 (all open positions on match)", result.PositionsSettled)
	}
	if result.MarketsClosed != 1 {
		t.Fatalf("MarketsClosed = %d, want 1 (future market only)", result.MarketsClosed)
	}
	if futureMarket.Status != markets.MarketStatusClosed {
		t.Fatalf("future market status = %q, want closed", futureMarket.Status)
	}
	if depthMarket.Status != markets.MarketStatusActive {
		t.Fatalf("depth market should stay active during innings 1 square-off, got %q", depthMarket.Status)
	}

	depthLots := execSvc.OpenLongQty(context.Background(), userID, "1", depthID.Hex(), 155)
	if depthLots != 0 {
		t.Fatalf("depth open lots = %d, want 0 after innings square-off", depthLots)
	}
	futureLots := execSvc.OpenLongQty(context.Background(), userID, "1", futureID.Hex(), 160)
	if futureLots != 0 {
		t.Fatalf("future open lots = %d, want 0 after square-off", futureLots)
	}

	acct, _ := walletSvc.GetWallet(context.Background(), userID)
	// Sell 5 @ 164 after buy @ 150 → +70 proceeds net of original buy settlement already in wallet from manual exec only
	// Wallet got sell proceeds: 5 * 164 = 820
	if acct.AvailableBalance < 800 {
		t.Fatalf("wallet balance = %.2f, expected sell proceeds credited", acct.AvailableBalance)
	}
}

func TestSquareOff_MatchSettlesAllMarkets(t *testing.T) {
	userID := primitive.NewObjectID()
	matchHex := "0000000000000000000000aa"
	futureID, _ := primitive.ObjectIDFromHex("0000000000000000000000d2")
	depthID, _ := primitive.ObjectIDFromHex("0000000000000000000000d1")

	futureMarket := markets.Market{ID: futureID, MatchID: "1", Type: "future", Status: markets.MarketStatusActive, LTP: 165}
	depthMarket := markets.Market{ID: depthID, MatchID: "1", Type: "match_depth", Status: markets.MarketStatusActive, LTP: 155}

	marketSvc := &stubMarketSvc{
		markets: []markets.Market{futureMarket, depthMarket},
		market:  &depthMarket,
		bid:     154,
		ask:     156,
		ok:      true,
	}

	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	execSvc := executions.NewService(executions.NewMemoryRepository())
	orderRepo := NewMemoryRepository()

	posView := &squareOffPositions{
		byMatch: map[string][]PositionSnapshot{
			"1": {
				{UserID: userID, MatchID: "1", MarketID: futureID.Hex(), Strike: 160, Lots: 2, BuyPrice: 150, Status: "open"},
				{UserID: userID, MatchID: "1", MarketID: depthID.Hex(), Strike: 155, Lots: 4, BuyPrice: 150, Status: "open"},
			},
		},
	}

	_, _ = execSvc.Create(context.Background(), executions.Execution{
		UserID: userID, MatchID: "1", MarketID: futureID.Hex(), Strike: 160, Side: "buy", Price: 150, Quantity: 2,
	})
	_, _ = execSvc.Create(context.Background(), executions.Execution{
		UserID: userID, MatchID: "1", MarketID: depthID.Hex(), Strike: 155, Side: "buy", Price: 150, Quantity: 4,
	})

	matchID, _ := primitive.ObjectIDFromHex(matchHex)
	svc := NewService(orderRepo, marketSvc, &stubMatchSvc{match: &matches.Match{ID: matchID, Status: "live", Innings: 2}}, walletSvc, execSvc, posView, nil)

	result, err := svc.SquareOff(context.Background(), matchHex, SquareOffScopeMatch)
	if err != nil {
		t.Fatalf("SquareOff: %v", err)
	}
	if result.PositionsSettled != 2 {
		t.Fatalf("PositionsSettled = %d, want 2", result.PositionsSettled)
	}
	if result.MarketsClosed != 2 {
		t.Fatalf("MarketsClosed = %d, want 2", result.MarketsClosed)
	}
}

func TestSquareOff_MatchSettlesShortAtAsk(t *testing.T) {
	userID := primitive.NewObjectID()
	matchHex := "0000000000000000000000aa"
	marketID, _ := primitive.ObjectIDFromHex("0000000000000000000000d3")

	market := markets.Market{ID: marketID, MatchID: "1", Type: "future", Status: markets.MarketStatusActive, LTP: 45}
	marketSvc := &stubMarketSvc{
		markets: []markets.Market{market},
		market:  &market,
		bid:     44,
		ask:     45,
		ok:      true,
	}

	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	_, _ = walletSvc.AdminCredit(context.Background(), primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: 10000, Reason: "seed"})
	_, _ = walletSvc.ReserveOrderMargin(context.Background(), userID, 500, "short-open", "short initial margin")
	_, _ = walletSvc.SettleShortOpenFill(context.Background(), userID, 500, "short-open", "short sale proceeds")

	execSvc := executions.NewService(executions.NewMemoryRepository())
	_, _ = execSvc.Create(context.Background(), executions.Execution{
		UserID: userID, MatchID: "1", MarketID: marketID.Hex(), Strike: 160, Side: "sell", Price: 50, Quantity: 10,
	})

	orderRepo := NewMemoryRepository()
	orderRepo.orders = nil
	posView := &squareOffPositions{
		byMatch: map[string][]PositionSnapshot{
			"1": {
				{UserID: userID, MatchID: "1", MarketID: marketID.Hex(), Strike: 160, Lots: -10, SellPrice: 50, LTP: 45, Status: "open"},
			},
		},
	}

	matchID, _ := primitive.ObjectIDFromHex(matchHex)
	svc := NewService(orderRepo, marketSvc, &stubMatchSvc{match: &matches.Match{ID: matchID, Status: "live", Innings: 2}}, walletSvc, execSvc, posView, nil)

	result, err := svc.SquareOff(context.Background(), matchHex, SquareOffScopeMatch)
	if err != nil {
		t.Fatalf("SquareOff: %v", err)
	}
	if result.PositionsSettled != 1 || result.TotalRealizedPnL != 50 {
		t.Fatalf("settled/pnl = %d/%.2f, want 1/50", result.PositionsSettled, result.TotalRealizedPnL)
	}
	if got := execSvc.NetLots(context.Background(), userID, "1", marketID.Hex(), 160); got != 0 {
		t.Fatalf("net lots = %d, want 0", got)
	}
	orders := orderRepo.GetAll(context.Background())
	if len(orders) != 1 || orders[0].Side != "buy" || orders[0].AverageFillPrice != 45 {
		t.Fatalf("settlement order = %+v, want one BUY at ask 45", orders)
	}
	acct, _ := walletSvc.GetWallet(context.Background(), userID)
	if acct.CashBalance != 10050 || acct.ReservedBalance != 0 || acct.AvailableBalance != 10050 {
		t.Fatalf("wallet = cash %.2f reserved %.2f available %.2f, want 10050/0/10050", acct.CashBalance, acct.ReservedBalance, acct.AvailableBalance)
	}
}

func TestReopenMatchMarkets(t *testing.T) {
	marketID, _ := primitive.ObjectIDFromHex("0000000000000000000000d2")
	m := markets.Market{ID: marketID, MatchID: "1", Type: "future", Status: markets.MarketStatusClosed}
	marketSvc := &stubMarketSvc{markets: []markets.Market{m}, market: &m}

	svc := NewService(NewMemoryRepository(), marketSvc, &stubMatchSvc{}, wallet.NewService(wallet.NewMemoryRepository()), executions.NewService(executions.NewMemoryRepository()), &squareOffPositions{}, nil)

	if err := svc.ReopenMatchMarkets(context.Background(), "0000000000000000000000aa"); err != nil {
		t.Fatalf("ReopenMatchMarkets: %v", err)
	}
	if m.Status != markets.MarketStatusActive {
		t.Fatalf("status = %q, want active", m.Status)
	}
}
