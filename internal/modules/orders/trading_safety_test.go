package orders

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
)

type providerMatchStub struct {
	match       *matches.Match
	gateValid   bool
	gateTouches int
	failAtTouch int
}

type providerMarketStub struct {
	*stubMarketSvc
	gateValid   bool
	gateTouches int
	failAtTouch int
}

func (s *providerMarketStub) VerifyProviderMarketGate(_ context.Context, _ string, stateVersion, tradingVersion int64) (*markets.Market, bool, error) {
	s.gateTouches++
	valid := s.gateValid && (s.failAtTouch == 0 || s.gateTouches < s.failAtTouch)
	if valid {
		s.market.MatchStateVersion = stateVersion
		s.market.TradingVersion = tradingVersion
	}
	return s.market, valid, nil
}

func (s *providerMatchStub) GetMatchByID(context.Context, string) (*matches.Match, error) {
	return s.match, nil
}

func (s *providerMatchStub) VerifyTradingGate(_ context.Context, _ string, stateVersion, tradingVersion int64) (*matches.Match, bool, error) {
	s.gateTouches++
	valid := s.gateValid && (s.failAtTouch == 0 || s.gateTouches < s.failAtTouch)
	_ = stateVersion
	_ = tradingVersion
	return s.match, valid, nil
}

func newProviderTradingService(t *testing.T) (*Service, *providerMarketStub, *providerMatchStub, primitive.ObjectID) {
	t.Helper()
	userID := primitive.NewObjectID()
	matchID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()
	walletSvc := wallet.NewService(wallet.NewMemoryRepository())
	_, _ = walletSvc.AdminCredit(context.Background(), primitive.NewObjectID(), userID, wallet.FundingRequest{Amount: 100000})
	marketSvc := &providerMarketStub{gateValid: true, stubMarketSvc: &stubMarketSvc{
		market: &markets.Market{
			ID: marketID, MatchID: matchID.Hex(), Status: markets.MarketStatusActive,
			Kind: markets.MarketKindInningsScore, FormulaVersion: markets.FormulaVersionInningsScoreV1,
			Innings: 1, Lifecycle: markets.MarketLifecycleOpen, MatchStateVersion: 12, TradingVersion: 5,
		},
		bid: 50, ask: 51, ok: true,
	}}
	matchSvc := &providerMatchStub{gateValid: true, match: &matches.Match{
		ID: matchID, DataSource: matches.DataSourceSportmonks, Status: matches.StatusLive,
		Innings: 1, CurrentScore: 80, WicketsLost: 2, BallsLeft: 50,
		FeedState: matches.FeedStateHealthy, TradingState: markets.MarketLifecycleOpen,
		StateVersion: 12, TradingVersion: 5,
	}}
	repo := NewMemoryRepository()
	repo.orders = nil
	return NewService(repo, marketSvc, matchSvc, walletSvc, executions.NewService(executions.NewMemoryRepository()), nil, nil), marketSvc, matchSvc, userID
}

func TestProviderPreviewUsesAuthoritativeMatchState(t *testing.T) {
	svc, marketSvc, matchSvc, userID := newProviderTradingService(t)
	malicious := markets.PriceCalculationInput{Innings: 2, CurrentScore: 199, TargetScore: 200, BallsBowled: 119}
	preview, err := svc.PreviewOrder(context.Background(), userID, CreateOrderRequest{
		MatchID: matchSvc.match.ID.Hex(), MarketID: marketSvc.market.ID.Hex(), Strike: 130,
		Side: "buy", Type: OrderTypeMarket, Quantity: 1, PricingSnapshot: &malicious,
	})
	if err != nil {
		t.Fatalf("PreviewOrder: %v", err)
	}
	if marketSvc.lastInput.Innings != 1 || marketSvc.lastInput.CurrentScore != 80 {
		t.Fatalf("pricing used client state: %+v", marketSvc.lastInput)
	}
	if preview.MatchStateVersion != 12 || preview.TradingVersion != 5 {
		t.Fatalf("preview versions = %d/%d", preview.MatchStateVersion, preview.TradingVersion)
	}
	remaining := time.Until(preview.ExpiresAt)
	if remaining <= 0 || remaining > 5*time.Second {
		t.Fatalf("preview expiry remaining = %s", remaining)
	}
}

func TestProviderOrderRequiresCurrentVersionsAndTouchesGateForCreateAndFill(t *testing.T) {
	svc, marketSvc, matchSvc, userID := newProviderTradingService(t)
	req := CreateOrderRequest{
		MatchID: matchSvc.match.ID.Hex(), MarketID: marketSvc.market.ID.Hex(), Strike: 130,
		Side: "buy", Type: OrderTypeMarket, Quantity: 1,
		ExpectedMatchStateVersion: 12, ExpectedTradingVersion: 5, QuoteExpiresAt: time.Now().UTC().Add(5 * time.Second),
	}
	order, err := svc.CreateOrder(context.Background(), userID, req)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.MatchStateVersion != 12 || order.TradingVersion != 5 {
		t.Fatalf("stored versions = %d/%d", order.MatchStateVersion, order.TradingVersion)
	}
	if matchSvc.gateTouches != 2 {
		t.Fatalf("gate touches = %d, want 2 (reserve and fill)", matchSvc.gateTouches)
	}
	if marketSvc.gateTouches != 2 {
		t.Fatalf("market gate touches = %d, want 2 (reserve and fill)", marketSvc.gateTouches)
	}

	req.ClientOrderID = "stale-version"
	req.ExpectedTradingVersion = 4
	// Stale client fence is rebound to the live gate while trading stays open.
	order, err = svc.CreateOrder(context.Background(), userID, req)
	if err != nil {
		t.Fatalf("stale fence buy should refresh: %v", err)
	}
	if order.TradingVersion != matchSvc.match.TradingVersion {
		t.Fatalf("stored trading version = %d, want live %d", order.TradingVersion, matchSvc.match.TradingVersion)
	}
}

func TestProviderCloseRefreshesStaleFenceAndFillSurvivesScoreTick(t *testing.T) {
	svc, marketSvc, matchSvc, userID := newProviderTradingService(t)

	// Sportmonks tick bumped versions; client still carries the old fence.
	matchSvc.match.StateVersion = 13
	marketSvc.market.MatchStateVersion = 13
	req := CreateOrderRequest{
		MatchID: matchSvc.match.ID.Hex(), MarketID: marketSvc.market.ID.Hex(), Strike: 130,
		Side: "buy", Type: OrderTypeMarket, Quantity: 1, PositionEffect: PositionEffectAuto,
		ExpectedMatchStateVersion: 12, ExpectedTradingVersion: 5, QuoteExpiresAt: time.Now().UTC().Add(5 * time.Second),
	}
	order, err := svc.CreateOrder(context.Background(), userID, req)
	if err != nil {
		t.Fatalf("stale-fence buy after score tick: %v", err)
	}
	if order.MatchStateVersion != 13 {
		t.Fatalf("order stateVersion = %d, want live 13", order.MatchStateVersion)
	}

	// Fill path: order frozen at v12, live match at v13 — must use live gate and succeed.
	resting := Order{
		ID: primitive.NewObjectID(), UserID: userID,
		MatchID: matchSvc.match.ID.Hex(), MarketID: marketSvc.market.ID.Hex(),
		Strike: 131, Side: "sell", Type: OrderTypeMarket, Status: StatusOpen,
		Quantity: 1, RemainingQuantity: 1, Price: 50,
		MatchStateVersion: 12, TradingVersion: 5,
		PositionEffect: PositionEffectAuto,
	}
	created, err := svc.repo.Create(context.Background(), resting)
	if err != nil {
		t.Fatalf("seed resting order: %v", err)
	}
	if _, err := svc.applyFill(context.Background(), userID, created, 50, 1); err != nil {
		t.Fatalf("fill after score tick: %v", err)
	}
}

func TestProviderOrderRejectsMarketFromAnotherMatch(t *testing.T) {
	svc, marketSvc, matchSvc, userID := newProviderTradingService(t)
	marketSvc.market.MatchID = primitive.NewObjectID().Hex()
	_, err := svc.PreviewOrder(context.Background(), userID, CreateOrderRequest{
		MatchID: matchSvc.match.ID.Hex(), MarketID: marketSvc.market.ID.Hex(), Strike: 130,
		Side: "buy", Type: OrderTypeMarket, Quantity: 1,
	})
	if !errors.Is(err, ErrMarketContractMismatch) {
		t.Fatalf("error = %v, want market contract mismatch", err)
	}
}

func TestProviderOrderRejectsLegacyMatchIDSuffix(t *testing.T) {
	svc, marketSvc, matchSvc, userID := newProviderTradingService(t)
	matchID := matchSvc.match.ID.Hex()
	marketSvc.market.MatchID = matchID[len(matchID)-2:]
	_, err := svc.PreviewOrder(context.Background(), userID, CreateOrderRequest{
		MatchID: matchID, MarketID: marketSvc.market.ID.Hex(), Strike: 130,
		Side: "buy", Type: OrderTypeMarket, Quantity: 1,
	})
	if !errors.Is(err, ErrMarketContractMismatch) {
		t.Fatalf("error = %v, want exact provider match ownership", err)
	}
}

func TestProviderInternalCloseAttachesCurrentFence(t *testing.T) {
	svc, _, matchSvc, _ := newProviderTradingService(t)
	request := CreateOrderRequest{MatchID: matchSvc.match.ID.Hex()}
	before := time.Now().UTC()
	if err := svc.attachProviderFence(context.Background(), &request); err != nil {
		t.Fatalf("attachProviderFence: %v", err)
	}
	if request.ExpectedMatchStateVersion != matchSvc.match.StateVersion ||
		request.ExpectedTradingVersion != matchSvc.match.TradingVersion {
		t.Fatalf("fence = %d/%d", request.ExpectedMatchStateVersion, request.ExpectedTradingVersion)
	}
	if request.QuoteExpiresAt.Before(before) || request.QuoteExpiresAt.After(before.Add(6*time.Second)) {
		t.Fatalf("quote expiry = %s", request.QuoteExpiresAt)
	}
}

func TestProviderGateChangeBeforeFillCancelsOrderAndReleasesMargin(t *testing.T) {
	svc, marketSvc, matchSvc, userID := newProviderTradingService(t)
	marketSvc.failAtTouch = 2
	_, err := svc.CreateOrder(context.Background(), userID, CreateOrderRequest{
		MatchID: matchSvc.match.ID.Hex(), MarketID: marketSvc.market.ID.Hex(), Strike: 130,
		Side: "buy", Type: OrderTypeMarket, Quantity: 2,
		ExpectedMatchStateVersion: 12, ExpectedTradingVersion: 5, QuoteExpiresAt: time.Now().UTC().Add(5 * time.Second),
	})
	if !errors.Is(err, ErrTradingStateChanged) {
		t.Fatalf("error = %v, want trading state changed", err)
	}
	orders := svc.repo.GetAll(context.Background())
	if len(orders) != 1 || orders[0].Status != StatusCancelled {
		t.Fatalf("orders = %+v, want one cancelled order", orders)
	}
	account, walletErr := svc.wallets.GetWallet(context.Background(), userID)
	if walletErr != nil {
		t.Fatal(walletErr)
	}
	if account.ReservedBalance != 0 {
		t.Fatalf("reserved balance = %.2f, want 0", account.ReservedBalance)
	}
}

func TestTradingStateChangedHasStableHTTPCode(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeOrderError(recorder, ErrTradingStateChanged)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", recorder.Code)
	}
	if body := recorder.Body.String(); body == "" || !containsAll(body, "TRADING_STATE_CHANGED", "Trading state changed") {
		t.Fatalf("body = %s", body)
	}
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}
