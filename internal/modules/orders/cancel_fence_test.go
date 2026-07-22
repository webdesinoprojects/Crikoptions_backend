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

// A trading-gate job drains asynchronously. Because the gate now reopens as
// soon as a live poll arrives, a job can still be running when the user places
// a fresh order — and that order must survive. Orders resting from before the
// gate closed still get cancelled.
func TestCancelProviderWorkingOrdersSkipsOrdersPlacedAfterGateClosed(t *testing.T) {
	const gateTradingVersion int64 = 12

	matchID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()
	userID := primitive.NewObjectID()
	match := &matches.Match{ID: matchID, DataSource: matches.DataSourceSportmonks}
	market := markets.Market{ID: marketID, MatchID: matchID.Hex()}

	walletSvc := fundedWallet(t, userID, 1000)
	repo := NewMemoryRepository()
	stale, err := repo.Create(context.Background(), Order{
		UserID: userID, MatchID: matchID.Hex(), MarketID: marketID.Hex(),
		Side: "buy", Type: OrderTypeLimit, Quantity: 1, Price: 10,
		TradingVersion: gateTradingVersion - 1,
	})
	if err != nil {
		t.Fatalf("create stale order: %v", err)
	}
	fresh, err := repo.Create(context.Background(), Order{
		UserID: userID, MatchID: matchID.Hex(), MarketID: marketID.Hex(),
		Side: "buy", Type: OrderTypeLimit, Quantity: 1, Price: 10,
		TradingVersion: gateTradingVersion + 1,
	})
	if err != nil {
		t.Fatalf("create fresh order: %v", err)
	}
	reserveFor(t, walletSvc, userID, *stale)
	reserveFor(t, walletSvc, userID, *fresh)

	svc := NewService(
		repo,
		&stubMarketSvc{market: &market},
		&stubMatchSvc{match: match},
		walletSvc,
		executions.NewService(executions.NewMemoryRepository()),
		nil,
		nil,
	)

	cancelled, err := svc.CancelProviderWorkingOrders(context.Background(), matchID.Hex(), gateTradingVersion)
	if err != nil {
		t.Fatalf("CancelProviderWorkingOrders: %v", err)
	}
	if cancelled != 1 {
		t.Fatalf("cancelled = %d, want 1", cancelled)
	}

	staleAfter, err := repo.GetByID(context.Background(), stale.ID)
	if err != nil {
		t.Fatalf("reload stale order: %v", err)
	}
	if staleAfter.Status != StatusCancelled {
		t.Fatalf("pre-gate order status = %q, want %q", staleAfter.Status, StatusCancelled)
	}

	freshAfter, err := repo.GetByID(context.Background(), fresh.ID)
	if err != nil {
		t.Fatalf("reload fresh order: %v", err)
	}
	if freshAfter.Status != StatusOpen {
		t.Fatalf("post-reopen order status = %q, want it left open", freshAfter.Status)
	}
}

// Settlement and void pass 0: everything resting must go, whatever version.
func TestCancelProviderWorkingOrdersZeroFenceCancelsEverything(t *testing.T) {
	matchID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()
	userID := primitive.NewObjectID()
	match := &matches.Match{ID: matchID, DataSource: matches.DataSourceSportmonks}
	market := markets.Market{ID: marketID, MatchID: matchID.Hex()}

	walletSvc := fundedWallet(t, userID, 1000)
	repo := NewMemoryRepository()
	for _, version := range []int64{0, 5, 99} {
		order, err := repo.Create(context.Background(), Order{
			UserID: userID, MatchID: matchID.Hex(), MarketID: marketID.Hex(),
			Side: "buy", Type: OrderTypeLimit, Quantity: 1, Price: 10,
			TradingVersion: version,
		})
		if err != nil {
			t.Fatalf("create order v%d: %v", version, err)
		}
		reserveFor(t, walletSvc, userID, *order)
	}

	svc := NewService(
		repo,
		&stubMarketSvc{market: &market},
		&stubMatchSvc{match: match},
		walletSvc,
		executions.NewService(executions.NewMemoryRepository()),
		nil,
		nil,
	)

	cancelled, err := svc.CancelProviderWorkingOrders(context.Background(), matchID.Hex(), 0)
	if err != nil {
		t.Fatalf("CancelProviderWorkingOrders: %v", err)
	}
	if cancelled != 3 {
		t.Fatalf("cancelled = %d, want 3", cancelled)
	}
}

func fundedWallet(t *testing.T, userID primitive.ObjectID, amount float64) *wallet.Service {
	t.Helper()
	svc := wallet.NewService(wallet.NewMemoryRepository())
	if _, err := svc.AdminCredit(context.Background(), primitive.NewObjectID(), userID,
		wallet.FundingRequest{Amount: amount, Reason: "seed"}); err != nil {
		t.Fatalf("fund wallet: %v", err)
	}
	return svc
}

// Mirror what order placement does, so cancellation has margin to release.
func reserveFor(t *testing.T, svc *wallet.Service, userID primitive.ObjectID, order Order) {
	t.Helper()
	amount := order.RemainingReservedAmount()
	if amount <= 0 {
		return
	}
	if _, err := svc.ReserveOrderMargin(context.Background(), userID, amount, order.ID.Hex(), "test reserve"); err != nil {
		t.Fatalf("reserve margin: %v", err)
	}
}
