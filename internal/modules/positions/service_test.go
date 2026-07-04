package positions

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
)

type stubExecutionReader struct {
	items []executions.Execution
}

func (s *stubExecutionReader) List(_ context.Context, filter executions.Filter) []executions.Execution {
	var out []executions.Execution
	for _, e := range s.items {
		if !filter.UserID.IsZero() && e.UserID != filter.UserID {
			continue
		}
		if filter.MatchID != "" && e.MatchID != filter.MatchID {
			continue
		}
		if filter.MarketID != "" && e.MarketID != filter.MarketID {
			continue
		}
		out = append(out, e)
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
	items := []executions.Execution{
		{UserID: uid, MatchID: "1", MarketID: "m1", Strike: 130, Side: "buy", Quantity: 50, Price: 155, CreatedAt: now},
	}
	svc := NewService(&stubExecutionReader{items: items}, &stubMarketReader{ltps: map[string]float64{"m1": 160}})

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
	if p.PnL != 250 {
		t.Errorf("pnl = %v, want 250", p.PnL)
	}
}

func TestAggregate_OpenShort(t *testing.T) {
	uid := primitive.NewObjectID()
	now := time.Now().UTC()
	items := []executions.Execution{
		{UserID: uid, MatchID: "1", MarketID: "m1", Strike: 130, Side: "sell", Quantity: 20, Price: 50, CreatedAt: now},
	}
	svc := NewService(&stubExecutionReader{items: items}, &stubMarketReader{ltps: map[string]float64{"m1": 45}})

	open, err := svc.GetUserOpenPositions(context.Background(), uid)
	if err != nil {
		t.Fatalf("GetUserOpenPositions: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("expected 1 open position, got %d", len(open))
	}
	p := open[0]
	if p.Side != "SELL" {
		t.Errorf("side = %q, want SELL", p.Side)
	}
	if p.Lots != -20 {
		t.Errorf("lots = %d, want -20", p.Lots)
	}
	if p.SellPrice != 50 {
		t.Errorf("sellPrice = %v, want 50", p.SellPrice)
	}
	if p.PnL != 100 {
		t.Errorf("pnl = %v, want 100", p.PnL)
	}
}

func TestAggregate_ClosedPosition(t *testing.T) {
	uid := primitive.NewObjectID()
	now := time.Now().UTC()
	items := []executions.Execution{
		{UserID: uid, MatchID: "1", MarketID: "m1", Strike: 130, Side: "buy", Quantity: 50, Price: 100, CreatedAt: now},
		{UserID: uid, MatchID: "1", MarketID: "m1", Strike: 130, Side: "sell", Quantity: 50, Price: 110, CreatedAt: now},
	}
	svc := NewService(&stubExecutionReader{items: items}, &stubMarketReader{ltps: map[string]float64{"m1": 115}})

	open, _ := svc.GetUserOpenPositions(context.Background(), uid)
	if len(open) != 0 {
		t.Errorf("expected no open positions, got %d", len(open))
	}
	closed, _ := svc.GetUserClosedPositions(context.Background(), uid)
	if len(closed) != 1 {
		t.Fatalf("expected 1 closed position, got %d", len(closed))
	}
	p := closed[0]
	if p.PnL != 500 {
		t.Errorf("pnl = %v, want 500", p.PnL)
	}
}

func TestAggregate_ClosedShortPositionKeepsSellSide(t *testing.T) {
	uid := primitive.NewObjectID()
	now := time.Now().UTC()
	items := []executions.Execution{
		{UserID: uid, MatchID: "1", MarketID: "m1", Strike: 130, Side: "buy", Quantity: 10, Price: 40, CreatedAt: now.Add(time.Minute)},
		{UserID: uid, MatchID: "1", MarketID: "m1", Strike: 130, Side: "sell", Quantity: 10, Price: 50, CreatedAt: now},
	}
	svc := NewService(&stubExecutionReader{items: items}, &stubMarketReader{ltps: map[string]float64{"m1": 45}})

	open, _ := svc.GetUserOpenPositions(context.Background(), uid)
	if len(open) != 0 {
		t.Errorf("expected no open positions, got %d", len(open))
	}
	closed, _ := svc.GetUserClosedPositions(context.Background(), uid)
	if len(closed) != 1 {
		t.Fatalf("expected 1 closed position, got %d", len(closed))
	}
	p := closed[0]
	if p.Side != "SELL" {
		t.Errorf("side = %q, want SELL", p.Side)
	}
	if p.RealizedPnL != 100 || p.PnL != 100 {
		t.Errorf("realized/pnl = %.2f/%.2f, want 100/100", p.RealizedPnL, p.PnL)
	}
}

func TestAggregate_PartiallyClosed(t *testing.T) {
	uid := primitive.NewObjectID()
	now := time.Now().UTC()
	items := []executions.Execution{
		{UserID: uid, MatchID: "1", MarketID: "m1", Strike: 130, Side: "buy", Quantity: 100, Price: 100, CreatedAt: now},
		{UserID: uid, MatchID: "1", MarketID: "m1", Strike: 130, Side: "sell", Quantity: 40, Price: 110, CreatedAt: now},
	}
	svc := NewService(&stubExecutionReader{items: items}, &stubMarketReader{ltps: map[string]float64{"m1": 120}})

	open, _ := svc.GetUserOpenPositions(context.Background(), uid)
	if len(open) != 1 {
		t.Fatalf("expected 1 open position, got %d", len(open))
	}
	if open[0].Lots != 60 {
		t.Errorf("lots = %d, want 60", open[0].Lots)
	}
	if open[0].PnL != 1200 {
		t.Errorf("pnl = %v, want 1200", open[0].PnL)
	}
}

func TestAggregate_AdminList(t *testing.T) {
	u1 := primitive.NewObjectID()
	u2 := primitive.NewObjectID()
	now := time.Now().UTC()
	items := []executions.Execution{
		{UserID: u1, MatchID: "1", MarketID: "m1", Strike: 130, Side: "buy", Quantity: 10, Price: 100, CreatedAt: now},
		{UserID: u2, MatchID: "1", MarketID: "m1", Strike: 130, Side: "buy", Quantity: 20, Price: 105, CreatedAt: now},
		{UserID: u1, MatchID: "1", MarketID: "m2", Strike: 140, Side: "buy", Quantity: 5, Price: 50, CreatedAt: now},
	}
	svc := NewService(&stubExecutionReader{items: items}, &stubMarketReader{ltps: map[string]float64{"m1": 100, "m2": 50}})

	all, err := svc.ListAdminPositions(context.Background(), PositionFilter{})
	if err != nil {
		t.Fatalf("ListAdminPositions: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 positions across all users, got %d", len(all))
	}
}

func TestListUserPositions_FiltersByMarket(t *testing.T) {
	uid := primitive.NewObjectID()
	otherUser := primitive.NewObjectID()
	now := time.Now().UTC()
	items := []executions.Execution{
		{UserID: uid, MatchID: "1", MarketID: "m1", Strike: 130, Side: "buy", Quantity: 10, Price: 100, CreatedAt: now},
		{UserID: uid, MatchID: "1", MarketID: "m2", Strike: 140, Side: "buy", Quantity: 20, Price: 100, CreatedAt: now},
		{UserID: otherUser, MatchID: "1", MarketID: "m2", Strike: 150, Side: "buy", Quantity: 30, Price: 100, CreatedAt: now},
	}
	svc := NewService(&stubExecutionReader{items: items}, &stubMarketReader{ltps: map[string]float64{"m1": 105, "m2": 110}})

	filtered, err := svc.ListUserPositions(context.Background(), uid, PositionFilter{
		Status:   "open",
		MarketID: "m2",
	})
	if err != nil {
		t.Fatalf("ListUserPositions: %v", err)
	}
	if len(filtered) != 1 {
		t.Fatalf("expected 1 filtered position, got %d", len(filtered))
	}
	if filtered[0].UserID != uid {
		t.Fatalf("userID = %s, want %s", filtered[0].UserID.Hex(), uid.Hex())
	}
	if filtered[0].MarketID != "m2" {
		t.Fatalf("marketID = %q, want m2", filtered[0].MarketID)
	}
}
