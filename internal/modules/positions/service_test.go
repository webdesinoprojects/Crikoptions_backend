package positions

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/orders"
)

type stubOrderReader struct {
	orders []orders.Order
}

func (s *stubOrderReader) GetByUserID(_ context.Context, userID primitive.ObjectID, status, _ string) []orders.Order {
	var out []orders.Order
	for _, o := range s.orders {
		if !userID.IsZero() && o.UserID != userID {
			continue
		}
		if status != "" && o.Status != status {
			continue
		}
		out = append(out, o)
	}
	return out
}

func (s *stubOrderReader) List(_ context.Context, f orders.OrderFilter) []orders.Order {
	var out []orders.Order
	for _, o := range s.orders {
		if !f.UserID.IsZero() && o.UserID != f.UserID {
			continue
		}
		if f.Status != "" && o.Status != f.Status {
			continue
		}
		if f.MatchID != "" && o.MatchID != f.MatchID {
			continue
		}
		if f.MarketID != "" && o.MarketID != f.MarketID {
			continue
		}
		if f.Side != "" && o.Side != f.Side {
			continue
		}
		out = append(out, o)
	}
	return out
}

type stubMarketReader struct {
	ltps map[string]float64
}

func (s *stubMarketReader) GetMarketByID(_ context.Context, id string) (*markets.Market, error) {
	ltp, ok := s.ltps[id]
	if !ok {
		return nil, nil
	}
	return &markets.Market{ID: primitive.NewObjectID(), LTP: ltp}, nil
}

func TestAggregate_OpenLong(t *testing.T) {
	uid := primitive.NewObjectID()
	now := time.Now().UTC()
	orderList := []orders.Order{
		{UserID: uid, MatchID: "1", MarketID: "m1", Side: "buy", Quantity: 50, Price: 155, Status: "executed", CreatedAt: now, UpdatedAt: now},
	}
	svc := NewService(&stubOrderReader{orders: orderList}, &stubMarketReader{ltps: map[string]float64{"m1": 160}})

	open, err := svc.GetUserOpenPositions(context.Background(), uid)
	if err != nil {
		t.Fatalf("GetUserOpenPositions: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("expected 1 open position, got %d", len(open))
	}
	p := open[0]
	if p.Status != "open" {
		t.Errorf("status = %q, want open", p.Status)
	}
	if p.Lots != 50 {
		t.Errorf("lots = %d, want 50", p.Lots)
	}
	if p.BuyPrice != 155 {
		t.Errorf("buyPrice = %v, want 155", p.BuyPrice)
	}
	if p.LTP != 160 {
		t.Errorf("ltp = %v, want 160", p.LTP)
	}
	// Unrealized PnL = (160 - 155) * 50 = 250
	if p.PnL != 250 {
		t.Errorf("pnl = %v, want 250", p.PnL)
	}
}

func TestAggregate_ClosedPosition(t *testing.T) {
	uid := primitive.NewObjectID()
	now := time.Now().UTC()
	orderList := []orders.Order{
		{UserID: uid, MatchID: "1", MarketID: "m1", Side: "buy", Quantity: 50, Price: 100, Status: "executed", CreatedAt: now, UpdatedAt: now},
		{UserID: uid, MatchID: "1", MarketID: "m1", Side: "sell", Quantity: 50, Price: 110, Status: "executed", CreatedAt: now, UpdatedAt: now},
	}
	svc := NewService(&stubOrderReader{orders: orderList}, &stubMarketReader{ltps: map[string]float64{"m1": 115}})

	open, _ := svc.GetUserOpenPositions(context.Background(), uid)
	if len(open) != 0 {
		t.Errorf("expected no open positions, got %d", len(open))
	}
	closed, _ := svc.GetUserClosedPositions(context.Background(), uid)
	if len(closed) != 1 {
		t.Fatalf("expected 1 closed position, got %d", len(closed))
	}
	p := closed[0]
	if p.Status != "closed" {
		t.Errorf("status = %q, want closed", p.Status)
	}
	if p.Lots != 0 {
		t.Errorf("lots = %d, want 0", p.Lots)
	}
	if p.BuyPrice != 100 {
		t.Errorf("buyPrice = %v, want 100", p.BuyPrice)
	}
	if p.SellPrice != 110 {
		t.Errorf("sellPrice = %v, want 110", p.SellPrice)
	}
	// Realized PnL = (110 - 100) * 50 = 500
	if p.PnL != 500 {
		t.Errorf("pnl = %v, want 500", p.PnL)
	}
}

func TestAggregate_PartiallyClosed(t *testing.T) {
	uid := primitive.NewObjectID()
	now := time.Now().UTC()
	orderList := []orders.Order{
		{UserID: uid, MatchID: "1", MarketID: "m1", Side: "buy", Quantity: 100, Price: 100, Status: "executed", CreatedAt: now, UpdatedAt: now},
		{UserID: uid, MatchID: "1", MarketID: "m1", Side: "sell", Quantity: 40, Price: 110, Status: "executed", CreatedAt: now, UpdatedAt: now},
	}
	svc := NewService(&stubOrderReader{orders: orderList}, &stubMarketReader{ltps: map[string]float64{"m1": 120}})

	open, _ := svc.GetUserOpenPositions(context.Background(), uid)
	if len(open) != 1 {
		t.Fatalf("expected 1 open position, got %d", len(open))
	}
	p := open[0]
	if p.Lots != 60 {
		t.Errorf("lots = %d, want 60 (100 buy - 40 sell)", p.Lots)
	}
	// BuyPrice = 100, SellPrice = 110. Unrealized PnL = (LTP - BuyPrice) * |lots| = (120-100)*60 = 1200
	if p.PnL != 1200 {
		t.Errorf("pnl = %v, want 1200", p.PnL)
	}
}

func TestAggregate_OpenShort(t *testing.T) {
	uid := primitive.NewObjectID()
	now := time.Now().UTC()
	orderList := []orders.Order{
		{UserID: uid, MatchID: "1", MarketID: "m1", Side: "sell", Quantity: 50, Price: 200, Status: "executed", CreatedAt: now, UpdatedAt: now},
	}
	svc := NewService(&stubOrderReader{orders: orderList}, &stubMarketReader{ltps: map[string]float64{"m1": 195}})

	open, _ := svc.GetUserOpenPositions(context.Background(), uid)
	if len(open) != 1 {
		t.Fatalf("expected 1 open position, got %d", len(open))
	}
	p := open[0]
	if p.Lots != -50 {
		t.Errorf("lots = %d, want -50", p.Lots)
	}
	// Short PnL = (SellPrice - LTP) * |lots| = (200-195)*50 = 250
	if p.PnL != 250 {
		t.Errorf("pnl = %v, want 250", p.PnL)
	}
}

func TestAggregate_AdminList(t *testing.T) {
	u1 := primitive.NewObjectID()
	u2 := primitive.NewObjectID()
	now := time.Now().UTC()
	orderList := []orders.Order{
		{UserID: u1, MatchID: "1", MarketID: "m1", Side: "buy", Quantity: 10, Price: 100, Status: "executed", CreatedAt: now, UpdatedAt: now},
		{UserID: u2, MatchID: "1", MarketID: "m1", Side: "buy", Quantity: 20, Price: 105, Status: "executed", CreatedAt: now, UpdatedAt: now},
		{UserID: u1, MatchID: "1", MarketID: "m2", Side: "buy", Quantity: 5, Price: 50, Status: "executed", CreatedAt: now, UpdatedAt: now},
	}
	svc := NewService(&stubOrderReader{orders: orderList}, &stubMarketReader{ltps: map[string]float64{"m1": 100, "m2": 50}})

	all, err := svc.ListAdminPositions(context.Background(), PositionFilter{})
	if err != nil {
		t.Fatalf("ListAdminPositions: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 positions across all users, got %d", len(all))
	}

	// Filter by user
	u1Hex := u1.Hex()
	byUser, _ := svc.ListAdminPositions(context.Background(), PositionFilter{UserID: u1Hex})
	if len(byUser) != 2 {
		t.Errorf("expected 2 positions for u1, got %d", len(byUser))
	}
	for _, p := range byUser {
		if p.UserID != u1 {
			t.Errorf("got position for wrong user: %v", p.UserID)
		}
	}
}
