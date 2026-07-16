package orders

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
)

type squareOffPositions struct {
	byMatch map[string][]PositionSnapshot
}

type liveExecutionPositions struct {
	executions *executions.Service
	targets    []PositionSnapshot
}

func (p *liveExecutionPositions) PositionFor(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) (PositionSnapshot, bool) {
	summary := p.executions.PositionSummary(ctx, userID, matchID, marketID, strike)
	return PositionSnapshot{
		UserID: userID, MatchID: matchID, MarketID: marketID, Strike: strike, Lots: summary.NetLots,
		ShortCollateral: round2(summary.OpenShortNotional * (1 + ShortInitialMarginRate)),
	}, true
}

func (p *liveExecutionPositions) ResolveCloseTarget(context.Context, primitive.ObjectID, string) (PositionSnapshot, bool) {
	return PositionSnapshot{}, false
}

func (p *liveExecutionPositions) OpenCloseTargets(context.Context, primitive.ObjectID) ([]PositionSnapshot, error) {
	return nil, nil
}

func (p *liveExecutionPositions) ListOpenByMatch(ctx context.Context, matchID string) ([]PositionSnapshot, error) {
	open := make([]PositionSnapshot, 0, len(p.targets))
	for _, target := range p.targets {
		if matchID != "" && target.MatchID != matchID {
			continue
		}
		summary := p.executions.PositionSummary(ctx, target.UserID, target.MatchID, target.MarketID, target.Strike)
		if summary.NetLots == 0 {
			continue
		}
		target.Lots = summary.NetLots
		target.BuyPrice = summary.AvgBuyPrice
		target.SellPrice = summary.AvgSellPrice
		target.ShortCollateral = round2(summary.OpenShortNotional * (1 + ShortInitialMarginRate))
		open = append(open, target)
	}
	return open, nil
}

type failingProviderSettlementMarkets struct {
	*markets.Service
	err error
}

type failingOrderListRepository struct {
	*MemoryRepository
	err error
}

type failNthFillRepository struct {
	*MemoryRepository
	failAt int
	calls  int
}

func (r *failNthFillRepository) UpdateFill(ctx context.Context, id primitive.ObjectID, update FillUpdate) (*Order, error) {
	r.calls++
	if r.calls == r.failAt {
		return nil, errors.New("injected provider void fill failure")
	}
	return r.MemoryRepository.UpdateFill(ctx, id, update)
}

type failVoidGateMarkets struct {
	*markets.Service
	failOnce bool
}

func (m *failVoidGateMarkets) SetProviderMarketGate(ctx context.Context, matchID string, innings int, lifecycle string, blockers []string, finalScore *int, finalRevision int64) error {
	if lifecycle == markets.MarketLifecycleVoid && m.failOnce {
		m.failOnce = false
		return errors.New("injected provider void gate failure")
	}
	return m.Service.SetProviderMarketGate(ctx, matchID, innings, lifecycle, blockers, finalScore, finalRevision)
}

func (r *failingOrderListRepository) ListWithError(context.Context, OrderFilter) ([]Order, error) {
	return nil, r.err
}

func (s *failingProviderSettlementMarkets) GetProviderSettlementMarket(context.Context, string, int, int64) (*markets.Market, error) {
	return nil, s.err
}

func TestCancelProviderWorkingOrdersPreservesReadFailures(t *testing.T) {
	matchID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()
	match := &matches.Match{ID: matchID, DataSource: matches.DataSourceSportmonks}
	market := markets.Market{ID: marketID, MatchID: matchID.Hex()}
	readErr := errors.New("read failed")

	tests := []struct {
		name       string
		repo       Repository
		marketList *stubMarketSvc
	}{
		{
			name: "market list",
			repo: NewMemoryRepository(),
			marketList: &stubMarketSvc{
				market:  &market,
				listErr: readErr,
			},
		},
		{
			name: "order list",
			repo: &failingOrderListRepository{
				MemoryRepository: NewMemoryRepository(),
				err:              readErr,
			},
			marketList: &stubMarketSvc{market: &market},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(
				tt.repo,
				tt.marketList,
				&stubMatchSvc{match: match},
				wallet.NewService(wallet.NewMemoryRepository()),
				executions.NewService(executions.NewMemoryRepository()),
				nil,
				nil,
			)
			if _, err := svc.CancelProviderWorkingOrders(context.Background(), matchID.Hex()); !errors.Is(err, readErr) {
				t.Fatalf("CancelProviderWorkingOrders() error = %v, want %v", err, readErr)
			}
		})
	}
}

func (p *squareOffPositions) PositionFor(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) (PositionSnapshot, bool) {
	return PositionSnapshot{}, false
}

func (p *squareOffPositions) ResolveCloseTarget(ctx context.Context, userID primitive.ObjectID, positionID string) (PositionSnapshot, bool) {
	return PositionSnapshot{}, false
}

func (p *squareOffPositions) OpenCloseTargets(ctx context.Context, userID primitive.ObjectID) ([]PositionSnapshot, error) {
	return nil, nil
}

func (p *squareOffPositions) ListOpenByMatch(_ context.Context, matchID string) ([]PositionSnapshot, error) {
	if matchID == "" {
		var all []PositionSnapshot
		for _, list := range p.byMatch {
			all = append(all, list...)
		}
		return all, nil
	}
	return p.byMatch[matchID], nil
}

func TestSquareOff_Innings1SettlesFutureMarketsOnly(t *testing.T) {
	userID := primitive.NewObjectID()
	matchHex := "0000000000000000000000aa"
	futureID, _ := primitive.ObjectIDFromHex("0000000000000000000000d2")
	depthID, _ := primitive.ObjectIDFromHex("0000000000000000000000d1")

	futureMarket := markets.Market{ID: futureID, MatchID: "1", Type: "future", Status: markets.MarketStatusActive, LTP: 165}
	depthMarket := markets.Market{ID: depthID, MatchID: "1", Type: "match_depth", Status: markets.MarketStatusActive, LTP: 155}

	marketSvc := &stubMarketSvc{
		markets: []markets.Market{futureMarket, depthMarket},
		market:  &futureMarket,
		bid:     164,
		ask:     166,
		ok:      true,
	}

	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	_, _ = walletSvc.AdminCredit(context.Background(), primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: 10000, Reason: "seed"})

	execSvc := executions.NewService(executions.NewMemoryRepository())
	orderRepo := NewMemoryRepository()
	orderRepo.orders = nil

	posView := &squareOffPositions{
		byMatch: map[string][]PositionSnapshot{
			"1": {
				{UserID: userID, MatchID: "1", MarketID: futureID.Hex(), Strike: 160, Lots: 5, BuyPrice: 150, LTP: 165, Status: "open"},
				{UserID: userID, MatchID: "1", MarketID: depthID.Hex(), Strike: 155, Lots: 3, BuyPrice: 150, LTP: 155, Status: "open"},
			},
			matchHex: {
				{UserID: userID, MatchID: "1", MarketID: futureID.Hex(), Strike: 160, Lots: 5, BuyPrice: 150, LTP: 165, Status: "open"},
				{UserID: userID, MatchID: "1", MarketID: depthID.Hex(), Strike: 155, Lots: 3, BuyPrice: 150, LTP: 155, Status: "open"},
			},
		},
	}

	// Seed a buy execution so OpenLongQty reflects the future position.
	_, _ = execSvc.Create(context.Background(), executions.Execution{
		UserID: userID, MatchID: "1", MarketID: futureID.Hex(), Strike: 160,
		Side: "buy", Price: 150, Quantity: 5,
	})
	_, _ = execSvc.Create(context.Background(), executions.Execution{
		UserID: userID, MatchID: "1", MarketID: depthID.Hex(), Strike: 155,
		Side: "buy", Price: 150, Quantity: 3,
	})

	matchID, _ := primitive.ObjectIDFromHex(matchHex)
	svc := NewService(
		orderRepo,
		marketSvc,
		&stubMatchSvc{match: &matches.Match{ID: matchID, Status: "live", Innings: 1, CurrentScore: 200, WicketsLost: 4, BallsLeft: 0}},
		walletSvc,
		execSvc,
		posView,
		nil,
	)

	result, err := svc.SquareOff(context.Background(), matchHex, SquareOffScopeInnings1)
	if err != nil {
		t.Fatalf("SquareOff: %v", err)
	}
	if result.PositionsSettled != 2 {
		t.Fatalf("PositionsSettled = %d, want 2 (all open positions on match)", result.PositionsSettled)
	}
	if result.MarketsClosed != 1 {
		t.Fatalf("MarketsClosed = %d, want 1 (future market only)", result.MarketsClosed)
	}
	if futureMarket.Status != markets.MarketStatusClosed {
		t.Fatalf("future market status = %q, want closed", futureMarket.Status)
	}
	if depthMarket.Status != markets.MarketStatusActive {
		t.Fatalf("depth market should stay active during innings 1 square-off, got %q", depthMarket.Status)
	}

	depthLots := execSvc.OpenLongQty(context.Background(), userID, "1", depthID.Hex(), 155)
	if depthLots != 0 {
		t.Fatalf("depth open lots = %d, want 0 after innings square-off", depthLots)
	}
	futureLots := execSvc.OpenLongQty(context.Background(), userID, "1", futureID.Hex(), 160)
	if futureLots != 0 {
		t.Fatalf("future open lots = %d, want 0 after square-off", futureLots)
	}

	acct, _ := walletSvc.GetWallet(context.Background(), userID)
	// Sell 5 @ 164 after buy @ 150 → +70 proceeds net of original buy settlement already in wallet from manual exec only
	// Wallet got sell proceeds: 5 * 164 = 820
	if acct.AvailableBalance < 800 {
		t.Fatalf("wallet balance = %.2f, expected sell proceeds credited", acct.AvailableBalance)
	}
}

func TestVoidProviderInningsMarketIsIdempotent(t *testing.T) {
	matchID := primitive.NewObjectID()
	marketRepo := markets.NewMemoryRepository()
	marketSvc := markets.NewService(marketRepo)
	if err := marketSvc.EnsureProviderInningsMarket(context.Background(), markets.ProviderInningsMarketSpec{
		MatchID: matchID.Hex(), Innings: 2, BattingTeamName: "Beta",
		Format: "T20", ScheduledBalls: 120,
	}); err != nil {
		t.Fatal(err)
	}
	if err := marketSvc.SetProviderMarketGate(
		context.Background(), matchID.Hex(), 2, markets.MarketLifecycleSettling,
		[]string{"voiding"}, nil, 0,
	); err != nil {
		t.Fatal(err)
	}
	orderRepo := NewMemoryRepository()
	orderRepo.orders = nil
	svc := NewService(
		orderRepo, marketSvc,
		&stubMatchSvc{match: &matches.Match{
			ID: matchID, DataSource: matches.DataSourceSportmonks, Status: matches.StatusAbandoned,
		}},
		wallet.NewService(wallet.NewMemoryRepository()),
		executions.NewService(executions.NewMemoryRepository()),
		&squareOffPositions{byMatch: map[string][]PositionSnapshot{}}, nil,
	)
	for attempt := 0; attempt < 2; attempt++ {
		if err := svc.VoidProviderInningsMarket(context.Background(), matchID.Hex(), 2); err != nil {
			t.Fatalf("attempt %d: %v", attempt+1, err)
		}
	}
	got := marketSvc.GetMarketsByMatchID(context.Background(), matchID.Hex())
	if len(got) != 1 || got[0].Lifecycle != markets.MarketLifecycleVoid {
		t.Fatalf("market = %+v", got)
	}
}

func TestVoidProviderInningsReversesClosedContractPnL(t *testing.T) {
	ctx := context.Background()
	matchID := primitive.NewObjectID()
	userID := primitive.NewObjectID()
	marketRepo := markets.NewMemoryRepository()
	marketSvc := markets.NewService(marketRepo)
	if err := marketSvc.EnsureProviderInningsMarket(ctx, markets.ProviderInningsMarketSpec{
		MatchID: matchID.Hex(), Innings: 1, BattingTeamName: "Alpha",
		Format: "T20", ScheduledBalls: 120,
	}); err != nil {
		t.Fatal(err)
	}
	market := marketSvc.GetMarketsByMatchID(ctx, matchID.Hex())[0]
	provisional := 150
	if err := marketSvc.SetProviderMarketGate(ctx, matchID.Hex(), 1, markets.MarketLifecycleSettling, []string{"voiding"}, &provisional, 7); err != nil {
		t.Fatal(err)
	}

	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	if _, err := walletSvc.AdminCredit(ctx, primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: 1000}); err != nil {
		t.Fatal(err)
	}
	executionRepo := executions.NewMemoryRepository()
	executionSvc := executions.NewService(executionRepo)
	buyOrderID := primitive.NewObjectID()
	sellOrderID := primitive.NewObjectID()
	if _, err := walletSvc.SettleBuyFill(ctx, userID, 100, 0, buyOrderID.Hex(), "original buy"); err != nil {
		t.Fatal(err)
	}
	if _, err := executionSvc.Create(ctx, executions.Execution{
		ID: primitive.NewObjectID(), UserID: userID, OrderID: buyOrderID,
		MatchID: matchID.Hex(), MarketID: market.ID.Hex(), Strike: 100,
		Side: "buy", Price: 100, Quantity: 1,
		CreatedAt: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := walletSvc.SettleSellFill(ctx, userID, 120, sellOrderID.Hex(), "original sell"); err != nil {
		t.Fatal(err)
	}
	if _, err := executionSvc.Create(ctx, executions.Execution{
		ID: primitive.NewObjectID(), UserID: userID, OrderID: sellOrderID,
		MatchID: matchID.Hex(), MarketID: market.ID.Hex(), Strike: 100,
		Side: "sell", Price: 120, Quantity: 1,
		CreatedAt: time.Date(2026, 7, 16, 12, 1, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	svc := NewService(
		NewMemoryRepository(), marketSvc,
		&stubMatchSvc{match: &matches.Match{ID: matchID, DataSource: matches.DataSourceSportmonks, Status: matches.StatusAbandoned}},
		walletSvc, executionSvc,
		&squareOffPositions{byMatch: map[string][]PositionSnapshot{}}, nil,
	)
	if err := svc.VoidProviderInningsMarket(ctx, matchID.Hex(), 1); err != nil {
		t.Fatal(err)
	}
	account, err := walletSvc.GetWallet(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if account.CashBalance != 1000 || account.ReservedBalance != 0 || account.AvailableBalance != 1000 {
		t.Fatalf("voided wallet = %+v", account)
	}
	voided := marketSvc.GetMarketsByMatchID(ctx, matchID.Hex())[0]
	if voided.Lifecycle != markets.MarketLifecycleVoid || voided.FinalRevision != 0 || voided.FinalScore != 0 {
		t.Fatalf("voided market = %+v", voided)
	}
}

func TestVoidProviderInningsRestoresAllPositionHistoriesExactlyOnce(t *testing.T) {
	tests := []struct {
		name  string
		seed  float64
		fills []executions.Execution
	}{
		{name: "open long", seed: 1000, fills: []executions.Execution{{Side: "buy", Price: 100, Quantity: 2}}},
		{name: "partially closed long", seed: 1000, fills: []executions.Execution{{Side: "buy", Price: 100, Quantity: 2}, {Side: "sell", Price: 120, Quantity: 1}}},
		{name: "closed long", seed: 1000, fills: []executions.Execution{{Side: "buy", Price: 100, Quantity: 1}, {Side: "sell", Price: 120, Quantity: 1}}},
		{name: "open short", seed: 1000, fills: []executions.Execution{{Side: "sell", Price: 100, Quantity: 2}}},
		{name: "partially covered short", seed: 1000, fills: []executions.Execution{{Side: "sell", Price: 100, Quantity: 2}, {Side: "buy", Price: 80, Quantity: 1}}},
		{name: "closed losing short", seed: 100, fills: []executions.Execution{{Side: "sell", Price: 1, Quantity: 1}, {Side: "buy", Price: 100, Quantity: 1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			matchID := primitive.NewObjectID()
			userID := primitive.NewObjectID()
			marketSvc := markets.NewService(markets.NewMemoryRepository())
			if err := marketSvc.EnsureProviderInningsMarket(ctx, markets.ProviderInningsMarketSpec{
				MatchID: matchID.Hex(), Innings: 1, BattingTeamName: "Alpha", Format: "T20", ScheduledBalls: 120,
			}); err != nil {
				t.Fatal(err)
			}
			market := marketSvc.GetMarketsByMatchID(ctx, matchID.Hex())[0]
			provisional := 150
			if err := marketSvc.SetProviderMarketGate(ctx, matchID.Hex(), 1, markets.MarketLifecycleSettling, []string{"voiding"}, &provisional, 7); err != nil {
				t.Fatal(err)
			}

			walletSvc := wallet.NewService(wallet.NewMemoryRepository())
			if _, err := walletSvc.AdminCredit(ctx, primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: tt.seed}); err != nil {
				t.Fatal(err)
			}
			executionSvc := executions.NewService(executions.NewMemoryRepository())
			for i := range tt.fills {
				fill := tt.fills[i]
				fill.ID = primitive.NewObjectID()
				fill.UserID = userID
				fill.OrderID = primitive.NewObjectID()
				fill.MatchID = matchID.Hex()
				fill.MarketID = market.ID.Hex()
				fill.Strike = 100
				fill.CreatedAt = time.Date(2026, 7, 16, 12, i, 0, 0, time.UTC)
				applyOriginalProviderFill(t, ctx, walletSvc, executionSvc, fill)
			}
			positions := &liveExecutionPositions{
				executions: executionSvc,
				targets:    []PositionSnapshot{{UserID: userID, MatchID: matchID.Hex(), MarketID: market.ID.Hex(), Strike: 100}},
			}
			orderRepo := NewMemoryRepository()
			orderRepo.orders = nil
			svc := NewService(
				orderRepo, marketSvc,
				&stubMatchSvc{match: &matches.Match{ID: matchID, DataSource: matches.DataSourceSportmonks, Status: matches.StatusAbandoned}},
				walletSvc, executionSvc, positions, nil,
			)

			for attempt := 0; attempt < 2; attempt++ {
				if err := svc.VoidProviderInningsMarket(ctx, matchID.Hex(), 1); err != nil {
					t.Fatalf("attempt %d: %v", attempt+1, err)
				}
			}
			account, err := walletSvc.GetWallet(ctx, userID)
			if err != nil {
				t.Fatal(err)
			}
			if account.CashBalance != tt.seed || account.ReservedBalance != 0 || account.AvailableBalance != tt.seed {
				t.Fatalf("wallet = %+v, want %.2f/0/%.2f", account, tt.seed, tt.seed)
			}
			if got := executionSvc.NetLots(ctx, userID, matchID.Hex(), market.ID.Hex(), 100); got != 0 {
				t.Fatalf("net lots = %d, want 0", got)
			}
			reversalOrders := orderRepo.GetAll(ctx)
			if len(reversalOrders) != len(tt.fills) {
				t.Fatalf("void reversal orders = %d, want %d", len(reversalOrders), len(tt.fills))
			}
			for _, order := range reversalOrders {
				if order.PositionIntent != positionIntentProviderVoidReverse || !order.ReserveReconciled || order.OutstandingReserve != 0 || order.RemainingReservedAmount() != 0 {
					t.Fatalf("void reversal reserve metadata = %+v", order)
				}
			}
			ledger, err := walletSvc.GetLedger(ctx, userID, 100)
			if err != nil {
				t.Fatal(err)
			}
			reversals := 0
			for _, entry := range ledger {
				if entry.Type == wallet.LedgerProviderVoid {
					reversals++
				}
			}
			if reversals != 1 {
				t.Fatalf("provider void ledger entries = %d, want 1", reversals)
			}
		})
	}
}

func TestProviderVoidCompensationUsesCommittedCollateralNotExecutionTimestamps(t *testing.T) {
	userID := primitive.NewObjectID()
	matchID := primitive.NewObjectID().Hex()
	marketID := primitive.NewObjectID().Hex()
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	// Commit order was BUY, SELL@10 (close), SELL@100 (open short), while the
	// client-side timestamps imply a different and financially incorrect path.
	originals := []executions.Execution{
		{ID: primitive.NewObjectID(), UserID: userID, OrderID: primitive.NewObjectID(), MatchID: matchID, MarketID: marketID, Strike: 100, Side: "buy", Price: 5, Quantity: 1, CreatedAt: base.Add(time.Minute)},
		{ID: primitive.NewObjectID(), UserID: userID, OrderID: primitive.NewObjectID(), MatchID: matchID, MarketID: marketID, Strike: 100, Side: "sell", Price: 10, Quantity: 1, CreatedAt: base.Add(2 * time.Minute)},
		{ID: primitive.NewObjectID(), UserID: userID, OrderID: primitive.NewObjectID(), MatchID: matchID, MarketID: marketID, Strike: 100, Side: "sell", Price: 100, Quantity: 1, CreatedAt: base},
	}
	compensations, err := providerVoidCompensations(originals, matchID, marketID, map[primitive.ObjectID]float64{userID: 200})
	if err != nil {
		t.Fatal(err)
	}
	if len(compensations) != 1 || compensations[0].cashDelta != -105 || compensations[0].reservedDelta != -200 {
		t.Fatalf("compensations = %+v, want cash -105 and reserve -200", compensations)
	}
}

func TestProviderVoidRetryUsesFrozenCompensationAfterPartialOrCompleteUnwind(t *testing.T) {
	for _, failPoint := range []string{"after first reversal", "after all reversals"} {
		t.Run(failPoint, func(t *testing.T) {
			ctx := context.Background()
			matchID := primitive.NewObjectID()
			userID := primitive.NewObjectID()
			baseMarketSvc := markets.NewService(markets.NewMemoryRepository())
			if err := baseMarketSvc.EnsureProviderInningsMarket(ctx, markets.ProviderInningsMarketSpec{
				MatchID: matchID.Hex(), Innings: 1, BattingTeamName: "Alpha", Format: "T20", ScheduledBalls: 120,
			}); err != nil {
				t.Fatal(err)
			}
			market := baseMarketSvc.GetMarketsByMatchID(ctx, matchID.Hex())[0]
			provisional := 150
			if err := baseMarketSvc.SetProviderMarketGate(ctx, matchID.Hex(), 1, markets.MarketLifecycleSettling, []string{"voiding"}, &provisional, 7); err != nil {
				t.Fatal(err)
			}

			walletSvc := wallet.NewService(wallet.NewMemoryRepository())
			if _, err := walletSvc.AdminCredit(ctx, primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: 1000}); err != nil {
				t.Fatal(err)
			}
			executionSvc := executions.NewService(executions.NewMemoryRepository())
			// Leave one short lot and 200 collateral. The first synthetic reversal
			// changes the open projection to two short lots / 360 collateral, so a
			// retry would conflict without the frozen compensation snapshot.
			for i, fill := range []executions.Execution{{Side: "sell", Price: 100, Quantity: 2}, {Side: "buy", Price: 80, Quantity: 1}} {
				fill.ID = primitive.NewObjectID()
				fill.UserID = userID
				fill.OrderID = primitive.NewObjectID()
				fill.MatchID = matchID.Hex()
				fill.MarketID = market.ID.Hex()
				fill.Strike = 100
				fill.CreatedAt = time.Date(2026, 7, 16, 12, i, 0, 0, time.UTC)
				applyOriginalProviderFill(t, ctx, walletSvc, executionSvc, fill)
			}
			positions := &liveExecutionPositions{
				executions: executionSvc,
				targets:    []PositionSnapshot{{UserID: userID, MatchID: matchID.Hex(), MarketID: market.ID.Hex(), Strike: 100}},
			}

			baseOrderRepo := NewMemoryRepository()
			baseOrderRepo.orders = nil
			var orderRepo Repository = baseOrderRepo
			var marketReader MarketReader = baseMarketSvc
			if failPoint == "after first reversal" {
				orderRepo = &failNthFillRepository{MemoryRepository: baseOrderRepo, failAt: 2}
			} else {
				marketReader = &failVoidGateMarkets{Service: baseMarketSvc, failOnce: true}
			}
			svc := NewService(
				orderRepo, marketReader,
				&stubMatchSvc{match: &matches.Match{ID: matchID, DataSource: matches.DataSourceSportmonks, Status: matches.StatusAbandoned}},
				walletSvc, executionSvc, positions, nil,
			)

			if err := svc.VoidProviderInningsMarket(ctx, matchID.Hex(), 1); err == nil {
				t.Fatal("first void unexpectedly succeeded")
			}
			if err := svc.VoidProviderInningsMarket(ctx, matchID.Hex(), 1); err != nil {
				t.Fatalf("retry void: %v", err)
			}
			account, err := walletSvc.GetWallet(ctx, userID)
			if err != nil {
				t.Fatal(err)
			}
			if account.CashBalance != 1000 || account.ReservedBalance != 0 || account.AvailableBalance != 1000 {
				t.Fatalf("wallet = %+v, want exactly restored once", account)
			}
			ledger, err := walletSvc.GetLedger(ctx, userID, 100)
			if err != nil {
				t.Fatal(err)
			}
			reversals := 0
			for _, entry := range ledger {
				if entry.Type == wallet.LedgerProviderVoid {
					reversals++
				}
			}
			if reversals != 1 {
				t.Fatalf("void ledger entries = %d, want 1", reversals)
			}
			if got := executionSvc.NetLots(ctx, userID, matchID.Hex(), market.ID.Hex(), 100); got != 0 {
				t.Fatalf("net lots = %d, want 0", got)
			}
		})
	}
}

func applyOriginalProviderFill(t *testing.T, ctx context.Context, walletSvc *wallet.Service, executionSvc *executions.Service, fill executions.Execution) {
	t.Helper()
	before := executionSvc.PositionSummary(ctx, fill.UserID, fill.MatchID, fill.MarketID, fill.Strike)
	plan, err := buildPositionPlan(fill.Side, fill.Quantity, PositionEffectAuto, before.NetLots, fill.Strike, fill.Price)
	if err != nil {
		t.Fatal(err)
	}
	switch fill.Side {
	case "buy":
		if plan.CoverShortQty > 0 {
			collateral := round2(before.OpenShortNotional * (1 + ShortInitialMarginRate))
			release := proRataShortCollateralRelease(collateral, before.NetLots, plan.CoverShortQty)
			if release > 0 {
				if _, err := walletSvc.ReleaseOrderMargin(ctx, fill.UserID, release, fill.OrderID.Hex(), "original cover release"); err != nil {
					t.Fatal(err)
				}
			}
		}
		cost := round2(fill.Price * float64(fill.Quantity))
		if cost > 0 {
			if _, err := walletSvc.SettleBuyFill(ctx, fill.UserID, cost, 0, fill.OrderID.Hex(), "original buy"); err != nil {
				t.Fatal(err)
			}
		}
	case "sell":
		if plan.OpenShortQty > 0 {
			margin := round2(fill.Price * float64(plan.OpenShortQty) * ShortInitialMarginRate)
			if margin > 0 {
				if _, err := walletSvc.ReserveOrderMargin(ctx, fill.UserID, margin, fill.OrderID.Hex(), "original short margin"); err != nil {
					t.Fatal(err)
				}
			}
		}
		if plan.CloseLongQty > 0 {
			proceeds := round2(fill.Price * float64(plan.CloseLongQty))
			if proceeds > 0 {
				if _, err := walletSvc.SettleSellFill(ctx, fill.UserID, proceeds, fill.OrderID.Hex(), "original long close"); err != nil {
					t.Fatal(err)
				}
			}
		}
		if plan.OpenShortQty > 0 {
			proceeds := round2(fill.Price * float64(plan.OpenShortQty))
			if proceeds > 0 {
				if _, err := walletSvc.SettleShortOpenFill(ctx, fill.UserID, proceeds, fill.OrderID.Hex(), "original short proceeds"); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if _, err := executionSvc.Create(ctx, fill); err != nil {
		t.Fatal(err)
	}
}

func TestSettleProviderInningsRequiresExactContractRevision(t *testing.T) {
	matchID := primitive.NewObjectID()
	marketRepo := markets.NewMemoryRepository()
	marketSvc := markets.NewService(marketRepo)
	if err := marketSvc.EnsureProviderInningsMarket(context.Background(), markets.ProviderInningsMarketSpec{
		MatchID: matchID.Hex(), Innings: 1, BattingTeamName: "Alpha",
		Format: "T20", ScheduledBalls: 120,
	}); err != nil {
		t.Fatal(err)
	}
	finalScore := 176
	if err := marketSvc.SetProviderMarketGate(
		context.Background(), matchID.Hex(), 1, markets.MarketLifecycleSettling,
		[]string{"finalizing"}, &finalScore, 9,
	); err != nil {
		t.Fatal(err)
	}
	svc := NewService(
		NewMemoryRepository(), marketSvc,
		&stubMatchSvc{match: &matches.Match{ID: matchID, DataSource: matches.DataSourceSportmonks}},
		wallet.NewService(wallet.NewMemoryRepository()),
		executions.NewService(executions.NewMemoryRepository()),
		&squareOffPositions{byMatch: map[string][]PositionSnapshot{}}, nil,
	)

	if err := svc.SettleProviderInnings(context.Background(), matchID.Hex(), 1, 10); err == nil {
		t.Fatal("settlement accepted a missing final revision contract")
	}
	if err := svc.SettleProviderInnings(context.Background(), matchID.Hex(), 1, 9); err != nil {
		t.Fatalf("settle exact contract: %v", err)
	}
	market, err := marketSvc.GetProviderSettlementMarket(context.Background(), matchID.Hex(), 1, 9)
	if err != nil || market == nil || market.Lifecycle != markets.MarketLifecycleSettled || market.SettlementRevision != 9 {
		t.Fatalf("settled market = %+v, err = %v", market, err)
	}
}

func TestSettleProviderInningsHandlesInsolvencyAndCurrentShortCollateral(t *testing.T) {
	tests := []struct {
		name         string
		seed         float64
		debit        float64
		fills        []executions.Execution
		expectedCash float64
	}{
		{
			name: "losing short can settle below zero", seed: 100, debit: 99,
			fills: []executions.Execution{{Side: "sell", Price: 1, Quantity: 1}}, expectedCash: -98,
		},
		{
			name: "long sales are excluded from short collateral", seed: 10000,
			fills: []executions.Execution{
				{Side: "buy", Price: 30, Quantity: 10},
				{Side: "sell", Price: 100, Quantity: 5},
				{Side: "sell", Price: 50, Quantity: 10},
			},
			expectedCash: 10200,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			matchID := primitive.NewObjectID()
			userID := primitive.NewObjectID()
			marketSvc := markets.NewService(markets.NewMemoryRepository())
			if err := marketSvc.EnsureProviderInningsMarket(ctx, markets.ProviderInningsMarketSpec{
				MatchID: matchID.Hex(), Innings: 1, BattingTeamName: "Alpha", Format: "T20", ScheduledBalls: 120,
			}); err != nil {
				t.Fatal(err)
			}
			market := marketSvc.GetMarketsByMatchID(ctx, matchID.Hex())[0]
			finalScore := 200
			if err := marketSvc.SetProviderMarketGate(ctx, matchID.Hex(), 1, markets.MarketLifecycleSettling, []string{"finalizing"}, &finalScore, 9); err != nil {
				t.Fatal(err)
			}

			walletSvc := wallet.NewService(wallet.NewMemoryRepository())
			if _, err := walletSvc.AdminCredit(ctx, primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: tt.seed}); err != nil {
				t.Fatal(err)
			}
			executionSvc := executions.NewService(executions.NewMemoryRepository())
			for i := range tt.fills {
				fill := tt.fills[i]
				fill.ID = primitive.NewObjectID()
				fill.UserID = userID
				fill.OrderID = primitive.NewObjectID()
				fill.MatchID = matchID.Hex()
				fill.MarketID = market.ID.Hex()
				fill.Strike = 100
				fill.CreatedAt = time.Date(2026, 7, 16, 12, i, 0, 0, time.UTC)
				applyOriginalProviderFill(t, ctx, walletSvc, executionSvc, fill)
			}
			if tt.debit > 0 {
				if _, err := walletSvc.AdminDebit(ctx, primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: tt.debit}); err != nil {
					t.Fatal(err)
				}
			}
			positions := &liveExecutionPositions{
				executions: executionSvc,
				targets:    []PositionSnapshot{{UserID: userID, MatchID: matchID.Hex(), MarketID: market.ID.Hex(), Strike: 100}},
			}
			svc := NewService(
				NewMemoryRepository(), marketSvc,
				&stubMatchSvc{match: &matches.Match{ID: matchID, DataSource: matches.DataSourceSportmonks, Status: matches.StatusCompleted}},
				walletSvc, executionSvc, positions, nil,
			)

			for attempt := 0; attempt < 2; attempt++ {
				if err := svc.SettleProviderInnings(ctx, matchID.Hex(), 1, 9); err != nil {
					t.Fatalf("attempt %d: %v", attempt+1, err)
				}
			}
			account, err := walletSvc.GetWallet(ctx, userID)
			if err != nil {
				t.Fatal(err)
			}
			if account.CashBalance != tt.expectedCash || account.ReservedBalance != 0 || account.AvailableBalance != tt.expectedCash {
				t.Fatalf("wallet = %+v, want %.2f/0/%.2f", account, tt.expectedCash, tt.expectedCash)
			}
			if got := executionSvc.NetLots(ctx, userID, matchID.Hex(), market.ID.Hex(), 100); got != 0 {
				t.Fatalf("net lots = %d, want 0", got)
			}
			settled, err := marketSvc.GetProviderSettlementMarket(ctx, matchID.Hex(), 1, 9)
			if err != nil || settled == nil || settled.Lifecycle != markets.MarketLifecycleSettled {
				t.Fatalf("settled market = %+v, err=%v", settled, err)
			}
		})
	}
}

func TestProviderSettlementAggregatesMoreThanFiveHundredExecutions(t *testing.T) {
	ctx := context.Background()
	matchID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()
	userID := primitive.NewObjectID()
	executionSvc := executions.NewService(executions.NewMemoryRepository())
	for i := 0; i < 600; i++ {
		if _, err := executionSvc.Create(ctx, executions.Execution{
			ID: primitive.NewObjectID(), UserID: userID, OrderID: primitive.NewObjectID(),
			MatchID: matchID.Hex(), MarketID: marketID.Hex(), Strike: 100,
			Side: "buy", Price: 1, Quantity: 1,
			CreatedAt: time.Date(2026, 7, 16, 12, 0, 0, i, time.UTC),
		}); err != nil {
			t.Fatal(err)
		}
	}
	market := &markets.Market{
		ID: marketID, MatchID: matchID.Hex(), Kind: markets.MarketKindInningsScore,
		Innings: 1, FormulaVersion: markets.FormulaVersionInningsScoreV1,
		FinalScore: 200, FinalRevision: 9,
	}
	match := &matches.Match{ID: matchID, DataSource: matches.DataSourceSportmonks, Status: matches.StatusCompleted}
	svc := NewService(
		NewMemoryRepository(), &stubMarketSvc{market: market}, &stubMatchSvc{match: match},
		wallet.NewService(wallet.NewMemoryRepository()), executionSvc, &squareOffPositions{}, nil,
	)

	if _, err := svc.forceClosePosition(ctx, userID, PositionSnapshot{
		UserID: userID, MatchID: matchID.Hex(), MarketID: marketID.Hex(), Strike: 100,
		Lots: 600, BuyPrice: 1,
	}, match, market); err != nil {
		t.Fatalf("force close: %v", err)
	}
	summary, err := executionSvc.PositionSummaryWithError(ctx, userID, matchID.Hex(), marketID.Hex(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if summary.NetLots != 0 {
		t.Fatalf("net lots = %d, want 0", summary.NetLots)
	}
}

func TestSettleProviderInningsPropagatesMarketLookupFailure(t *testing.T) {
	matchID := primitive.NewObjectID()
	lookupErr := errors.New("market read failed")
	marketSvc := &failingProviderSettlementMarkets{
		Service: markets.NewService(markets.NewMemoryRepository()),
		err:     lookupErr,
	}
	svc := NewService(
		NewMemoryRepository(), marketSvc,
		&stubMatchSvc{match: &matches.Match{ID: matchID, DataSource: matches.DataSourceSportmonks}},
		wallet.NewService(wallet.NewMemoryRepository()),
		executions.NewService(executions.NewMemoryRepository()),
		&squareOffPositions{byMatch: map[string][]PositionSnapshot{}}, nil,
	)

	err := svc.SettleProviderInnings(context.Background(), matchID.Hex(), 1, 4)
	if !errors.Is(err, lookupErr) {
		t.Fatalf("settlement error = %v, want %v", err, lookupErr)
	}
}

func TestProviderMatchIDKeysExcludeLegacySuffix(t *testing.T) {
	match := &matches.Match{ID: primitive.NewObjectID(), DataSource: matches.DataSourceSportmonks}
	keys := matchIDKeys(match)
	if len(keys) != 1 || keys[0] != match.ID.Hex() {
		t.Fatalf("provider match keys = %v, want only %q", keys, match.ID.Hex())
	}
}

func TestSquareOff_MatchSettlesAllMarkets(t *testing.T) {
	userID := primitive.NewObjectID()
	matchHex := "0000000000000000000000aa"
	futureID, _ := primitive.ObjectIDFromHex("0000000000000000000000d2")
	depthID, _ := primitive.ObjectIDFromHex("0000000000000000000000d1")

	futureMarket := markets.Market{ID: futureID, MatchID: "1", Type: "future", Status: markets.MarketStatusActive, LTP: 165}
	depthMarket := markets.Market{ID: depthID, MatchID: "1", Type: "match_depth", Status: markets.MarketStatusActive, LTP: 155}

	marketSvc := &stubMarketSvc{
		markets: []markets.Market{futureMarket, depthMarket},
		market:  &depthMarket,
		bid:     154,
		ask:     156,
		ok:      true,
	}

	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	execSvc := executions.NewService(executions.NewMemoryRepository())
	orderRepo := NewMemoryRepository()

	posView := &squareOffPositions{
		byMatch: map[string][]PositionSnapshot{
			"1": {
				{UserID: userID, MatchID: "1", MarketID: futureID.Hex(), Strike: 160, Lots: 2, BuyPrice: 150, Status: "open"},
				{UserID: userID, MatchID: "1", MarketID: depthID.Hex(), Strike: 155, Lots: 4, BuyPrice: 150, Status: "open"},
			},
		},
	}

	_, _ = execSvc.Create(context.Background(), executions.Execution{
		UserID: userID, MatchID: "1", MarketID: futureID.Hex(), Strike: 160, Side: "buy", Price: 150, Quantity: 2,
	})
	_, _ = execSvc.Create(context.Background(), executions.Execution{
		UserID: userID, MatchID: "1", MarketID: depthID.Hex(), Strike: 155, Side: "buy", Price: 150, Quantity: 4,
	})

	matchID, _ := primitive.ObjectIDFromHex(matchHex)
	svc := NewService(orderRepo, marketSvc, &stubMatchSvc{match: &matches.Match{ID: matchID, Status: "live", Innings: 2}}, walletSvc, execSvc, posView, nil)

	result, err := svc.SquareOff(context.Background(), matchHex, SquareOffScopeMatch)
	if err != nil {
		t.Fatalf("SquareOff: %v", err)
	}
	if result.PositionsSettled != 2 {
		t.Fatalf("PositionsSettled = %d, want 2", result.PositionsSettled)
	}
	if result.MarketsClosed != 2 {
		t.Fatalf("MarketsClosed = %d, want 2", result.MarketsClosed)
	}
}

func TestSquareOff_MatchSettlesShortAtAsk(t *testing.T) {
	userID := primitive.NewObjectID()
	matchHex := "0000000000000000000000aa"
	marketID, _ := primitive.ObjectIDFromHex("0000000000000000000000d3")

	market := markets.Market{ID: marketID, MatchID: "1", Type: "future", Status: markets.MarketStatusActive, LTP: 45}
	marketSvc := &stubMarketSvc{
		markets: []markets.Market{market},
		market:  &market,
		bid:     44,
		ask:     45,
		ok:      true,
	}

	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	_, _ = walletSvc.AdminCredit(context.Background(), primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: 10000, Reason: "seed"})
	_, _ = walletSvc.ReserveOrderMargin(context.Background(), userID, 500, "short-open", "short initial margin")
	_, _ = walletSvc.SettleShortOpenFill(context.Background(), userID, 500, "short-open", "short sale proceeds")

	execSvc := executions.NewService(executions.NewMemoryRepository())
	_, _ = execSvc.Create(context.Background(), executions.Execution{
		UserID: userID, MatchID: "1", MarketID: marketID.Hex(), Strike: 160, Side: "sell", Price: 50, Quantity: 10,
	})

	orderRepo := NewMemoryRepository()
	orderRepo.orders = nil
	posView := &squareOffPositions{
		byMatch: map[string][]PositionSnapshot{
			"1": {
				{UserID: userID, MatchID: "1", MarketID: marketID.Hex(), Strike: 160, Lots: -10, SellPrice: 50, LTP: 45, Status: "open"},
			},
		},
	}

	matchID, _ := primitive.ObjectIDFromHex(matchHex)
	svc := NewService(orderRepo, marketSvc, &stubMatchSvc{match: &matches.Match{ID: matchID, Status: "live", Innings: 2}}, walletSvc, execSvc, posView, nil)

	result, err := svc.SquareOff(context.Background(), matchHex, SquareOffScopeMatch)
	if err != nil {
		t.Fatalf("SquareOff: %v", err)
	}
	if result.PositionsSettled != 1 || result.TotalRealizedPnL != 50 {
		t.Fatalf("settled/pnl = %d/%.2f, want 1/50", result.PositionsSettled, result.TotalRealizedPnL)
	}
	if got := execSvc.NetLots(context.Background(), userID, "1", marketID.Hex(), 160); got != 0 {
		t.Fatalf("net lots = %d, want 0", got)
	}
	orders := orderRepo.GetAll(context.Background())
	if len(orders) != 1 || orders[0].Side != "buy" || orders[0].AverageFillPrice != 45 {
		t.Fatalf("settlement order = %+v, want one BUY at ask 45", orders)
	}
	acct, _ := walletSvc.GetWallet(context.Background(), userID)
	if acct.CashBalance != 10050 || acct.ReservedBalance != 0 || acct.AvailableBalance != 10050 {
		t.Fatalf("wallet = cash %.2f reserved %.2f available %.2f, want 10050/0/10050", acct.CashBalance, acct.ReservedBalance, acct.AvailableBalance)
	}
}

func TestReopenMatchMarkets(t *testing.T) {
	marketID, _ := primitive.ObjectIDFromHex("0000000000000000000000d2")
	m := markets.Market{ID: marketID, MatchID: "1", Type: "future", Status: markets.MarketStatusClosed}
	marketSvc := &stubMarketSvc{markets: []markets.Market{m}, market: &m}

	svc := NewService(NewMemoryRepository(), marketSvc, &stubMatchSvc{}, wallet.NewService(wallet.NewMemoryRepository()), executions.NewService(executions.NewMemoryRepository()), &squareOffPositions{}, nil)

	if err := svc.ReopenMatchMarkets(context.Background(), "0000000000000000000000aa"); err != nil {
		t.Fatalf("ReopenMatchMarkets: %v", err)
	}
	if m.Status != markets.MarketStatusActive {
		t.Fatalf("status = %q, want active", m.Status)
	}
}
