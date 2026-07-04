package positions

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
)

func TestProjectionLifecycle_PartialCloseFullCloseAndReopen(t *testing.T) {
	ctx := context.Background()
	userID := primitive.NewObjectID()
	repo := NewMemoryProjectionRepository()
	svc := NewServiceWithProjection(&stubExecutionReader{}, &stubMarketReader{ltps: map[string]float64{"m1": 120}}, repo)
	now := time.Now().UTC()

	fills := []executions.Execution{
		{UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130, Side: "buy", Quantity: 100, Price: 100, CreatedAt: now},
		{UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130, Side: "sell", Quantity: 40, Price: 110, CreatedAt: now.Add(time.Minute)},
	}
	for _, fill := range fills {
		if err := repo.ApplyExecution(ctx, fill); err != nil {
			t.Fatalf("ApplyExecution: %v", err)
		}
	}

	open, err := svc.GetUserOpenPositions(ctx, userID)
	if err != nil {
		t.Fatalf("GetUserOpenPositions: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("open len = %d, want 1", len(open))
	}
	if open[0].Lots != 60 || open[0].MatchedLots != 40 || open[0].RealizedPnL != 400 || open[0].PnL != 1200 {
		t.Fatalf("partial position = %+v, want lots 60 matched 40 realized 400 pnl 1200", open[0])
	}

	if err := repo.ApplyExecution(ctx, executions.Execution{
		UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130, Side: "sell", Quantity: 60, Price: 115, CreatedAt: now.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("ApplyExecution close: %v", err)
	}

	open, err = svc.GetUserOpenPositions(ctx, userID)
	if err != nil {
		t.Fatalf("GetUserOpenPositions after close: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("open len after close = %d, want 0", len(open))
	}
	closed, err := svc.GetUserClosedPositions(ctx, userID)
	if err != nil {
		t.Fatalf("GetUserClosedPositions: %v", err)
	}
	if len(closed) != 1 {
		t.Fatalf("closed len = %d, want 1", len(closed))
	}
	closedID := closed[0].ID
	if closed[0].Lots != 0 || closed[0].MatchedLots != 100 || closed[0].RealizedPnL != 1300 {
		t.Fatalf("closed position = %+v, want matched 100 realized 1300", closed[0])
	}

	if err := repo.ApplyExecution(ctx, executions.Execution{
		UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130, Side: "buy", Quantity: 10, Price: 90, CreatedAt: now.Add(3 * time.Minute),
	}); err != nil {
		t.Fatalf("ApplyExecution reopen: %v", err)
	}
	open, err = svc.GetUserOpenPositions(ctx, userID)
	if err != nil {
		t.Fatalf("GetUserOpenPositions after reopen: %v", err)
	}
	if len(open) != 1 || open[0].Lots != 10 {
		t.Fatalf("reopened open = %+v, want one 10-lot position", open)
	}
	if open[0].ID == closedID {
		t.Fatalf("reopened position reused closed id %s", closedID)
	}
}

func TestProjectionLifecycle_OpenShortCoverAndReopen(t *testing.T) {
	ctx := context.Background()
	userID := primitive.NewObjectID()
	repo := NewMemoryProjectionRepository()
	svc := NewServiceWithProjection(&stubExecutionReader{}, &stubMarketReader{ltps: map[string]float64{"m1": 45}}, repo)
	now := time.Now().UTC()

	if err := repo.ApplyExecution(ctx, executions.Execution{
		UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130, Side: "sell", Quantity: 10, Price: 50, CreatedAt: now,
	}); err != nil {
		t.Fatalf("ApplyExecution short open: %v", err)
	}

	open, err := svc.GetUserOpenPositions(ctx, userID)
	if err != nil {
		t.Fatalf("GetUserOpenPositions: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("open len = %d, want 1", len(open))
	}
	if open[0].Side != "SELL" || open[0].Lots != -10 || open[0].SellPrice != 50 || open[0].PnL != 50 {
		t.Fatalf("short position = %+v, want SELL lots -10 sellPrice 50 pnl 50", open[0])
	}

	if err := repo.ApplyExecution(ctx, executions.Execution{
		UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130, Side: "buy", Quantity: 10, Price: 40, CreatedAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("ApplyExecution cover: %v", err)
	}

	open, err = svc.GetUserOpenPositions(ctx, userID)
	if err != nil {
		t.Fatalf("GetUserOpenPositions after cover: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("open len after cover = %d, want 0", len(open))
	}
	closed, err := svc.GetUserClosedPositions(ctx, userID)
	if err != nil {
		t.Fatalf("GetUserClosedPositions: %v", err)
	}
	if len(closed) != 1 {
		t.Fatalf("closed len = %d, want 1", len(closed))
	}
	closedID := closed[0].ID
	if closed[0].Side != "SELL" || closed[0].Lots != 0 || closed[0].MatchedLots != 10 || closed[0].RealizedPnL != 100 {
		t.Fatalf("closed short = %+v, want SELL matched 10 realized 100", closed[0])
	}

	if err := repo.ApplyExecution(ctx, executions.Execution{
		UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130, Side: "sell", Quantity: 3, Price: 55, CreatedAt: now.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("ApplyExecution reopen short: %v", err)
	}
	open, err = svc.GetUserOpenPositions(ctx, userID)
	if err != nil {
		t.Fatalf("GetUserOpenPositions after reopen: %v", err)
	}
	if len(open) != 1 || open[0].Side != "SELL" || open[0].Lots != -3 {
		t.Fatalf("reopened open = %+v, want one 3-lot short", open)
	}
	if open[0].ID == closedID {
		t.Fatalf("reopened short reused closed id %s", closedID)
	}
}
