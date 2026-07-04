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
	summary := p.exec.PositionSummary(ctx, userID, matchID, marketID, strike)
	lots := summary.NetLots
	status := "open"
	if lots == 0 {
		status = "closed"
	}
	return PositionSnapshot{
		MatchID:   matchID,
		MarketID:  marketID,
		Strike:    strike,
		Lots:      lots,
		BuyPrice:  summary.AvgBuyPrice,
		SellPrice: summary.AvgSellPrice,
		LTP:       p.ltp,
		Status:    status,
	}, true
}

func (p *execPositions) ResolveCloseTarget(_ context.Context, _ primitive.ObjectID, _ string) (PositionSnapshot, bool) {
	return p.closeTarget, p.hasTarget
}

func (p *execPositions) OpenCloseTargets(_ context.Context, _ primitive.ObjectID) ([]PositionSnapshot, error) {
	return p.closeTargets, nil
}

func (p *execPositions) ListOpenByMatch(_ context.Context, _ string) ([]PositionSnapshot, error) {
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

func (f *exitFixture) netQty(strike float64) int {
	return f.execSvc.PositionSummary(context.Background(), f.userID, "1", f.marketID.Hex(), strike).NetLots
}

func (f *exitFixture) balance(t *testing.T) float64 {
	t.Helper()
	acct, err := f.walletSvc.GetWallet(context.Background(), f.userID)
	if err != nil {
		t.Fatalf("wallet: %v", err)
	}
	return acct.AvailableBalance
}

func (f *exitFixture) account(t *testing.T) *wallet.Account {
	t.Helper()
	acct, err := f.walletSvc.GetWallet(context.Background(), f.userID)
	if err != nil {
		t.Fatalf("wallet: %v", err)
	}
	return acct
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

// 4. AUTO sell beyond long closes the long first, then opens a short.
func TestShortSelling_SellBeyondLongFlipsShort(t *testing.T) {
	f := newExitFixture(t, 100000)
	f.buy(t, 130, 10)

	f.market.bid = 48
	order, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   130,
		Side:     "sell",
		Type:     OrderTypeMarket,
		Quantity: 15,
	})
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	if order.Status != StatusExecuted || order.FilledQuantity != 15 {
		t.Fatalf("order status/filled = %q/%d, want executed/15", order.Status, order.FilledQuantity)
	}
	if order.PositionIntent != "SELL_CLOSE_AND_OPEN_SHORT" {
		t.Fatalf("position intent = %q, want SELL_CLOSE_AND_OPEN_SHORT", order.PositionIntent)
	}
	if got := f.netQty(130); got != -5 {
		t.Fatalf("net lots = %d, want -5", got)
	}
	acct := f.account(t)
	if acct.CashBalance != 100371.50 || acct.ReservedBalance != 480 || acct.AvailableBalance != 99891.50 {
		t.Fatalf("wallet = cash %.2f reserved %.2f available %.2f, want 100371.50/480/99891.50", acct.CashBalance, acct.ReservedBalance, acct.AvailableBalance)
	}
}

// 5. AUTO sell from flat opens a short and reserves short proceeds plus initial margin.
func TestShortSelling_SellFromFlatOpensShort(t *testing.T) {
	f := newExitFixture(t, 100000)

	f.market.bid = 48
	f.market.ask = 49
	order, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   140,
		Side:     "sell",
		Type:     OrderTypeMarket,
		Quantity: 5,
	})
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	if order.Status != StatusExecuted || order.PositionIntent != "SELL_TO_OPEN_SHORT" {
		t.Fatalf("order status/intent = %q/%q, want executed/SELL_TO_OPEN_SHORT", order.Status, order.PositionIntent)
	}
	if order.ReservedAmount != 240 || order.ReservedQuantity != 5 {
		t.Fatalf("reserved = %.2f/%d, want 240/5", order.ReservedAmount, order.ReservedQuantity)
	}
	if got := f.netQty(140); got != -5 {
		t.Fatalf("net lots = %d, want -5", got)
	}
	acct := f.account(t)
	if acct.CashBalance != 100240 || acct.ReservedBalance != 480 || acct.AvailableBalance != 99760 {
		t.Fatalf("wallet = cash %.2f reserved %.2f available %.2f, want 100240/480/99760", acct.CashBalance, acct.ReservedBalance, acct.AvailableBalance)
	}

	msg, ok := f.pub.last("user:" + f.userID.Hex() + ":positions")
	if !ok {
		t.Fatal("no positions broadcast")
	}
	if msg["lots"].(int) != -5 || msg["sellPrice"].(float64) != 48 || msg["status"].(string) != "open" {
		t.Fatalf("positions msg = %+v, want lots -5 / sellPrice 48 / open", msg)
	}
}

// 6. BUY flow unchanged: opening a position still works and accrues lots.
func TestExit_BuyFlowUnchanged(t *testing.T) {
	f := newExitFixture(t, 100000)
	f.buy(t, 130, 10)
	if got := f.openQty(130); got != 10 {
		t.Fatalf("open lots = %d, want 10", got)
	}
	msg, ok := f.pub.last("user:" + f.userID.Hex() + ":orders")
	if !ok {
		t.Fatal("buy should broadcast order updates")
	}
	if msg["side"].(string) != "BUY" || msg["positionIntent"].(string) != "BUY_TO_OPEN_LONG" {
		t.Fatalf("order msg = %+v, want BUY/BUY_TO_OPEN_LONG", msg)
	}
}

func TestShortSelling_BuyCoversShortAndReleasesCollateral(t *testing.T) {
	f := newExitFixture(t, 100000)

	f.market.bid = 48
	f.market.ask = 49
	_, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   140,
		Side:     "sell",
		Type:     OrderTypeMarket,
		Quantity: 10,
	})
	if err != nil {
		t.Fatalf("open short: %v", err)
	}

	f.market.bid = 39.5
	f.market.ask = 40
	partial, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   140,
		Side:     "buy",
		Type:     OrderTypeMarket,
		Quantity: 4,
	})
	if err != nil {
		t.Fatalf("partial cover: %v", err)
	}
	if partial.PositionIntent != "BUY_TO_COVER" || f.netQty(140) != -6 {
		t.Fatalf("partial intent/net = %q/%d, want BUY_TO_COVER/-6", partial.PositionIntent, f.netQty(140))
	}
	acct := f.account(t)
	if acct.CashBalance != 100320 || acct.ReservedBalance != 576 || acct.AvailableBalance != 99744 {
		t.Fatalf("partial wallet = cash %.2f reserved %.2f available %.2f, want 100320/576/99744", acct.CashBalance, acct.ReservedBalance, acct.AvailableBalance)
	}

	full, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   140,
		Side:     "buy",
		Type:     OrderTypeMarket,
		Quantity: 6,
	})
	if err != nil {
		t.Fatalf("full cover: %v", err)
	}
	if full.PositionIntent != "BUY_TO_COVER" || f.netQty(140) != 0 {
		t.Fatalf("full intent/net = %q/%d, want BUY_TO_COVER/0", full.PositionIntent, f.netQty(140))
	}
	acct = f.account(t)
	if acct.CashBalance != 100080 || acct.ReservedBalance != 0 || acct.AvailableBalance != 100080 {
		t.Fatalf("full wallet = cash %.2f reserved %.2f available %.2f, want 100080/0/100080", acct.CashBalance, acct.ReservedBalance, acct.AvailableBalance)
	}
}

func TestShortSelling_BuyBeyondShortFlipsLong(t *testing.T) {
	f := newExitFixture(t, 100000)

	f.market.bid = 48
	f.market.ask = 49
	_, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   140,
		Side:     "sell",
		Type:     OrderTypeMarket,
		Quantity: 5,
	})
	if err != nil {
		t.Fatalf("open short: %v", err)
	}

	f.market.bid = 39.5
	f.market.ask = 40
	order, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   140,
		Side:     "buy",
		Type:     OrderTypeMarket,
		Quantity: 8,
	})
	if err != nil {
		t.Fatalf("flip long: %v", err)
	}
	if order.PositionIntent != "BUY_COVER_AND_OPEN_LONG" {
		t.Fatalf("position intent = %q, want BUY_COVER_AND_OPEN_LONG", order.PositionIntent)
	}
	if got := f.netQty(140); got != 3 {
		t.Fatalf("net lots = %d, want 3", got)
	}
	acct := f.account(t)
	if acct.CashBalance != 99920 || acct.ReservedBalance != 0 || acct.AvailableBalance != 99920 {
		t.Fatalf("wallet = cash %.2f reserved %.2f available %.2f, want 99920/0/99920", acct.CashBalance, acct.ReservedBalance, acct.AvailableBalance)
	}
}

func TestPositionEffectCloseRejectsOpeningOrFlipping(t *testing.T) {
	f := newExitFixture(t, 100000)

	f.market.bid = 48
	_, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:        "1",
		MarketID:       f.marketID.Hex(),
		Strike:         140,
		Side:           "sell",
		Type:           OrderTypeMarket,
		PositionEffect: PositionEffectClose,
		Quantity:       5,
	})
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Message != "No open position for strike 140" {
		t.Fatalf("flat close err = %v, want no open position APIError", err)
	}

	f.buy(t, 130, 10)
	_, err = f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:        "1",
		MarketID:       f.marketID.Hex(),
		Strike:         130,
		Side:           "sell",
		Type:           OrderTypeMarket,
		PositionEffect: PositionEffectClose,
		Quantity:       15,
	})
	apiErr, ok = err.(*APIError)
	if !ok || apiErr.Message != "Cannot close 15 lots; only 10 long" {
		t.Fatalf("long over-close err = %v, want insufficient long APIError", err)
	}

	_, err = f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   150,
		Side:     "sell",
		Type:     OrderTypeMarket,
		Quantity: 5,
	})
	if err != nil {
		t.Fatalf("open short: %v", err)
	}
	f.market.ask = 49
	_, err = f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:        "1",
		MarketID:       f.marketID.Hex(),
		Strike:         150,
		Side:           "buy",
		Type:           OrderTypeMarket,
		PositionEffect: PositionEffectClose,
		Quantity:       6,
	})
	apiErr, ok = err.(*APIError)
	if !ok || apiErr.Message != "Cannot cover 6 lots; only 5 short" {
		t.Fatalf("short over-cover err = %v, want insufficient short APIError", err)
	}
}

func TestShortSelling_PendingShortLimitReservesAndCancelReleases(t *testing.T) {
	f := newExitFixture(t, 100000)

	f.market.bid = 48
	f.market.ask = 49
	order, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   140,
		Side:     "sell",
		Type:     OrderTypeLimit,
		Quantity: 10,
		Price:    60,
	})
	if err != nil {
		t.Fatalf("open short limit: %v", err)
	}
	if order.Status != StatusOpen || order.ReservedAmount != 600 || order.ReservedQuantity != 10 {
		t.Fatalf("order status/reserved = %q/%.2f/%d, want open/600/10", order.Status, order.ReservedAmount, order.ReservedQuantity)
	}
	acct := f.account(t)
	if acct.ReservedBalance != 600 || acct.AvailableBalance != 99400 {
		t.Fatalf("wallet after reserve = reserved %.2f available %.2f, want 600/99400", acct.ReservedBalance, acct.AvailableBalance)
	}

	cancelled, err := f.svc.CancelOrder(context.Background(), order.ID, f.userID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled == nil || cancelled.Status != StatusCancelled {
		t.Fatalf("cancelled = %+v, want cancelled order", cancelled)
	}
	acct = f.account(t)
	if acct.ReservedBalance != 0 || acct.AvailableBalance != 100000 {
		t.Fatalf("wallet after cancel = reserved %.2f available %.2f, want 0/100000", acct.ReservedBalance, acct.AvailableBalance)
	}
}

func TestShortSelling_InsufficientMarginRejected(t *testing.T) {
	f := newExitFixture(t, 100)

	f.market.bid = 50
	_, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   140,
		Side:     "sell",
		Type:     OrderTypeMarket,
		Quantity: 3,
	})
	if err != ErrInsufficientBalance {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	if got := f.netQty(140); got != 0 {
		t.Fatalf("net lots = %d, want 0", got)
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
	_, err := f.svc.CreateOrder(context.Background(), f.userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: f.marketID.Hex(),
		Strike:   150,
		Side:     "sell",
		Type:     OrderTypeMarket,
		Quantity: 5,
	})
	if err != nil {
		t.Fatalf("open short: %v", err)
	}

	f.market.bid = 48.78
	f.market.ask = 49.28
	f.posView.closeTargets = []PositionSnapshot{
		{MatchID: "1", MarketID: f.marketID.Hex(), Strike: 130, Lots: 10, Status: "open"},
		{MatchID: "1", MarketID: f.marketID.Hex(), Strike: 140, Lots: 5, Status: "open"},
		{MatchID: "1", MarketID: f.marketID.Hex(), Strike: 150, Lots: -5, Status: "open"},
	}

	result, err := f.svc.CloseAllPositions(context.Background(), f.userID, OrderTypeMarket)
	if err != nil {
		t.Fatalf("close all: %v", err)
	}
	if result.Requested != 3 || result.Submitted != 3 || result.Failed != 0 {
		t.Fatalf("result = requested %d submitted %d failed %d, want 3/3/0", result.Requested, result.Submitted, result.Failed)
	}
	if len(result.Orders) != 3 {
		t.Fatalf("orders = %d, want 3", len(result.Orders))
	}
	if got := f.openQty(130); got != 0 {
		t.Fatalf("open lots strike 130 = %d, want 0", got)
	}
	if got := f.openQty(140); got != 0 {
		t.Fatalf("open lots strike 140 = %d, want 0", got)
	}
	if got := f.netQty(150); got != 0 {
		t.Fatalf("net lots strike 150 = %d, want 0", got)
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
