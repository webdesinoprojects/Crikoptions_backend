package positions

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
)

type stubMarkets struct{ ltp float64 }

func (s stubMarkets) GetMarketByID(_ context.Context, id string) (*markets.Market, error) {
	oid, _ := primitive.ObjectIDFromHex(id)
	return &markets.Market{ID: oid, LTP: s.ltp}, nil
}

func seedFill(t *testing.T, exec *executions.Service, userID primitive.ObjectID, marketID string, strike float64, side string, price float64, qty int) {
	t.Helper()
	_, err := exec.Create(context.Background(), executions.Execution{
		UserID:   userID,
		OrderID:  primitive.NewObjectID(),
		MatchID:  "1",
		MarketID: marketID,
		Strike:   strike,
		Side:     side,
		Price:    price,
		Quantity: qty,
	})
	if err != nil {
		t.Fatalf("seed fill: %v", err)
	}
}

func TestRealizedPnL_PartialExitKeepsOpenWithRealizedSlice(t *testing.T) {
	userID := primitive.NewObjectID()
	marketID := primitive.NewObjectID().Hex()

	exec := executions.NewService(executions.NewMemoryRepository())
	seedFill(t, exec, userID, marketID, 130, "buy", 34.85, 15)
	seedFill(t, exec, userID, marketID, 130, "sell", 49, 5)

	svc := NewService(exec, stubMarkets{ltp: 50}, nil, nil)
	open, err := svc.GetUserOpenPositions(context.Background(), userID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("open positions = %d, want 1", len(open))
	}
	p := open[0]
	if p.Lots != 10 {
		t.Fatalf("lots = %d, want 10", p.Lots)
	}
	// Realized on closed slice: (49 - 34.85) * 5 = 70.75.
	if p.RealizedPnL != 70.75 {
		t.Fatalf("realizedPnl = %.2f, want 70.75", p.RealizedPnL)
	}
}

func TestRealizedPnL_FullExitClosesPosition(t *testing.T) {
	userID := primitive.NewObjectID()
	marketID := primitive.NewObjectID().Hex()

	exec := executions.NewService(executions.NewMemoryRepository())
	seedFill(t, exec, userID, marketID, 140, "buy", 34.85, 10)
	seedFill(t, exec, userID, marketID, 140, "sell", 48.78, 10)

	svc := NewService(exec, stubMarkets{ltp: 48}, nil, nil)

	open, _ := svc.GetUserOpenPositions(context.Background(), userID)
	if len(open) != 0 {
		t.Fatalf("open positions = %d, want 0 after full exit", len(open))
	}

	closed, err := svc.GetUserClosedPositions(context.Background(), userID)
	if err != nil {
		t.Fatalf("closed: %v", err)
	}
	if len(closed) != 1 {
		t.Fatalf("closed positions = %d, want 1", len(closed))
	}
	c := closed[0]
	if c.Lots != 0 {
		t.Fatalf("lots = %d, want 0", c.Lots)
	}
	// Realized: (48.78 - 34.85) * 10 = 139.30.
	if c.RealizedPnL != 139.30 {
		t.Fatalf("realizedPnl = %.2f, want 139.30", c.RealizedPnL)
	}
}
