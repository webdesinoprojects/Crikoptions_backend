package positions

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
)

func TestProjectionApplyExecution_EnforcesConstraintAndRevision(t *testing.T) {
	ctx := context.Background()
	userID := primitive.NewObjectID()
	repo := NewMemoryProjectionRepository()
	now := time.Now().UTC()

	opened, err := repo.ApplyExecution(ctx, executions.Execution{
		UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130,
		Side: "buy", Quantity: 10, Price: 100, CreatedAt: now,
	}, ProjectionConstraint{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if opened.Before.Lots != 0 || opened.After.Lots != 10 || opened.After.Revision != 1 {
		t.Fatalf("open transition = %+v", opened)
	}

	minimum := 5
	closed, err := repo.ApplyExecution(ctx, executions.Execution{
		UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130,
		Side: "sell", Quantity: 5, Price: 110, CreatedAt: now.Add(time.Second),
	}, ProjectionConstraint{MinLots: &minimum})
	if err != nil {
		t.Fatalf("partial close: %v", err)
	}
	if closed.Before.Lots != 10 || closed.After.Lots != 5 || closed.After.Revision != 2 {
		t.Fatalf("close transition = %+v", closed)
	}

	minimum = 6
	_, err = repo.ApplyExecution(ctx, executions.Execution{
		UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130,
		Side: "sell", Quantity: 6, Price: 110, CreatedAt: now.Add(2 * time.Second),
	}, ProjectionConstraint{MinLots: &minimum})
	if !errors.Is(err, ErrProjectionConstraint) {
		t.Fatalf("over-close error = %v, want ErrProjectionConstraint", err)
	}
	projection, err := repo.GetOpenByKey(ctx, userID, "1", "m1", 130)
	if err != nil {
		t.Fatalf("GetOpenByKey: %v", err)
	}
	if projection == nil || projection.Lots != 5 || projection.Revision != 2 {
		t.Fatalf("projection after rejected close = %+v", projection)
	}
}

func TestProjectionLifecycle_ReopenWithCollidingIDReplacesClosedProjection(t *testing.T) {
	ctx := context.Background()
	userID := primitive.NewObjectID()
	repo := NewMemoryProjectionRepository()
	createdAt := time.Now().UTC()

	for _, execution := range []executions.Execution{
		{UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130, Side: "buy", Quantity: 5, Price: 100, CreatedAt: createdAt},
		{UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130, Side: "sell", Quantity: 5, Price: 110, CreatedAt: createdAt},
	} {
		if _, err := repo.ApplyExecution(ctx, execution, ProjectionConstraint{}); err != nil {
			t.Fatalf("close lifecycle: %v", err)
		}
	}

	reopened, err := repo.ApplyExecution(ctx, executions.Execution{
		UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130,
		Side: "buy", Quantity: 3, Price: 90, CreatedAt: createdAt,
	}, ProjectionConstraint{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Before.Lots != 0 || reopened.After.Lots != 3 || reopened.After.Status != "open" || reopened.After.Revision != 3 {
		t.Fatalf("reopen transition = %+v", reopened)
	}

	all, err := repo.List(ctx, ProjectionFilter{UserID: userID, MatchID: "1", MarketID: "m1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 || all[0].Status != "open" || all[0].Lots != 3 {
		t.Fatalf("projections = %+v, want one reopened projection", all)
	}
}

func TestProjectionLifecycle_PartialCloseFullCloseAndReopen(t *testing.T) {
	ctx := context.Background()
	userID := primitive.NewObjectID()
	repo := NewMemoryProjectionRepository()
	svc := NewServiceWithProjection(&stubExecutionReader{}, &stubMarketReader{ltps: map[string]float64{"m1": 120}}, repo, nil, nil)
	now := time.Now().UTC()

	fills := []executions.Execution{
		{UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130, Side: "buy", Quantity: 100, Price: 100, CreatedAt: now},
		{UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130, Side: "sell", Quantity: 40, Price: 110, CreatedAt: now.Add(time.Minute)},
	}
	for _, fill := range fills {
		if _, err := repo.ApplyExecution(ctx, fill, ProjectionConstraint{}); err != nil {
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

	if _, err := repo.ApplyExecution(ctx, executions.Execution{
		UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130, Side: "sell", Quantity: 60, Price: 115, CreatedAt: now.Add(2 * time.Minute),
	}, ProjectionConstraint{}); err != nil {
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

	if _, err := repo.ApplyExecution(ctx, executions.Execution{
		UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130, Side: "buy", Quantity: 10, Price: 90, CreatedAt: now.Add(3 * time.Minute),
	}, ProjectionConstraint{}); err != nil {
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
	svc := NewServiceWithProjection(&stubExecutionReader{}, &stubMarketReader{ltps: map[string]float64{"m1": 45}}, repo, nil, nil)
	now := time.Now().UTC()

	if _, err := repo.ApplyExecution(ctx, executions.Execution{
		UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130, Side: "sell", Quantity: 10, Price: 50, CreatedAt: now,
	}, ProjectionConstraint{}); err != nil {
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

	if _, err := repo.ApplyExecution(ctx, executions.Execution{
		UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130, Side: "buy", Quantity: 10, Price: 40, CreatedAt: now.Add(time.Minute),
	}, ProjectionConstraint{}); err != nil {
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

	if _, err := repo.ApplyExecution(ctx, executions.Execution{
		UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130, Side: "sell", Quantity: 3, Price: 55, CreatedAt: now.Add(2 * time.Minute),
	}, ProjectionConstraint{}); err != nil {
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

func TestProjectionTracksCurrentShortCollateralAcrossLongToShortFlip(t *testing.T) {
	ctx := context.Background()
	userID := primitive.NewObjectID()
	repo := NewMemoryProjectionRepository()
	now := time.Now().UTC()

	for i, fill := range []executions.Execution{
		{UserID: userID, MatchID: "1", MarketID: "m1", Strike: 100, Side: "buy", Quantity: 10, Price: 30},
		{UserID: userID, MatchID: "1", MarketID: "m1", Strike: 100, Side: "sell", Quantity: 5, Price: 100},
		{UserID: userID, MatchID: "1", MarketID: "m1", Strike: 100, Side: "sell", Quantity: 10, Price: 50},
	} {
		fill.CreatedAt = now.Add(time.Duration(i) * time.Second)
		if _, err := repo.ApplyExecution(ctx, fill, ProjectionConstraint{}); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}

	open, err := repo.GetOpenByKey(ctx, userID, "1", "m1", 100)
	if err != nil || open == nil {
		t.Fatalf("open short = %+v, err=%v", open, err)
	}
	if open.Lots != -5 || open.ShortCollateral != 500 {
		t.Fatalf("open short = %+v, want lots -5 and collateral 500", open)
	}
	covered, err := repo.ApplyExecution(ctx, executions.Execution{
		UserID: userID, MatchID: "1", MarketID: "m1", Strike: 100,
		Side: "buy", Quantity: 5, Price: 100, CreatedAt: now.Add(3 * time.Second),
	}, ProjectionConstraint{})
	if err != nil {
		t.Fatalf("cover: %v", err)
	}
	if covered.ShortCollateralRelease != 500 || covered.After.ShortCollateral != 0 || covered.After.Lots != 0 {
		t.Fatalf("cover transition = %+v, want exact 500 collateral release", covered)
	}
}
