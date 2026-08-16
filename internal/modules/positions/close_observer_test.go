package positions

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
)

type captureCloseObserver struct {
	events []CloseFill
}

func (c *captureCloseObserver) OnProfitableClose(_ context.Context, event CloseFill) error {
	c.events = append(c.events, event)
	return nil
}

func TestApplyExecutionNotifiesProfitableCloseWithClocks(t *testing.T) {
	ctx := context.Background()
	userID := primitive.NewObjectID()
	repo := NewMemoryProjectionRepository()
	svc := NewServiceWithProjection(nil, nil, repo, nil, nil)
	obs := &captureCloseObserver{}
	svc.SetCloseObserver(obs)
	now := time.Now().UTC()
	fillID := primitive.NewObjectID()

	if _, err := svc.ApplyExecution(ctx, executions.Execution{
		ID: fillID, UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130,
		Side: "buy", Quantity: 1, Price: 40, CreatedAt: now,
		OversText: "5.0", LegalBalls: 30, Innings: 1, Format: "T20", ClockAt: now,
	}, ""); err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(obs.events) != 0 {
		t.Fatalf("open must not count as a close: %+v", obs.events)
	}

	closeID := primitive.NewObjectID()
	if _, err := svc.ApplyExecution(ctx, executions.Execution{
		ID: closeID, UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130,
		Side: "sell", Quantity: 1, Price: 55, CreatedAt: now.Add(time.Second),
		OversText: "5.4", LegalBalls: 34, Innings: 1, Format: "T20", ClockAt: now.Add(time.Second),
	}, ""); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(obs.events) != 1 {
		t.Fatalf("events=%d, want 1 profitable close", len(obs.events))
	}
	ev := obs.events[0]
	if ev.FillID != closeID.Hex() || ev.RealizedPnL <= 0 {
		t.Fatalf("event=%+v", ev)
	}
	if ev.Open.LegalBalls != 30 || ev.Close.LegalBalls != 34 {
		t.Fatalf("clocks open=%+v close=%+v", ev.Open, ev.Close)
	}
}

type failingCloseObserver struct{}

func (failingCloseObserver) OnProfitableClose(context.Context, CloseFill) error {
	return errors.New("challenge store down")
}

func TestApplyExecutionStillClosesWhenChallengeObserverFails(t *testing.T) {
	ctx := context.Background()
	userID := primitive.NewObjectID()
	repo := NewMemoryProjectionRepository()
	svc := NewServiceWithProjection(nil, nil, repo, nil, nil)
	svc.SetCloseObserver(failingCloseObserver{})
	now := time.Now().UTC()

	if _, err := svc.ApplyExecution(ctx, executions.Execution{
		ID: primitive.NewObjectID(), UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130,
		Side: "buy", Quantity: 1, Price: 40, CreatedAt: now,
	}, ""); err != nil {
		t.Fatalf("open: %v", err)
	}
	transition, err := svc.ApplyExecution(ctx, executions.Execution{
		ID: primitive.NewObjectID(), UserID: userID, MatchID: "1", MarketID: "m1", Strike: 130,
		Side: "sell", Quantity: 1, Price: 55, CreatedAt: now.Add(time.Second),
	}, "")
	if err != nil {
		t.Fatalf("profitable close must not fail when challenges are down: %v", err)
	}
	if transition.NetLotsBefore != 1 {
		t.Fatalf("transition=%+v, want a real close", transition)
	}
}
