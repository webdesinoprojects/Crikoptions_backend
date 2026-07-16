package orders

import (
	"context"
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
)

type stubMarketSvc struct {
	market     *markets.Market
	markets    []markets.Market
	bid        float64
	ask        float64
	ok         bool
	listErr    error
	lastInput  markets.PriceCalculationInput
	lastStrike float64
}

func (s *stubMarketSvc) GetMarketByID(_ context.Context, _ string) (*markets.Market, error) {
	return s.market, nil
}

func (s *stubMarketSvc) GetMarketsByMatchID(_ context.Context, _ string) []markets.Market {
	if len(s.markets) > 0 {
		return s.markets
	}
	if s.market != nil {
		return []markets.Market{*s.market}
	}
	return nil
}

func (s *stubMarketSvc) ListMarketsByMatchID(ctx context.Context, matchID string) ([]markets.Market, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.GetMarketsByMatchID(ctx, matchID), nil
}

func (s *stubMarketSvc) SetMarketStatus(_ context.Context, id, status string) (*markets.Market, error) {
	if s.market != nil && s.market.ID.Hex() == id {
		s.market.Status = status
		return s.market, nil
	}
	return s.market, nil
}

func (s *stubMarketSvc) StrikeQuote(input markets.PriceCalculationInput, strike float64) (float64, float64, bool) {
	s.lastInput = input
	s.lastStrike = strike
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

type capturePositionWriter struct {
	execs      []executions.Execution
	effects    []string
	transition PositionTransition
	err        error
}

// rollbackOrderRepository models the order-collection rollback provided by
// Mongo transactions for failure-path unit tests.
type rollbackOrderRepository struct {
	*MemoryRepository
}

func (r *rollbackOrderRepository) DoTx(ctx context.Context, fn func(context.Context) error) error {
	r.mu.RLock()
	ordersBefore := append([]Order(nil), r.orders...)
	compensationsBefore := make(map[string]ProviderVoidCompensation, len(r.voidCompensations))
	for key, value := range r.voidCompensations {
		compensationsBefore[key] = value
	}
	r.mu.RUnlock()

	err := fn(ctx)
	if err == nil {
		return nil
	}
	r.mu.Lock()
	r.orders = ordersBefore
	r.voidCompensations = compensationsBefore
	r.mu.Unlock()
	return err
}

func (c *capturePositionWriter) ApplyExecution(_ context.Context, exec executions.Execution, effect string) (PositionTransition, error) {
	c.execs = append(c.execs, exec)
	c.effects = append(c.effects, effect)
	return c.transition, c.err
}

func (c *capturePositionWriter) PositionFor(context.Context, primitive.ObjectID, string, string, float64) (PositionSnapshot, bool) {
	return PositionSnapshot{}, false
}

func (c *capturePositionWriter) ResolveCloseTarget(context.Context, primitive.ObjectID, string) (PositionSnapshot, bool) {
	return PositionSnapshot{}, false
}

func (c *capturePositionWriter) OpenCloseTargets(context.Context, primitive.ObjectID) ([]PositionSnapshot, error) {
	return nil, nil
}

func (c *capturePositionWriter) ListOpenByMatch(context.Context, string) ([]PositionSnapshot, error) {
	return nil, nil
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
		nil,
		nil,
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

func TestCreateOrder_ExecutedFillCallsPositionProjectionWriter(t *testing.T) {
	userID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()

	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	_, _ = walletSvc.AdminCredit(context.Background(), primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: 100000})

	execSvc := executions.NewService(executions.NewMemoryRepository())
	positionWriter := &capturePositionWriter{}
	orderRepo := NewMemoryRepository()
	orderRepo.orders = nil

	svc := NewService(
		orderRepo,
		&stubMarketSvc{
			market: &markets.Market{ID: marketID, Status: markets.MarketStatusActive},
			bid:    54,
			ask:    55,
			ok:     true,
		},
		&stubMatchSvc{match: &matches.Match{Status: "live", Innings: 1, BallsLeft: 42}},
		walletSvc,
		execSvc,
		positionWriter,
		nil,
	)

	_, err := svc.CreateOrder(context.Background(), userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: marketID.Hex(),
		Strike:   130,
		Side:     "buy",
		Type:     OrderTypeMarket,
		Quantity: 7,
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	if len(positionWriter.execs) != 1 {
		t.Fatalf("projection writer executions = %d, want 1", len(positionWriter.execs))
	}
	got := positionWriter.execs[0]
	if got.UserID != userID || got.MarketID != marketID.Hex() || got.Strike != 130 || got.Quantity != 7 || got.Price != 55 {
		t.Fatalf("projection writer exec = %+v", got)
	}
}

func TestCreateOrder_FillUsesAtomicPositionTransitionForShortCollateral(t *testing.T) {
	userID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()
	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	_, _ = walletSvc.AdminCredit(context.Background(), primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: 100000})

	execSvc := executions.NewService(executions.NewMemoryRepository())
	_, err := execSvc.Create(context.Background(), executions.Execution{
		UserID: userID, MatchID: "1", MarketID: marketID.Hex(), Strike: 130,
		Side: "buy", Price: 40, Quantity: 10,
	})
	if err != nil {
		t.Fatalf("seed execution: %v", err)
	}

	positionWriter := &capturePositionWriter{transition: PositionTransition{NetLotsBefore: 0}}
	orderRepo := NewMemoryRepository()
	orderRepo.orders = nil
	svc := NewService(
		orderRepo,
		&stubMarketSvc{market: &markets.Market{ID: marketID, Status: markets.MarketStatusActive}, bid: 48, ask: 49, ok: true},
		&stubMatchSvc{match: &matches.Match{Status: "live", Innings: 1, BallsLeft: 42}},
		walletSvc,
		execSvc,
		positionWriter,
		nil,
	)

	order, err := svc.CreateOrder(context.Background(), userID, CreateOrderRequest{
		MatchID: "1", MarketID: marketID.Hex(), Strike: 130,
		Side: "sell", Type: OrderTypeMarket, PositionEffect: PositionEffectAuto, Quantity: 5,
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.Status != StatusExecuted {
		t.Fatalf("status = %q, want executed", order.Status)
	}
	account, err := walletSvc.GetWallet(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetWallet: %v", err)
	}
	if account.CashBalance != 100240 || account.ReservedBalance != 480 || account.AvailableBalance != 99760 {
		t.Fatalf("wallet = %.2f/%.2f/%.2f, want 100240/480/99760", account.CashBalance, account.ReservedBalance, account.AvailableBalance)
	}
	if len(positionWriter.effects) != 1 || positionWriter.effects[0] != PositionEffectAuto {
		t.Fatalf("projection effects = %v, want [AUTO]", positionWriter.effects)
	}
}

func TestFillReconcilesReservationAgainstActualCrossReplicaPosition(t *testing.T) {
	for _, fillOrder := range []string{"sell-first", "buy-first"} {
		t.Run(fillOrder, func(t *testing.T) {
			ctx := context.Background()
			userID := primitive.NewObjectID()
			marketID := primitive.NewObjectID()
			walletSvc := wallet.NewService(wallet.NewMemoryRepository())
			if _, err := walletSvc.AdminCredit(ctx, primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: 1000}); err != nil {
				t.Fatal(err)
			}
			executionSvc := executions.NewService(executions.NewMemoryRepository())
			orderRepo := NewMemoryRepository()
			orderRepo.orders = nil
			svc := NewService(
				orderRepo,
				&stubMarketSvc{market: &markets.Market{ID: marketID, Status: markets.MarketStatusActive}},
				&stubMatchSvc{match: &matches.Match{Status: matches.StatusLive}},
				walletSvc, executionSvc, nil, nil,
			)

			buy, err := orderRepo.Create(ctx, Order{
				UserID: userID, MatchID: "1", MarketID: marketID.Hex(), Strike: 100,
				Side: "buy", Type: OrderTypeMarket, PositionEffect: PositionEffectAuto, PositionIntent: "BUY_TO_OPEN_LONG",
				Quantity: 1, Price: 100, ReservedAmount: 100, ReservedQuantity: 1, RemainingQuantity: 1, Status: StatusOpen,
			})
			if err != nil {
				t.Fatal(err)
			}
			sell, err := orderRepo.Create(ctx, Order{
				UserID: userID, MatchID: "1", MarketID: marketID.Hex(), Strike: 100,
				Side: "sell", Type: OrderTypeMarket, PositionEffect: PositionEffectAuto, PositionIntent: "SELL_TO_OPEN_SHORT",
				Quantity: 1, Price: 100, ReservedAmount: 100, ReservedQuantity: 1, RemainingQuantity: 1, Status: StatusOpen,
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, order := range []*Order{buy, sell} {
				if _, err := walletSvc.ReserveOrderMargin(ctx, userID, 100, order.ID.Hex(), "concurrent order reserve"); err != nil {
					t.Fatal(err)
				}
			}

			first, second := sell, buy
			if fillOrder == "buy-first" {
				first, second = buy, sell
			}
			if _, err := svc.applyFill(ctx, userID, first, 100, 1); err != nil {
				t.Fatalf("first fill: %v", err)
			}
			if _, err := svc.applyFill(ctx, userID, second, 100, 1); err != nil {
				t.Fatalf("second fill: %v", err)
			}

			account, err := walletSvc.GetWallet(ctx, userID)
			if err != nil {
				t.Fatal(err)
			}
			if account.CashBalance != 1000 || account.ReservedBalance != 0 || account.AvailableBalance != 1000 {
				t.Fatalf("wallet = %+v, want 1000/0/1000", account)
			}
			if got := executionSvc.NetLots(ctx, userID, "1", marketID.Hex(), 100); got != 0 {
				t.Fatalf("net lots = %d, want 0", got)
			}
		})
	}
}

func TestBuyToCoverDoesNotExposeLegacyReserveAndCannotSpendOtherMargin(t *testing.T) {
	cover := Order{
		Side: "buy", PositionIntent: "BUY_TO_COVER", Price: 200,
		Quantity: 1, RemainingQuantity: 1, Status: StatusOpen,
	}
	if got := cover.RemainingReservedAmount(); got != 0 {
		t.Fatalf("cover remaining reserve = %.2f, want 0", got)
	}

	ctx := context.Background()
	userID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()
	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	if _, err := walletSvc.AdminCredit(ctx, primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := walletSvc.ReserveOrderMargin(ctx, userID, 900, "other-order", "other reserved margin"); err != nil {
		t.Fatal(err)
	}
	orderRepo := NewMemoryRepository()
	orderRepo.orders = nil
	created, err := orderRepo.Create(ctx, Order{
		UserID: userID, MatchID: "1", MarketID: marketID.Hex(), Strike: 100,
		Side: "buy", Type: OrderTypeMarket, PositionEffect: PositionEffectAuto, PositionIntent: "BUY_TO_COVER",
		Quantity: 1, Price: 200, RemainingQuantity: 1, Status: StatusOpen,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(
		orderRepo, &stubMarketSvc{market: &markets.Market{ID: marketID, Status: markets.MarketStatusActive}},
		&stubMatchSvc{match: &matches.Match{Status: matches.StatusLive}}, walletSvc,
		executions.NewService(executions.NewMemoryRepository()), nil, nil,
	)
	if _, err := svc.applyFill(ctx, userID, created, 200, 1); !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("unreserved cross-state fill error = %v, want insufficient funds", err)
	}
}

func TestCreateOrderImmediateFillMarginFailureCancelsAndReleasesReserve(t *testing.T) {
	ctx := context.Background()
	userID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()
	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	if _, err := walletSvc.AdminCredit(ctx, primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: 100}); err != nil {
		t.Fatal(err)
	}

	memoryRepo := NewMemoryRepository()
	memoryRepo.orders = nil
	orderRepo := &rollbackOrderRepository{MemoryRepository: memoryRepo}
	executionSvc := executions.NewService(executions.NewMemoryRepository())
	svc := NewService(
		orderRepo,
		&stubMarketSvc{market: &markets.Market{ID: marketID, Status: markets.MarketStatusActive}, bid: 200, ask: 210, ok: true},
		&stubMatchSvc{match: &matches.Match{Status: matches.StatusLive, Innings: 1, BallsLeft: 42}},
		walletSvc, executionSvc, nil, nil,
	)

	_, err := svc.CreateOrder(ctx, userID, CreateOrderRequest{
		ClientOrderID: "margin-top-up-failure", MatchID: "1", MarketID: marketID.Hex(),
		Strike: 100, Side: "sell", Type: OrderTypeLimit, Quantity: 1, Price: 100,
	})
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("CreateOrder error = %v, want ErrInsufficientBalance", err)
	}
	all := orderRepo.GetAll(ctx)
	if len(all) != 1 || all[0].Status != StatusCancelled {
		t.Fatalf("orders = %+v, want one cancelled order", all)
	}
	account, err := walletSvc.GetWallet(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if account.CashBalance != 100 || account.ReservedBalance != 0 || account.AvailableBalance != 100 {
		t.Fatalf("wallet = %+v, want 100/0/100", account)
	}
	if fills := executionSvc.ListUserExecutions(ctx, userID, "", "", 10); len(fills) != 0 {
		t.Fatalf("executions = %d, want none", len(fills))
	}
}

func TestCreateOrderRejectsReservedInternalClientOrderIDBeforeReplay(t *testing.T) {
	for _, clientOrderID := range []string{"settlement:spoof", "void:spoof"} {
		t.Run(clientOrderID, func(t *testing.T) {
			ctx := context.Background()
			userID := primitive.NewObjectID()
			repo := NewMemoryRepository()
			repo.orders = nil
			if _, err := repo.Create(ctx, Order{
				ClientOrderID: clientOrderID, UserID: userID, MatchID: "provider-match",
				MarketID: primitive.NewObjectID().Hex(), Strike: 100, Side: "buy",
				Type: OrderTypeMarket, Quantity: 1, RemainingQuantity: 1, Status: StatusOpen,
			}); err != nil {
				t.Fatal(err)
			}
			svc := NewService(repo, nil, nil, nil, nil, nil, nil)
			_, err := svc.CreateOrder(ctx, userID, CreateOrderRequest{ClientOrderID: clientOrderID})
			if !errors.Is(err, ErrReservedClientOrderID) {
				t.Fatalf("CreateOrder error = %v, want ErrReservedClientOrderID", err)
			}
		})
	}
}

func TestCancelOrderRejectsInternalSyntheticOrders(t *testing.T) {
	for _, clientOrderID := range []string{"settlement:market:1:v1:close", "void:market:execution:0:v1:reverse"} {
		t.Run(clientOrderID, func(t *testing.T) {
			ctx := context.Background()
			userID := primitive.NewObjectID()
			repo := NewMemoryRepository()
			repo.orders = nil
			created, err := repo.Create(ctx, Order{
				ClientOrderID: clientOrderID, UserID: userID, MatchID: "provider-match",
				MarketID: primitive.NewObjectID().Hex(), Strike: 100, Side: "buy",
				Type: OrderTypeMarket, Quantity: 1, RemainingQuantity: 1, Status: StatusOpen,
			})
			if err != nil {
				t.Fatal(err)
			}
			svc := NewService(repo, nil, nil, nil, nil, nil, nil)
			if _, err := svc.CancelOrder(ctx, created.ID, userID); !errors.Is(err, ErrInternalOrderCancel) {
				t.Fatalf("CancelOrder error = %v, want ErrInternalOrderCancel", err)
			}
			current, err := repo.GetByID(ctx, created.ID)
			if err != nil || current == nil || current.Status != StatusOpen {
				t.Fatalf("internal order after cancellation attempt = %+v, err=%v", current, err)
			}
		})
	}
}

func TestCreateOrder_CloseFillCancelsWhenAtomicPositionChanged(t *testing.T) {
	userID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()
	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	_, _ = walletSvc.AdminCredit(context.Background(), primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: 100000})

	execSvc := executions.NewService(executions.NewMemoryRepository())
	_, err := execSvc.Create(context.Background(), executions.Execution{
		UserID: userID, MatchID: "1", MarketID: marketID.Hex(), Strike: 130,
		Side: "buy", Price: 40, Quantity: 5,
	})
	if err != nil {
		t.Fatalf("seed execution: %v", err)
	}

	positionWriter := &capturePositionWriter{err: ErrInsufficientPosition}
	orderRepo := NewMemoryRepository()
	orderRepo.orders = nil
	svc := NewService(
		orderRepo,
		&stubMarketSvc{market: &markets.Market{ID: marketID, Status: markets.MarketStatusActive}, bid: 48, ask: 49, ok: true},
		&stubMatchSvc{match: &matches.Match{Status: "live", Innings: 1, BallsLeft: 42}},
		walletSvc,
		execSvc,
		positionWriter,
		nil,
	)

	_, err = svc.CreateOrder(context.Background(), userID, CreateOrderRequest{
		MatchID: "1", MarketID: marketID.Hex(), Strike: 130,
		Side: "sell", Type: OrderTypeMarket, PositionEffect: PositionEffectClose, Quantity: 5,
	})
	if !errors.Is(err, ErrInsufficientPosition) {
		t.Fatalf("CreateOrder error = %v, want ErrInsufficientPosition", err)
	}
	orders := orderRepo.GetAll(context.Background())
	if len(orders) != 1 || orders[0].Status != StatusCancelled {
		t.Fatalf("orders = %+v, want one cancelled order", orders)
	}
	if fills := execSvc.ListUserExecutions(context.Background(), userID, "", "", 10); len(fills) != 1 {
		t.Fatalf("executions = %d, want only seeded execution", len(fills))
	}
}

func TestShortInitialMarginTopUpCapsOriginalReservation(t *testing.T) {
	order := Order{ReservedAmount: 240, ReservedQuantity: 5}
	if got := shortInitialMarginTopUp(order, 48, 10); got != 240 {
		t.Fatalf("top-up = %.2f, want 240", got)
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
		nil,
		nil,
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

func TestCreateOrder_MarketBuyFillsAtAsk(t *testing.T) {
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
			bid:    19.37,
			ask:    19.87,
			ok:     true,
		},
		&stubMatchSvc{match: &matches.Match{Status: "live", Innings: 1, CurrentScore: 85, WicketsLost: 2, BallsLeft: 42}},
		walletSvc,
		execSvc,
		nil,
		nil,
	)

	order, err := svc.CreateOrder(context.Background(), userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: marketID.Hex(),
		Strike:   130,
		Side:     "buy",
		Type:     OrderTypeMarket,
		Quantity: 10,
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.Status != StatusExecuted {
		t.Fatalf("status = %q, want %q", order.Status, StatusExecuted)
	}
	if order.Price != 19.87 || order.AverageFillPrice != 19.87 {
		t.Fatalf("price/avg = %.2f/%.2f, want 19.87/19.87", order.Price, order.AverageFillPrice)
	}
	if order.FilledQuantity != 10 || order.RemainingQuantity != 0 {
		t.Fatalf("filled/remaining = %d/%d, want 10/0", order.FilledQuantity, order.RemainingQuantity)
	}
}

func TestCreateOrder_UsesPricingSnapshotForReplayFill(t *testing.T) {
	userID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()

	walletRepo := wallet.NewMemoryRepository()
	walletSvc := wallet.NewService(walletRepo)
	_, _ = walletSvc.AdminCredit(context.Background(), primitive.NewObjectID(), userID, wallet.FundingRequest{
		Amount: 100000,
		Reason: "seed",
	})

	marketSvc := &stubMarketSvc{
		market: &markets.Market{ID: marketID, Status: markets.MarketStatusActive},
		bid:    21.25,
		ask:    21.75,
		ok:     true,
	}
	orderRepo := NewMemoryRepository()
	orderRepo.orders = nil

	svc := NewService(
		orderRepo,
		marketSvc,
		&stubMatchSvc{match: &matches.Match{Status: "live", Innings: 1, CurrentScore: 15, WicketsLost: 0, BallsLeft: 114}},
		walletSvc,
		executions.NewService(executions.NewMemoryRepository()),
		nil,
		nil,
	)

	snapshot := markets.PriceCalculationInput{
		Innings:      2,
		CurrentScore: 92,
		WicketsLost:  4,
		BallsBowled:  55,
		TargetScore:  161,
	}
	order, err := svc.CreateOrder(context.Background(), userID, CreateOrderRequest{
		MatchID:         "1",
		MarketID:        marketID.Hex(),
		Strike:          130,
		Side:            "buy",
		Type:            OrderTypeMarket,
		Quantity:        3,
		PricingSnapshot: &snapshot,
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.Status != StatusExecuted {
		t.Fatalf("status = %q, want %q", order.Status, StatusExecuted)
	}
	if marketSvc.lastInput.Innings != 2 ||
		marketSvc.lastInput.CurrentScore != 92 ||
		marketSvc.lastInput.WicketsLost != 4 ||
		marketSvc.lastInput.BallsBowled != 55 ||
		marketSvc.lastInput.TargetScore != 161 {
		t.Fatalf("pricing input = %+v, want replay snapshot", marketSvc.lastInput)
	}
	if marketSvc.lastStrike != 130 {
		t.Fatalf("strike = %.2f, want 130", marketSvc.lastStrike)
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
		nil,
		nil,
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

func TestPreviewOrder_ReturnsBackendNotionalAndBalance(t *testing.T) {
	userID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()

	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	_, _ = walletSvc.AdminCredit(context.Background(), primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: 100})

	svc := NewService(
		NewMemoryRepository(),
		&stubMarketSvc{market: &markets.Market{ID: marketID, Status: markets.MarketStatusActive}, bid: 50, ask: 51, ok: true},
		&stubMatchSvc{match: &matches.Match{Status: "live", Innings: 1, BallsLeft: 42}},
		walletSvc,
		executions.NewService(executions.NewMemoryRepository()),
		nil,
		nil,
	)

	preview, err := svc.PreviewOrder(context.Background(), userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: marketID.Hex(),
		Strike:   130,
		Side:     "buy",
		Type:     OrderTypeMarket,
		Quantity: 3,
	})
	if err != nil {
		t.Fatalf("PreviewOrder: %v", err)
	}
	if preview.Notional != 153 || preview.MarginRequired != 153 {
		t.Fatalf("notional/margin = %.2f/%.2f, want 153/153", preview.Notional, preview.MarginRequired)
	}
	if preview.AvailableBalance != 100 || preview.SufficientBalance {
		t.Fatalf("available/sufficient = %.2f/%v, want 100/false", preview.AvailableBalance, preview.SufficientBalance)
	}
	if !preview.WillExecuteNow || preview.ExecutablePrice != 51 {
		t.Fatalf("execute/price = %v/%.2f, want true/51", preview.WillExecuteNow, preview.ExecutablePrice)
	}

	account, _ := walletSvc.GetWallet(context.Background(), userID)
	if account.ReservedBalance != 0 || account.AvailableBalance != 100 {
		t.Fatalf("preview mutated wallet: reserved %.2f available %.2f", account.ReservedBalance, account.AvailableBalance)
	}
}

func TestPreviewOrder_ShortSellRequiresMargin(t *testing.T) {
	userID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()

	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	_, _ = walletSvc.AdminCredit(context.Background(), primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: 100})

	svc := NewService(
		NewMemoryRepository(),
		&stubMarketSvc{market: &markets.Market{ID: marketID, Status: markets.MarketStatusActive}, bid: 50, ask: 51, ok: true},
		&stubMatchSvc{match: &matches.Match{Status: "live", Innings: 1, BallsLeft: 42}},
		walletSvc,
		executions.NewService(executions.NewMemoryRepository()),
		nil,
		nil,
	)

	preview, err := svc.PreviewOrder(context.Background(), userID, CreateOrderRequest{
		MatchID:  "1",
		MarketID: marketID.Hex(),
		Strike:   130,
		Side:     "sell",
		Type:     OrderTypeMarket,
		Quantity: 3,
	})
	if err != nil {
		t.Fatalf("PreviewOrder: %v", err)
	}
	if preview.PositionIntent != "SELL_TO_OPEN_SHORT" || preview.PositionEffect != PositionEffectAuto {
		t.Fatalf("intent/effect = %q/%q, want SELL_TO_OPEN_SHORT/AUTO", preview.PositionIntent, preview.PositionEffect)
	}
	if preview.Notional != 150 || preview.MarginRequired != 150 {
		t.Fatalf("notional/margin = %.2f/%.2f, want 150/150", preview.Notional, preview.MarginRequired)
	}
	if preview.SufficientBalance {
		t.Fatal("preview sufficient = true, want false with only 100 available")
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
		nil,
		nil,
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
	price, ok = matchLimitOrder("buy", 19.87, 19.37, 19.8700000001)
	if !ok || price != 19.8700000001 {
		t.Fatalf("buy boundary match = %.10f/%v, want 19.8700000001/true", price, ok)
	}
	_, ok = matchLimitOrder("buy", 19.87, 50, 51.75)
	if ok {
		t.Fatal("buy below ask should not match")
	}
}

func TestMatchMarketOrder(t *testing.T) {
	price, ok := matchMarketOrder("buy", 19.37, 19.87)
	if !ok || price != 19.87 {
		t.Fatalf("buy market = %.2f/%v, want 19.87/true", price, ok)
	}
	price, ok = matchMarketOrder("sell", 19.37, 19.87)
	if !ok || price != 19.37 {
		t.Fatalf("sell market = %.2f/%v, want 19.37/true", price, ok)
	}
	_, ok = matchMarketOrder("buy", 19.37, 0)
	if ok {
		t.Fatal("buy market without ask should not match")
	}
}
