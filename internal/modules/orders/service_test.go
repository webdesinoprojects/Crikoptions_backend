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

type stubMarketSvc struct {
	market *markets.Market
	bid    float64
	ask    float64
	ok     bool
}

func (s *stubMarketSvc) GetMarketByID(_ context.Context, _ string) (*markets.Market, error) {
	return s.market, nil
}

func (s *stubMarketSvc) StrikeQuote(_ markets.PriceCalculationInput, _ float64) (float64, float64, bool) {
	return s.bid, s.ask, s.ok
}

func (s *stubMarketSvc) IsTradable(_ *markets.Market) bool {
	return true
}

type stubMatchSvc struct {
	match *matches.Match
}

func (s *stubMatchSvc) GetMatchByID(_ context.Context, _ string) (*matches.Match, error) {
	return s.match, nil
}

func TestCreateOrder_LimitBuyAtAskFills(t *testing.T) {
	userID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()

	walletRepo := wallet.NewMemoryRepository()
	walletSvc := wallet.NewService(walletRepo)
	_, _ = walletSvc.AdminCredit(context.Background(), primitive.NewObjectID(), userID, wallet.FundingRequest{
		Amount: 100000,
		Reason: "seed",
	})

	execRepo := executions.NewMemoryRepository()
	execSvc := executions.NewService(execRepo)
	orderRepo := NewMemoryRepository()
	orderRepo.orders = nil

	svc := NewService(
		orderRepo,
		&stubMarketSvc{
			market: &markets.Market{ID: marketID, Status: markets.MarketStatusActive},
			bid:    50.75,
			ask:    51.75,
			ok:     true,
		},
		&stubMatchSvc{match: &matches.Match{Status: "live", Innings: 1, CurrentScore: 85, WicketsLost: 2, BallsLeft: 42}},
		walletSvc,
		execSvc,
	)

	order, err := svc.CreateOrder(context.Background(), userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: marketID.Hex(),
		Strike:   130,
		Side:     "buy",
		Type:     OrderTypeLimit,
		Quantity: 10,
		Price:    51.75,
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.Status != StatusExecuted {
		t.Fatalf("status = %q, want %q", order.Status, StatusExecuted)
	}
	if order.FilledQuantity != 10 {
		t.Fatalf("filledQuantity = %d, want 10", order.FilledQuantity)
	}
	if order.AverageFillPrice != 51.75 {
		t.Fatalf("averageFillPrice = %.2f, want 51.75", order.AverageFillPrice)
	}

	fills := execSvc.ListUserExecutions(context.Background(), userID, "", "", 10)
	if len(fills) != 1 {
		t.Fatalf("executions = %d, want 1", len(fills))
	}

	acct, _ := walletSvc.GetWallet(context.Background(), userID)
	if acct.ReservedBalance != 0 {
		t.Fatalf("reserved = %.2f, want 0", acct.ReservedBalance)
	}
}

func TestCreateOrder_LimitBuyBelowAskStaysOpen(t *testing.T) {
	userID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()

	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	_, _ = walletSvc.AdminCredit(context.Background(), primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: 100000})

	orderRepo := NewMemoryRepository()
	orderRepo.orders = nil
	svc := NewService(
		orderRepo,
		&stubMarketSvc{market: &markets.Market{ID: marketID}, bid: 50.75, ask: 51.75, ok: true},
		&stubMatchSvc{match: &matches.Match{Status: "live", Innings: 1, BallsLeft: 42}},
		walletSvc,
		executions.NewService(executions.NewMemoryRepository()),
	)

	order, err := svc.CreateOrder(context.Background(), userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: marketID.Hex(),
		Strike:   130,
		Side:     "buy",
		Quantity: 10,
		Price:    19.87,
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.Status != StatusOpen {
		t.Fatalf("status = %q, want open", order.Status)
	}

	acct, _ := walletSvc.GetWallet(context.Background(), userID)
	if acct.ReservedBalance != 198.7 {
		t.Fatalf("reserved = %.2f, want 198.70", acct.ReservedBalance)
	}
}

func TestCreateOrder_InsufficientBalanceRejected(t *testing.T) {
	userID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()

	svc := NewService(
		NewMemoryRepository(),
		&stubMarketSvc{market: &markets.Market{ID: marketID}, bid: 50, ask: 51, ok: true},
		&stubMatchSvc{match: &matches.Match{Status: "live", Innings: 1, BallsLeft: 42}},
		wallet.NewService(wallet.NewMemoryRepository()),
		executions.NewService(executions.NewMemoryRepository()),
	)

	_, err := svc.CreateOrder(context.Background(), userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: marketID.Hex(),
		Strike:   130,
		Side:     "buy",
		Quantity: 10,
		Price:    100,
	})
	if err == nil || err != ErrInsufficientBalance {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
}

func TestCancelOrder_ReleasesReservedBalance(t *testing.T) {
	userID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()

	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	_, _ = walletSvc.AdminCredit(context.Background(), primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: 100000})

	orderRepo := NewMemoryRepository()
	orderRepo.orders = nil
	svc := NewService(
		orderRepo,
		&stubMarketSvc{market: &markets.Market{ID: marketID}, bid: 50, ask: 51, ok: true},
		&stubMatchSvc{match: &matches.Match{Status: "live", Innings: 1, BallsLeft: 42}},
		walletSvc,
		executions.NewService(executions.NewMemoryRepository()),
	)

	order, err := svc.CreateOrder(context.Background(), userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: marketID.Hex(),
		Strike:   130,
		Side:     "buy",
		Quantity: 10,
		Price:    19.87,
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	_, err = svc.CancelOrder(context.Background(), order.ID, userID)
	if err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}

	acct, _ := walletSvc.GetWallet(context.Background(), userID)
	if acct.ReservedBalance != 0 {
		t.Fatalf("reserved = %.2f, want 0 after cancel", acct.ReservedBalance)
	}
	if acct.AvailableBalance != 100000 {
		t.Fatalf("available = %.2f, want 100000", acct.AvailableBalance)
	}
}

func TestMatchLimitOrder(t *testing.T) {
	price, ok := matchLimitOrder("buy", 52, 50, 51.75)
	if !ok || price != 51.75 {
		t.Fatalf("buy match = %.2f/%v, want 51.75/true", price, ok)
	}
	_, ok = matchLimitOrder("buy", 19.87, 50, 51.75)
	if ok {
		t.Fatal("buy below ask should not match")
	}
}
