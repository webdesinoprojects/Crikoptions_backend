package orders

import (
	"context"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
)

// capturePublisher records broadcast messages by topic for assertions.
type capturePublisher struct {
	mu   sync.Mutex
	msgs map[string][]map[string]any
}

func (p *capturePublisher) Publish(topic string, data any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.msgs == nil {
		p.msgs = map[string][]map[string]any{}
	}
	if m, ok := data.(map[string]any); ok {
		p.msgs[topic] = append(p.msgs[topic], m)
	}
}

func (p *capturePublisher) last(topic string) (map[string]any, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	list := p.msgs[topic]
	if len(list) == 0 {
		return nil, false
	}
	return list[len(list)-1], true
}

// execPositions implements PositionView by deriving live lots from executions.
type execPositions struct {
	exec         *executions.Service
	ltp          float64
	closeTarget  PositionSnapshot
	closeTargets []PositionSnapshot
	hasTarget    bool
}

func (p *execPositions) PositionFor(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) (PositionSnapshot, bool) {
	lots := p.exec.OpenLongQty(ctx, userID, matchID, marketID, strike)
	status := "open"
	if lots == 0 {
		status = "closed"
	}
	return PositionSnapshot{MatchID: matchID, MarketID: marketID, Strike: strike, Lots: lots, LTP: p.ltp, Status: status}, true
}

func (p *execPositions) ResolveCloseTarget(_ context.Context, _ primitive.ObjectID, _ string) (PositionSnapshot, bool) {
	return p.closeTarget, p.hasTarget
}

func (p *execPositions) OpenCloseTargets(_ context.Context, _ primitive.ObjectID) ([]PositionSnapshot, error) {
	return p.closeTargets, nil
}

type exitFixture struct {
	svc       *Service
	walletSvc *wallet.Service
	execSvc   *executions.Service
	pub       *capturePublisher
	market    *stubMarketSvc
	posView   *execPositions
	userID    primitive.ObjectID
	marketID  primitive.ObjectID
}

func newExitFixture(t *testing.T, startBalance float64) *exitFixture {
	t.Helper()
	userID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()

	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	_, _ = walletSvc.AdminCredit(context.Background(), primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: startBalance, Reason: "seed"})

	execSvc := executions.NewService(executions.NewMemoryRepository())
	orderRepo := NewMemoryRepository()
	orderRepo.orders = nil

	market := &stubMarketSvc{
		market: &markets.Market{ID: marketID, Status: markets.MarketStatusActive},
		bid:    34.35,
		ask:    34.85,
		ok:     true,
	}
	pub := &capturePublisher{}
	posView := &execPositions{exec: execSvc, ltp: 48.78}

	svc := NewService(
		orderRepo,
		market,
		&stubMatchSvc{match: &matches.Match{Status: "live", Innings: 1, CurrentScore: 85, WicketsLost: 2, BallsLeft: 42}},
		walletSvc,
		execSvc,
		posView,
		pub,
	)

	return &exitFixture{
		svc:       svc,
		walletSvc: walletSvc,
		execSvc:   execSvc,
		pub:       pub,
		market:    market,
		posView:   posView,
		userID:    userID,
		marketID:  marketID,
	}
}

func (f *exitFixture) buy(t *testing.T, strike float64, qty int) {
	t.Helper()
	order, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   strike,
		Side:     "buy",
		Type:     OrderTypeMarket,
		Quantity: qty,
	})
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	if order.Status != StatusExecuted {
		t.Fatalf("buy status = %q, want executed", order.Status)
	}
}

func (f *exitFixture) openQty(strike float64) int {
	return f.execSvc.OpenLongQty(context.Background(), f.userID, "1", f.marketID.Hex(), strike)
}

func (f *exitFixture) balance(t *testing.T) float64 {
	t.Helper()
	acct, err := f.walletSvc.GetWallet(context.Background(), f.userID)
	if err != nil {
		t.Fatalf("wallet: %v", err)
	}
	return acct.AvailableBalance
}

// 1. Full exit: long 10 @ 34.85, MARKET sell 10 @ bid 48.78.
func TestExit_FullMarketSell(t *testing.T) {
	f := newExitFixture(t, 100000)
	f.buy(t, 130, 10)

	// Price moves up before the exit.
	f.market.bid = 48.78
	f.market.ask = 49.28

	order, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   130,
		Side:     "sell",
		Type:     OrderTypeMarket,
		Quantity: 10,
	})
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	if order.Status != StatusExecuted || order.FilledQuantity != 10 {
		t.Fatalf("sell status/filled = %q/%d, want executed/10", order.Status, order.FilledQuantity)
	}
	if order.AverageFillPrice != 48.78 {
		t.Fatalf("avg fill = %.2f, want 48.78", order.AverageFillPrice)
	}
	if got := f.openQty(130); got != 0 {
		t.Fatalf("open lots = %d, want 0", got)
	}
	// Start 100000 - buy 348.50 + sell 487.80 = 100139.30 (realized 139.30).
	if bal := f.balance(t); bal != 100139.30 {
		t.Fatalf("balance = %.2f, want 100139.30", bal)
	}

	msg, ok := f.pub.last("user:" + f.userID.Hex() + ":positions")
	if !ok {
		t.Fatal("no positions broadcast")
	}
	if msg["lots"].(int) != 0 || msg["status"].(string) != "closed" {
		t.Fatalf("positions msg = %+v, want lots 0 / closed", msg)
	}
	omsg, ok := f.pub.last("user:" + f.userID.Hex() + ":orders")
	if !ok {
		t.Fatal("no orders broadcast")
	}
	if omsg["side"].(string) != "SELL" || omsg["status"].(string) != StatusExecuted {
		t.Fatalf("orders msg = %+v, want SELL/executed", omsg)
	}
}

// 2. Partial exit: long 15, sell 5 LIMIT @ 49 (marketable).
func TestExit_PartialLimitSell(t *testing.T) {
	f := newExitFixture(t, 100000)
	f.buy(t, 130, 15)

	f.market.bid = 49
	f.market.ask = 49.5

	order, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   130,
		Side:     "sell",
		Type:     OrderTypeLimit,
		Quantity: 5,
		Price:    49,
	})
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	if order.Status != StatusExecuted || order.FilledQuantity != 5 {
		t.Fatalf("sell status/filled = %q/%d, want executed/5", order.Status, order.FilledQuantity)
	}
	if got := f.openQty(130); got != 10 {
		t.Fatalf("open lots = %d, want 10", got)
	}
}

// 3. Working exit: long 10, sell LIMIT @ 60 when bid is 48 -> open; cancel restores nothing changed.
func TestExit_WorkingLimitThenCancel(t *testing.T) {
	f := newExitFixture(t, 100000)
	f.buy(t, 130, 10)

	f.market.bid = 48
	f.market.ask = 49

	order, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   130,
		Side:     "sell",
		Type:     OrderTypeLimit,
		Quantity: 10,
		Price:    60,
	})
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	if order.Status != StatusOpen {
		t.Fatalf("status = %q, want open", order.Status)
	}
	if got := f.openQty(130); got != 10 {
		t.Fatalf("open lots = %d, want 10 (unchanged)", got)
	}

	cancelled, err := f.svc.CancelOrder(context.Background(), order.ID, f.userID)
	if err != nil || cancelled == nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.Status != StatusCancelled {
		t.Fatalf("cancel status = %q, want cancelled", cancelled.Status)
	}
	if got := f.openQty(130); got != 10 {
		t.Fatalf("open lots after cancel = %d, want 10", got)
	}
}

// 4. Oversell: long 10, sell 15 -> 400 with exact message, no state change.
func TestExit_OversellRejected(t *testing.T) {
	f := newExitFixture(t, 100000)
	f.buy(t, 130, 10)

	f.market.bid = 48
	_, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   130,
		Side:     "sell",
		Type:     OrderTypeMarket,
		Quantity: 15,
	})
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Status != 400 || apiErr.Message != "Cannot sell 15 lots; only 10 open" {
		t.Fatalf("err = %d/%q, want 400/'Cannot sell 15 lots; only 10 open'", apiErr.Status, apiErr.Message)
	}
	if got := f.openQty(130); got != 10 {
		t.Fatalf("open lots = %d, want 10 (unchanged)", got)
	}
}

// 5. No position: sell strike with no holding -> 400, no state change.
func TestExit_NoPositionRejected(t *testing.T) {
	f := newExitFixture(t, 100000)

	f.market.bid = 48
	_, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   140,
		Side:     "sell",
		Type:     OrderTypeMarket,
		Quantity: 5,
	})
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Status != 400 || apiErr.Message != "No open position for strike 140" {
		t.Fatalf("err = %d/%q, want 400/'No open position for strike 140'", apiErr.Status, apiErr.Message)
	}
}

// 6. BUY flow unchanged: opening a position still works and accrues lots.
func TestExit_BuyFlowUnchanged(t *testing.T) {
	f := newExitFixture(t, 100000)
	f.buy(t, 130, 10)
	if got := f.openQty(130); got != 10 {
		t.Fatalf("open lots = %d, want 10", got)
	}
	// Buy must not emit sell broadcasts.
	if _, ok := f.pub.last("user:" + f.userID.Hex() + ":orders"); ok {
		t.Fatal("buy should not broadcast sell order updates")
	}
}

// Close endpoint path: defaults quantity to full lots and exits via SELL.
func TestExit_ClosePositionEndpoint(t *testing.T) {
	f := newExitFixture(t, 100000)
	f.buy(t, 130, 10)

	f.market.bid = 48.78
	f.market.ask = 49.28
	f.posView.closeTarget = PositionSnapshot{MatchID: "1", MarketID: f.marketID.Hex(), Strike: 130, Lots: 10, Status: "open"}
	f.posView.hasTarget = true

	order, err := f.svc.ClosePosition(context.Background(), f.userID, "anyid", OrderTypeMarket, 0, 0)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if order.Status != StatusExecuted || order.FilledQuantity != 10 {
		t.Fatalf("close status/filled = %q/%d, want executed/10", order.Status, order.FilledQuantity)
	}
	if got := f.openQty(130); got != 0 {
		t.Fatalf("open lots = %d, want 0", got)
	}
}

func TestExit_CloseAllPositions(t *testing.T) {
	f := newExitFixture(t, 100000)
	f.buy(t, 130, 10)
	f.buy(t, 140, 5)

	f.market.bid = 48.78
	f.market.ask = 49.28
	f.posView.closeTargets = []PositionSnapshot{
		{MatchID: "1", MarketID: f.marketID.Hex(), Strike: 130, Lots: 10, Status: "open"},
		{MatchID: "1", MarketID: f.marketID.Hex(), Strike: 140, Lots: 5, Status: "open"},
	}

	result, err := f.svc.CloseAllPositions(context.Background(), f.userID, OrderTypeMarket)
	if err != nil {
		t.Fatalf("close all: %v", err)
	}
	if result.Requested != 2 || result.Submitted != 2 || result.Failed != 0 {
		t.Fatalf("result = requested %d submitted %d failed %d, want 2/2/0", result.Requested, result.Submitted, result.Failed)
	}
	if len(result.Orders) != 2 {
		t.Fatalf("orders = %d, want 2", len(result.Orders))
	}
	if got := f.openQty(130); got != 0 {
		t.Fatalf("open lots strike 130 = %d, want 0", got)
	}
	if got := f.openQty(140); got != 0 {
		t.Fatalf("open lots strike 140 = %d, want 0", got)
	}
}

// No bid for a MARKET sell returns the explicit "No bid available".
func TestExit_NoBidRejected(t *testing.T) {
	f := newExitFixture(t, 100000)
	f.buy(t, 130, 10)

	f.market.bid = 0 // no liquidity
	_, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   130,
		Side:     "sell",
		Type:     OrderTypeMarket,
		Quantity: 5,
	})
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Message != "No bid available" {
		t.Fatalf("message = %q, want 'No bid available'", apiErr.Message)
	}
}
