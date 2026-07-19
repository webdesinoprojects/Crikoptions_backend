package orders

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/realtime"
)

var (
	ErrMarketNotFound         = errors.New("market not found")
	ErrMatchNotFound          = errors.New("match not found")
	ErrInvalidSide            = errors.New("invalid side, must be 'buy' or 'sell'")
	ErrInvalidQuantity        = errors.New("quantity must be positive")
	ErrInvalidPrice           = errors.New("price must be positive")
	ErrInvalidStrike          = errors.New("strike must be positive")
	ErrMarketNotTradable      = errors.New("market is not open for trading")
	ErrMatchNotTradable       = errors.New("match is not live for trading")
	ErrInsufficientBalance    = errors.New("insufficient available wallet balance")
	ErrInsufficientPosition   = errors.New("insufficient position to sell")
	ErrStrikeNotFound         = errors.New("strike not found in option chain")
	ErrNoLiquidity            = errors.New("no executable market quote available")
	ErrTradingStateChanged    = newCodedAPIError(http.StatusConflict, "TRADING_STATE_CHANGED", "Trading state changed; refresh the quote and try again")
	ErrMarketClosed           = newCodedAPIError(http.StatusBadRequest, "MARKET_CLOSED", "This market has settled and is no longer tradable")
	ErrMarketContractMismatch = newCodedAPIError(http.StatusConflict, "MARKET_CONTRACT_MISMATCH", "Market does not belong to the current match innings")
	ErrReservedClientOrderID  = newCodedAPIError(http.StatusBadRequest, "RESERVED_CLIENT_ORDER_ID", "clientOrderId uses a reserved system prefix")
	ErrInternalOrderCancel    = newCodedAPIError(http.StatusConflict, "ORDER_NOT_CANCELLABLE", "System settlement orders cannot be cancelled")
)

const ShortInitialMarginRate = 1.0

const (
	settlementClientOrderPrefix       = "settlement:"
	voidClientOrderPrefix             = "void:"
	positionIntentProviderVoidReverse = "PROVIDER_VOID_REVERSAL"
)

type MatchReader interface {
	GetMatchByID(ctx context.Context, id string) (*matches.Match, error)
}

// TradingGateVerifier conditionally touches the provider match gate inside the
// caller's transaction. The write makes a concurrent suspension/version bump
// conflict with order reservation or execution instead of allowing a stale
// snapshot-isolation read to commit.
type TradingGateVerifier interface {
	VerifyTradingGate(ctx context.Context, id string, stateVersion, tradingVersion int64) (*matches.Match, bool, error)
}

type ProviderMarketGateVerifier interface {
	VerifyProviderMarketGate(ctx context.Context, id string, stateVersion, tradingVersion int64) (*markets.Market, bool, error)
}

type MarketReader interface {
	GetMarketByID(ctx context.Context, id string) (*markets.Market, error)
	GetMarketsByMatchID(ctx context.Context, matchID string) []markets.Market
	ListMarketsByMatchID(ctx context.Context, matchID string) ([]markets.Market, error)
	SetMarketStatus(ctx context.Context, id, status string) (*markets.Market, error)
	StrikeQuote(input markets.PriceCalculationInput, strike float64) (bid, ask float64, ok bool)
	IsTradable(market *markets.Market) bool
}

type WalletPort interface {
	GetWallet(ctx context.Context, userID primitive.ObjectID) (*wallet.Account, error)
	ReserveOrderMargin(ctx context.Context, userID primitive.ObjectID, amount float64, orderID, description string) (*wallet.AdjustmentResult, error)
	ReleaseOrderMargin(ctx context.Context, userID primitive.ObjectID, amount float64, orderID, description string) (*wallet.AdjustmentResult, error)
	SettleBuyFill(ctx context.Context, userID primitive.ObjectID, fillCost, reserveRelease float64, orderID, description string) (*wallet.AdjustmentResult, error)
	SettleSellFill(ctx context.Context, userID primitive.ObjectID, proceeds float64, orderID, description string) (*wallet.AdjustmentResult, error)
	SettleShortOpenFill(ctx context.Context, userID primitive.ObjectID, proceeds float64, orderID, description string) (*wallet.AdjustmentResult, error)
	SettleForcedBuyFill(ctx context.Context, userID primitive.ObjectID, fillCost, reserveRelease float64, orderID, description string) (*wallet.AdjustmentResult, error)
	ReverseProviderContract(ctx context.Context, userID primitive.ObjectID, cashDelta, reservedDelta float64, marketID, description string) (*wallet.AdjustmentResult, error)
}

type ExecutionWriter interface {
	Create(ctx context.Context, exec executions.Execution) (*executions.Execution, error)
	ListWithError(ctx context.Context, filter executions.Filter) ([]executions.Execution, error)
	OpenLongQty(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) int
	PositionSummary(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) executions.PositionSummary
	PositionSummaryWithError(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) (executions.PositionSummary, error)
}

// EventPublisher streams realtime updates (implemented by realtime.Hub). It is
// optional; a nil publisher disables broadcasts.
type EventPublisher interface {
	Publish(topic string, data any)
}

// PositionSnapshot is the post-fill view of a user's position for a strike,
// used both for WebSocket broadcasts and the close endpoint.
type PositionSnapshot struct {
	UserID          primitive.ObjectID
	ID              string
	MatchID         string
	MarketID        string
	Strike          float64
	Lots            int
	BuyPrice        float64
	SellPrice       float64
	LTP             float64
	PnL             float64
	RealizedPnL     float64
	ShortCollateral float64
	Status          string
}

// PositionView reads derived positions for a user. Implemented by
// positions.Service. Optional; a nil view disables position broadcasts and the
// close endpoint resolution.
type PositionView interface {
	PositionFor(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) (PositionSnapshot, bool)
	ResolveCloseTarget(ctx context.Context, userID primitive.ObjectID, positionID string) (PositionSnapshot, bool)
	OpenCloseTargets(ctx context.Context, userID primitive.ObjectID) ([]PositionSnapshot, error)
	ListOpenByMatch(ctx context.Context, matchID string) ([]PositionSnapshot, error)
}

type PositionProjectionWriter interface {
	ApplyExecution(ctx context.Context, exec executions.Execution, effect string) (PositionTransition, error)
}

type PositionTransition struct {
	NetLotsBefore          int
	AverageSellBefore      float64
	ShortCollateralBefore  float64
	ShortCollateralRelease float64
	ProjectionRevision     int64
}

type Service struct {
	repo           Repository
	markets        MarketReader
	matches        MatchReader
	wallets        WalletPort
	executions     ExecutionWriter
	positions      PositionView
	positionWriter PositionProjectionWriter
	publisher      EventPublisher

	locks sync.Map // map[string]*sync.Mutex keyed by user|market|strike
}

func NewService(
	repo Repository,
	markets MarketReader,
	matches MatchReader,
	wallets WalletPort,
	executions ExecutionWriter,
	positions PositionView,
	publisher EventPublisher,
) *Service {
	positionWriter, _ := positions.(PositionProjectionWriter)
	return &Service{
		repo:           repo,
		markets:        markets,
		matches:        matches,
		wallets:        wallets,
		executions:     executions,
		positions:      positions,
		positionWriter: positionWriter,
		publisher:      publisher,
	}
}

// lockFor serializes mutations for a single (user, market, strike) so two
// concurrent sells cannot oversell the same position. Returns an unlock func.
func (s *Service) lockFor(userID primitive.ObjectID, marketID string, strike float64) func() {
	key := userID.Hex() + "|" + marketID + "|" + formatStrike(strike)
	actual, _ := s.locks.LoadOrStore(key, &sync.Mutex{})
	mu := actual.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

type positionPlan struct {
	Effect           string
	Intent           string
	NetLotsBefore    int
	ProjectedLots    int
	OpenLongQty      int
	CloseLongQty     int
	OpenShortQty     int
	CoverShortQty    int
	ReservedQuantity int
	ReservedAmount   float64
}

func buildPositionPlan(side string, quantity int, effect string, netLots int, strike, orderPrice float64) (positionPlan, error) {
	if effect == "" {
		effect = PositionEffectAuto
	}
	plan := positionPlan{Effect: effect, NetLotsBefore: netLots}

	switch side {
	case "buy":
		if netLots < 0 {
			plan.CoverShortQty = minInt(quantity, -netLots)
		}
		plan.OpenLongQty = quantity - plan.CoverShortQty
		plan.ProjectedLots = netLots + quantity
	case "sell":
		if netLots > 0 {
			plan.CloseLongQty = minInt(quantity, netLots)
		}
		plan.OpenShortQty = quantity - plan.CloseLongQty
		plan.ProjectedLots = netLots - quantity
	default:
		return plan, ErrInvalidSide
	}

	switch effect {
	case PositionEffectAuto:
	case PositionEffectClose:
		if side == "buy" {
			if netLots >= 0 {
				return plan, newAPIError(http.StatusBadRequest, "No short position to cover")
			}
			if quantity > -netLots {
				return plan, newAPIError(http.StatusBadRequest, fmt.Sprintf("Cannot cover %d lots; only %d short", quantity, -netLots))
			}
		} else {
			if netLots <= 0 {
				return plan, newAPIError(http.StatusBadRequest, fmt.Sprintf("No open position for strike %s", formatStrike(strike)))
			}
			if quantity > netLots {
				return plan, newAPIError(http.StatusBadRequest, fmt.Sprintf("Cannot close %d lots; only %d long", quantity, netLots))
			}
		}
	case PositionEffectOpen:
		if side == "buy" && netLots < 0 {
			return plan, newAPIError(http.StatusBadRequest, "Cannot open long while short; use AUTO to cover or flip")
		}
		if side == "sell" && netLots > 0 {
			return plan, newAPIError(http.StatusBadRequest, "Cannot open short while long; use AUTO to close or flip")
		}
	default:
		return plan, newAPIError(http.StatusBadRequest, "positionEffect must be AUTO, OPEN, or CLOSE")
	}

	switch side {
	case "buy":
		plan.ReservedQuantity = plan.OpenLongQty
		plan.ReservedAmount = round2(orderPrice * float64(plan.OpenLongQty))
	case "sell":
		plan.ReservedQuantity = plan.OpenShortQty
		plan.ReservedAmount = round2(orderPrice * float64(plan.OpenShortQty) * ShortInitialMarginRate)
	}
	plan.Intent = planIntent(side, plan)
	return plan, nil
}

func planIntent(side string, plan positionPlan) string {
	if side == "buy" {
		switch {
		case plan.CoverShortQty > 0 && plan.OpenLongQty > 0:
			return "BUY_COVER_AND_OPEN_LONG"
		case plan.CoverShortQty > 0:
			return "BUY_TO_COVER"
		default:
			return "BUY_TO_OPEN_LONG"
		}
	}
	switch {
	case plan.CloseLongQty > 0 && plan.OpenShortQty > 0:
		return "SELL_CLOSE_AND_OPEN_SHORT"
	case plan.CloseLongQty > 0:
		return "SELL_TO_CLOSE"
	default:
		return "SELL_TO_OPEN_SHORT"
	}
}

func previewMessage(intent string, marketable bool) string {
	action := map[string]string{
		"BUY_COVER_AND_OPEN_LONG":   "Buy to cover and open long",
		"BUY_TO_COVER":              "Buy to cover short",
		"BUY_TO_OPEN_LONG":          "Buy to open long",
		"SELL_CLOSE_AND_OPEN_SHORT": "Sell to close and open short",
		"SELL_TO_CLOSE":             "Sell to close long",
		"SELL_TO_OPEN_SHORT":        "Sell to open short",
	}[intent]
	if action == "" {
		action = "Order"
	}
	if marketable {
		return action + " at current quote"
	}
	return action + " as resting limit order"
}

func shortCollateralRelease(avgSellPrice, fillPrice float64, coverQty int) float64 {
	if coverQty <= 0 {
		return 0
	}
	entry := avgSellPrice
	if entry <= 0 {
		entry = fillPrice
	}
	return round2(entry * float64(coverQty) * (1 + ShortInitialMarginRate))
}

func shortInitialMarginTopUp(order Order, fillPrice float64, openShortQty int) float64 {
	if openShortQty <= 0 || fillPrice <= 0 {
		return 0
	}
	required := round2(fillPrice * float64(openShortQty) * ShortInitialMarginRate)
	reserved := 0.0
	if order.ReservedAmount > 0 && order.ReservedQuantity > 0 {
		reservedQty := minInt(openShortQty, order.ReservedQuantity)
		reserved = round2(order.ReservedAmount * float64(reservedQty) / float64(order.ReservedQuantity))
	}
	if required <= reserved {
		return 0
	}
	return round2(required - reserved)
}

func futureOrderReserve(side string, remainingQty, projectedLots int, orderPrice float64) float64 {
	if remainingQty <= 0 || orderPrice <= 0 {
		return 0
	}
	openQty := remainingQty
	switch side {
	case "buy":
		if projectedLots < 0 {
			openQty = remainingQty + projectedLots
		}
	case "sell":
		if projectedLots > 0 {
			openQty = remainingQty - projectedLots
		}
	default:
		return 0
	}
	if openQty < 0 {
		openQty = 0
	}
	rate := 1.0
	if side == "sell" {
		rate = ShortInitialMarginRate
	}
	return round2(orderPrice * float64(openQty) * rate)
}

func orderFundsOperationContext(ctx context.Context, userID, orderID primitive.ObjectID, action string) context.Context {
	key := fmt.Sprintf("wallet:user:%s:order:%s:%s", userID.Hex(), orderID.Hex(), action)
	return wallet.WithOperationKey(ctx, key)
}

func fillWalletOperationContext(
	ctx context.Context,
	userID primitive.ObjectID,
	order *Order,
	executionID primitive.ObjectID,
	providerContract bool,
	action string,
) context.Context {
	scope := "execution:" + executionID.Hex()
	if providerContract && order != nil {
		clientOrderID := strings.TrimSpace(order.ClientOrderID)
		if isInternalClientOrderID(clientOrderID) {
			scope = "contract:" + clientOrderID
		}
	}
	key := fmt.Sprintf("wallet:user:%s:%s:%s", userID.Hex(), scope, action)
	return wallet.WithOperationKey(ctx, key)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *Service) GetUserOrders(ctx context.Context, userID primitive.ObjectID, status, matchID string) []Order {
	return s.repo.GetByUserID(ctx, userID, status, matchID)
}

func (s *Service) ListOrders(ctx context.Context, userID primitive.ObjectID, status, matchID, marketID, side string) []Order {
	return s.repo.List(ctx, OrderFilter{
		UserID:   userID,
		Status:   status,
		MatchID:  matchID,
		MarketID: marketID,
		Side:     side,
	})
}

func (s *Service) GetOrderByID(ctx context.Context, id primitive.ObjectID) (*Order, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *Service) PreviewOrder(ctx context.Context, userID primitive.ObjectID, req CreateOrderRequest) (*OrderPreviewResponse, error) {
	req = normalizeCreateRequest(req)
	if err := validateCreateRequest(req); err != nil {
		return nil, err
	}

	market, err := s.markets.GetMarketByID(ctx, req.MarketID)
	if err != nil || market == nil {
		return nil, ErrMarketNotFound
	}

	match, err := s.matches.GetMatchByID(ctx, req.MatchID)
	if err != nil || match == nil {
		return nil, ErrMatchNotFound
	}
	if err := validateMarketContract(req.MatchID, market, match); err != nil {
		return nil, err
	}
	if !s.markets.IsTradable(market) {
		if isProviderMatch(match) {
			if isMarketPermanentlyClosed(market) {
				return nil, ErrMarketClosed
			}
			return nil, ErrTradingStateChanged
		}
		if req.Side == "sell" {
			return nil, newAPIError(http.StatusBadRequest, "Market not active")
		}
		return nil, ErrMarketNotTradable
	}
	if !isMatchTradable(match) {
		if isProviderMatch(match) {
			return nil, ErrTradingStateChanged
		}
		return nil, ErrMatchNotTradable
	}

	pricingInput := markets.PricingInputFromMatch(*match)
	if req.PricingSnapshot != nil && !isProviderMatch(match) {
		pricingInput = normalizePricingSnapshot(*req.PricingSnapshot)
	}
	bid, ask, ok := s.markets.StrikeQuote(pricingInput, req.Strike)
	if !ok {
		return nil, ErrStrikeNotFound
	}

	orderPrice := round2(req.Price)
	fillPrice := 0.0
	shouldFill := false
	switch req.Type {
	case OrderTypeMarket:
		var ok bool
		fillPrice, ok = matchMarketOrder(req.Side, bid, ask)
		if !ok {
			if req.Side == "sell" {
				return nil, newAPIError(http.StatusBadRequest, "No bid available")
			}
			return nil, ErrNoLiquidity
		}
		orderPrice = fillPrice
		shouldFill = true
	case OrderTypeLimit:
		fillPrice, shouldFill = matchLimitOrder(req.Side, orderPrice, bid, ask)
	default:
		return nil, errors.New("unsupported order type")
	}

	position := s.executions.PositionSummary(ctx, userID, req.MatchID, market.ID.Hex(), req.Strike)
	plan, err := buildPositionPlan(req.Side, req.Quantity, req.PositionEffect, position.NetLots, req.Strike, orderPrice)
	if err != nil {
		return nil, err
	}

	account, err := s.wallets.GetWallet(ctx, userID)
	if err != nil {
		return nil, err
	}

	notional := round2(orderPrice * float64(req.Quantity))
	marginRequired := plan.ReservedAmount
	sufficientBalance := req.Side != "buy" || account.AvailableBalance >= marginRequired
	if req.Side == "sell" {
		sufficientBalance = account.AvailableBalance >= marginRequired
	}

	message := "Preview available"
	if !sufficientBalance {
		message = "Insufficient available wallet balance"
	} else if shouldFill {
		message = previewMessage(plan.Intent, true)
	} else {
		message = previewMessage(plan.Intent, false)
	}

	return &OrderPreviewResponse{
		MatchID:           req.MatchID,
		MarketID:          market.ID.Hex(),
		Strike:            req.Strike,
		Side:              req.Side,
		Type:              req.Type,
		PositionEffect:    plan.Effect,
		PositionIntent:    plan.Intent,
		Quantity:          req.Quantity,
		RequestedPrice:    round2(req.Price),
		OrderPrice:        orderPrice,
		ExecutablePrice:   round2(fillPrice),
		Bid:               bid,
		Ask:               ask,
		Notional:          notional,
		MarginRequired:    marginRequired,
		NetLotsBefore:     plan.NetLotsBefore,
		ProjectedLots:     plan.ProjectedLots,
		AvailableBalance:  round2(account.AvailableBalance),
		SufficientBalance: sufficientBalance,
		WillExecuteNow:    shouldFill,
		Message:           message,
		MatchStateVersion: match.StateVersion,
		TradingVersion:    match.TradingVersion,
		ExpiresAt:         time.Now().UTC().Add(5 * time.Second),
	}, nil
}

func (s *Service) CreateOrder(ctx context.Context, userID primitive.ObjectID, req CreateOrderRequest) (*Order, error) {
	req = normalizeCreateRequest(req)
	if isInternalClientOrderID(req.ClientOrderID) {
		return nil, ErrReservedClientOrderID
	}

	if existing, err := s.repo.FindByClientOrderID(ctx, userID, req.ClientOrderID); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	if err := validateCreateRequest(req); err != nil {
		return nil, err
	}

	market, err := s.markets.GetMarketByID(ctx, req.MarketID)
	if err != nil || market == nil {
		return nil, ErrMarketNotFound
	}

	// Serialize mutations for this (user, market, strike) so concurrent sells
	// cannot oversell the same position.
	unlock := s.lockFor(userID, market.ID.Hex(), req.Strike)
	defer unlock()

	match, err := s.matches.GetMatchByID(ctx, req.MatchID)
	if err != nil || match == nil {
		return nil, ErrMatchNotFound
	}
	if err := validateMarketContract(req.MatchID, market, match); err != nil {
		return nil, err
	}
	if !s.markets.IsTradable(market) {
		if isProviderMatch(match) {
			if isMarketPermanentlyClosed(market) {
				return nil, ErrMarketClosed
			}
			return nil, ErrTradingStateChanged
		}
		// Exit path uses the explicit error contract; buy path is unchanged.
		if req.Side == "sell" {
			return nil, newAPIError(http.StatusBadRequest, "Market not active")
		}
		return nil, ErrMarketNotTradable
	}
	if !isMatchTradable(match) {
		if isProviderMatch(match) {
			return nil, ErrTradingStateChanged
		}
		return nil, ErrMatchNotTradable
	}
	if isProviderMatch(match) && !providerRequestMatchesGate(req, match) {
		// Sportmonks ticks bump stateVersion continuously between preview and
		// submit. Rebind to the live gate for buys and sells while trading is
		// still healthy+open — pricing already uses the live match snapshot.
		req.ExpectedMatchStateVersion = match.StateVersion
		req.ExpectedTradingVersion = match.TradingVersion
		req.QuoteExpiresAt = time.Now().UTC().Add(5 * time.Second)
	}

	pricingInput := markets.PricingInputFromMatch(*match)
	if req.PricingSnapshot != nil && !isProviderMatch(match) {
		pricingInput = normalizePricingSnapshot(*req.PricingSnapshot)
	}
	bid, ask, ok := s.markets.StrikeQuote(pricingInput, req.Strike)
	if !ok {
		return nil, ErrStrikeNotFound
	}

	orderPrice := round2(req.Price)
	fillPrice := 0.0
	shouldFill := false
	switch req.Type {
	case OrderTypeMarket:
		var ok bool
		fillPrice, ok = matchMarketOrder(req.Side, bid, ask)
		if !ok {
			if req.Side == "sell" {
				return nil, newAPIError(http.StatusBadRequest, "No bid available")
			}
			return nil, ErrNoLiquidity
		}
		orderPrice = fillPrice
		shouldFill = true
	case OrderTypeLimit:
		fillPrice, shouldFill = matchLimitOrder(req.Side, orderPrice, bid, ask)
	default:
		return nil, errors.New("unsupported order type")
	}

	position := s.executions.PositionSummary(ctx, userID, req.MatchID, market.ID.Hex(), req.Strike)
	plan, err := buildPositionPlan(req.Side, req.Quantity, req.PositionEffect, position.NetLots, req.Strike, orderPrice)
	if err != nil {
		return nil, err
	}

	order := Order{
		ClientOrderID:     req.ClientOrderID,
		UserID:            userID,
		MatchID:           req.MatchID,
		MarketID:          market.ID.Hex(),
		Strike:            req.Strike,
		Side:              req.Side,
		Type:              req.Type,
		PositionEffect:    plan.Effect,
		PositionIntent:    plan.Intent,
		Quantity:          req.Quantity,
		Price:             orderPrice,
		ReservedAmount:    plan.ReservedAmount,
		ReservedQuantity:  plan.ReservedQuantity,
		FilledQuantity:    0,
		RemainingQuantity: req.Quantity,
		Status:            StatusOpen,
		MatchStateVersion: match.StateVersion,
		TradingVersion:    match.TradingVersion,
		QuoteExpiresAt:    req.QuoteExpiresAt,
	}

	var created *Order
	const gateAttempts = 3
	for attempt := 0; attempt < gateAttempts; attempt++ {
		if attempt > 0 {
			// Give the racing feed tick time to commit before re-reading the gate;
			// back-to-back retries land inside the same sync window.
			if waitErr := sleepCtx(ctx, gateRetryBackoff); waitErr != nil {
				return nil, waitErr
			}
			// Re-load live match/market after a Sportmonks tick raced the gate.
			if refreshed, refreshErr := s.markets.GetMarketByID(ctx, req.MarketID); refreshErr == nil && refreshed != nil {
				market = refreshed
			}
			if refreshed, refreshErr := s.matches.GetMatchByID(ctx, req.MatchID); refreshErr == nil && refreshed != nil {
				match = refreshed
			}
			if !s.markets.IsTradable(market) || !isMatchTradable(match) {
				if isMarketPermanentlyClosed(market) {
					return nil, ErrMarketClosed
				}
				return nil, ErrTradingStateChanged
			}
			if err := validateMarketContract(req.MatchID, market, match); err != nil {
				return nil, err
			}
			req.ExpectedMatchStateVersion = match.StateVersion
			req.ExpectedTradingVersion = match.TradingVersion
			req.QuoteExpiresAt = time.Now().UTC().Add(5 * time.Second)
			order.MatchStateVersion = match.StateVersion
			order.TradingVersion = match.TradingVersion
			order.QuoteExpiresAt = req.QuoteExpiresAt
			// Re-price market orders against the live snapshot so fills stay consistent.
			pricingInput = markets.PricingInputFromMatch(*match)
			bid, ask, ok = s.markets.StrikeQuote(pricingInput, req.Strike)
			if !ok {
				return nil, ErrStrikeNotFound
			}
			if req.Type == OrderTypeMarket {
				fillPrice, ok = matchMarketOrder(req.Side, bid, ask)
				if !ok {
					if req.Side == "sell" {
						return nil, newAPIError(http.StatusBadRequest, "No bid available")
					}
					return nil, ErrNoLiquidity
				}
				orderPrice = fillPrice
				order.Price = orderPrice
				plan, err = buildPositionPlan(req.Side, req.Quantity, req.PositionEffect, position.NetLots, req.Strike, orderPrice)
				if err != nil {
					return nil, err
				}
				order.ReservedAmount = plan.ReservedAmount
				order.ReservedQuantity = plan.ReservedQuantity
				order.PositionEffect = plan.Effect
				order.PositionIntent = plan.Intent
			}
		}

		err = s.repo.DoTx(ctx, func(txCtx context.Context) error {
			if err := s.verifyTradingGates(txCtx, match, market.ID.Hex(), match.StateVersion, match.TradingVersion); err != nil {
				return err
			}
			var txErr error
			created, txErr = s.repo.Create(txCtx, order)
			if txErr != nil {
				return txErr
			}

			if plan.ReservedAmount > 0 {
				fundsCtx := orderFundsOperationContext(txCtx, userID, created.ID, "reserve")
				_, txErr = s.wallets.ReserveOrderMargin(fundsCtx, userID, plan.ReservedAmount, created.ID.Hex(), fmt.Sprintf("Reserve margin for order %s", created.ID.Hex()))
				if txErr != nil {
					return txErr
				}
			}
			return nil
		})
		if err == nil {
			break
		}
		if errors.Is(err, wallet.ErrInsufficientFunds) {
			return nil, ErrInsufficientBalance
		}
		if !errors.Is(err, ErrTradingStateChanged) || !isProviderMatch(match) || attempt == gateAttempts-1 {
			return nil, err
		}
	}

	if err != nil {
		if errors.Is(err, wallet.ErrInsufficientFunds) {
			return nil, ErrInsufficientBalance
		}
		return nil, err
	}

	if !shouldFill {
		s.broadcastOrderAndPosition(ctx, userID, created)
		return created, nil
	}

	var filled *Order
	for attempt := 0; attempt < gateAttempts; attempt++ {
		if attempt > 0 {
			if waitErr := sleepCtx(ctx, gateRetryBackoff); waitErr != nil {
				err = waitErr
				break
			}
		}
		filled, err = s.applyFill(ctx, userID, created, fillPrice, created.RemainingQuantity)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrTradingStateChanged) || !isProviderMatch(match) || attempt == gateAttempts-1 {
			break
		}
	}
	if err != nil {
		// Reservation and execution are separately fenced transactions. If the
		// provider gate or position changes between them, leave no marketable
		// order or margin behind for a matcher to execute against stale state.
		if shouldCancelFailedImmediateFill(err) {
			cancelled, cancelErr := s.CancelOrder(ctx, created.ID, userID)
			if cancelErr != nil {
				return nil, fmt.Errorf("%w (failed to cancel fenced order: %v)", err, cancelErr)
			}
			if cancelled == nil {
				current, lookupErr := s.repo.GetByID(ctx, created.ID)
				if lookupErr != nil {
					return nil, fmt.Errorf("%w (failed to confirm fenced order state: %v)", err, lookupErr)
				}
				if current != nil && (current.Status == StatusOpen || current.Status == StatusPartiallyFilled) {
					return nil, fmt.Errorf("%w (fenced order remains cancellable)", err)
				}
			}
		}
		if errors.Is(err, wallet.ErrInsufficientFunds) {
			return nil, ErrInsufficientBalance
		}
		return nil, err
	}
	s.broadcastOrderAndPosition(ctx, userID, filled)
	return filled, nil
}

func normalizePricingSnapshot(snapshot markets.PriceCalculationInput) markets.PriceCalculationInput {
	if snapshot.Innings != 1 && snapshot.Innings != 2 {
		return snapshot
	}
	if snapshot.WicketsLost < 0 {
		snapshot.WicketsLost = 0
	}
	if snapshot.WicketsLost > 10 {
		snapshot.WicketsLost = 10
	}
	if snapshot.CurrentScore < 0 {
		snapshot.CurrentScore = 0
	}
	if snapshot.BallsLeft < 0 {
		snapshot.BallsLeft = 0
	}
	maxBalls := matches.BallsODI
	if snapshot.BallsLeft > maxBalls {
		snapshot.BallsLeft = maxBalls
	}
	if snapshot.BallsBowled < 0 {
		snapshot.BallsBowled = 0
	}
	if snapshot.BallsBowled > maxBalls {
		snapshot.BallsBowled = maxBalls
	}
	return snapshot
}

func isProviderMatch(match *matches.Match) bool {
	return match != nil && strings.EqualFold(strings.TrimSpace(match.DataSource), matches.DataSourceSportmonks)
}

// isMarketPermanentlyClosed distinguishes settled/void contracts from transient
// suspensions. Settled markets stay in the match's market list after an innings
// ends; surfacing TRADING_STATE_CHANGED for them told users to retry an order
// that can never succeed.
func isMarketPermanentlyClosed(market *markets.Market) bool {
	if market == nil {
		return false
	}
	switch market.Lifecycle {
	case markets.MarketLifecycleSettling, markets.MarketLifecycleSettled, markets.MarketLifecycleVoid:
		return true
	}
	return market.Status == markets.MarketStatusClosed
}

// gateRetryBackoff spaces the gate retry attempts so they do not all land
// inside the same Sportmonks feed-sync window (a tick commits in a few ms;
// back-to-back retries observed the same in-flight state all three times).
const gateRetryBackoff = 75 * time.Millisecond

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func providerRequestMatchesGate(req CreateOrderRequest, match *matches.Match) bool {
	if !isProviderMatch(match) {
		return true
	}
	now := time.Now().UTC()
	return req.ExpectedMatchStateVersion == match.StateVersion &&
		req.ExpectedTradingVersion == match.TradingVersion &&
		!req.QuoteExpiresAt.IsZero() &&
		now.Before(req.QuoteExpiresAt) &&
		!req.QuoteExpiresAt.After(now.Add(6*time.Second))
}

func (s *Service) verifyTradingGates(ctx context.Context, match *matches.Match, marketID string, stateVersion, tradingVersion int64) error {
	if !isProviderMatch(match) {
		return nil
	}
	verifier, ok := s.matches.(TradingGateVerifier)
	if !ok {
		return ErrTradingStateChanged
	}
	verified, valid, err := verifier.VerifyTradingGate(ctx, match.ID.Hex(), stateVersion, tradingVersion)
	if err != nil {
		// Propagate driver errors raw. The gate write races the feed sync on the
		// same match/market docs, and a WriteConflict inside the order transaction
		// carries the TransientTransactionError label — WithTransaction retries it
		// automatically only if the label survives. Collapsing it into
		// ErrTradingStateChanged surfaced spurious 409s on healthy open matches.
		return err
	}
	if !valid || !isMatchTradable(verified) {
		return ErrTradingStateChanged
	}
	marketVerifier, ok := s.markets.(ProviderMarketGateVerifier)
	if !ok {
		return ErrTradingStateChanged
	}
	verifiedMarket, valid, err := marketVerifier.VerifyProviderMarketGate(ctx, marketID, verified.StateVersion, verified.TradingVersion)
	if err != nil {
		return err
	}
	if !valid || !s.markets.IsTradable(verifiedMarket) {
		return ErrTradingStateChanged
	}
	if err := validateMarketContract(verified.ID.Hex(), verifiedMarket, verified); err != nil {
		return ErrTradingStateChanged
	}
	return nil
}

func validateMarketContract(requestMatchID string, market *markets.Market, match *matches.Match) error {
	if market == nil || match == nil {
		return ErrMarketContractMismatch
	}
	requestMatchID = strings.TrimSpace(requestMatchID)
	marketMatchID := strings.TrimSpace(market.MatchID)
	if marketMatchID == "" {
		// Test doubles and historical in-memory fixtures may omit ownership; a
		// provider contract never may.
		if isProviderMatch(match) {
			return ErrMarketContractMismatch
		}
		return nil
	}
	providerMatch := isProviderMatch(match)
	belongs := marketMatchID == requestMatchID || marketMatchID == match.ID.Hex()
	if !belongs && !providerMatch && len(match.ID.Hex()) >= 2 {
		belongs = marketMatchID == match.ID.Hex()[len(match.ID.Hex())-2:]
	}
	if !belongs {
		return ErrMarketContractMismatch
	}
	if providerMatch {
		if market.Kind != markets.MarketKindInningsScore ||
			market.FormulaVersion != markets.FormulaVersionInningsScoreV1 {
			return ErrMarketContractMismatch
		}
		// Market innings is the contract. Match.Innings can advance a tick before
		// the prior innings market is closed — requiring equality caused intermittent
		// TRADING_STATE_CHANGED on otherwise open markets.
	} else if market.Innings > 0 && market.Innings != match.Innings {
		return ErrMarketContractMismatch
	}
	return nil
}

func (s *Service) CancelOrder(ctx context.Context, id, userID primitive.ObjectID) (*Order, error) {
	existing, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	if existing == nil || existing.UserID != userID {
		return nil, nil
	}
	if isInternalClientOrderID(existing.ClientOrderID) {
		return nil, ErrInternalOrderCancel
	}
	if existing.Status != StatusOpen && existing.Status != StatusPartiallyFilled {
		return nil, nil
	}

	var cancelled *Order
	err = s.repo.DoTx(ctx, func(txCtx context.Context) error {
		var txErr error
		cancelled, txErr = s.repo.Cancel(txCtx, id, userID)
		if txErr != nil || cancelled == nil {
			return txErr
		}
		if remainingReserve := cancelled.RemainingReservedAmount(); remainingReserve > 0 {
			fundsCtx := orderFundsOperationContext(txCtx, userID, cancelled.ID, "cancel-release")
			_, txErr = s.wallets.ReleaseOrderMargin(fundsCtx, userID, remainingReserve, cancelled.ID.Hex(), fmt.Sprintf("Release margin for cancelled order %s", cancelled.ID.Hex()))
		}
		return txErr
	})
	if err != nil {
		return nil, err
	}
	if cancelled != nil {
		// Position is unchanged on cancel, but the order state changed.
		s.broadcastOrderAndPosition(ctx, userID, cancelled)
	}
	return cancelled, nil
}

// ClosePosition resolves a derived position by id and submits the opposite-side
// order to exit it. Quantity defaults to the full open lots when zero.
func (s *Service) ClosePosition(ctx context.Context, userID primitive.ObjectID, positionID, orderType string, quantity int, price float64) (*Order, error) {
	if s.positions == nil {
		return nil, newAPIError(http.StatusBadRequest, "Position lookup is unavailable")
	}

	target, ok := s.positions.ResolveCloseTarget(ctx, userID, positionID)
	if !ok || target.Lots == 0 {
		return nil, newAPIError(http.StatusBadRequest, "No open position to close")
	}

	openLots := absInt(target.Lots)
	if quantity <= 0 {
		quantity = openLots
	}
	if quantity > openLots {
		return nil, newAPIError(http.StatusBadRequest, fmt.Sprintf("Cannot close %d lots; only %d open", quantity, openLots))
	}

	orderType = strings.ToUpper(strings.TrimSpace(orderType))
	if orderType == "" {
		orderType = OrderTypeMarket
	}

	side := "sell"
	if target.Lots < 0 {
		side = "buy"
	}

	request := CreateOrderRequest{
		MatchID:        target.MatchID,
		MarketID:       target.MarketID,
		Strike:         target.Strike,
		Side:           side,
		Type:           orderType,
		PositionEffect: PositionEffectClose,
		Quantity:       quantity,
		Price:          price,
	}
	if err := s.attachProviderFence(ctx, &request); err != nil {
		return nil, err
	}
	order, err := s.CreateOrder(ctx, userID, request)
	// Sportmonks can bump versions between fence attach and submit; one refresh is enough.
	if errors.Is(err, ErrTradingStateChanged) {
		if fenceErr := s.attachProviderFence(ctx, &request); fenceErr != nil {
			return nil, fenceErr
		}
		order, err = s.CreateOrder(ctx, userID, request)
	}
	return order, err
}

// CloseAllPositions submits MARKET close orders for every open position
// owned by the user. Individual failures are reported without hiding successful
// exits, since liquidity can differ by strike.
func (s *Service) CloseAllPositions(ctx context.Context, userID primitive.ObjectID, orderType string) (*CloseAllPositionsResponse, error) {
	if s.positions == nil {
		return nil, newAPIError(http.StatusBadRequest, "Position lookup is unavailable")
	}

	orderType = strings.ToUpper(strings.TrimSpace(orderType))
	if orderType == "" {
		orderType = OrderTypeMarket
	}
	if orderType != OrderTypeMarket {
		return nil, newAPIError(http.StatusBadRequest, "Exit all supports MARKET exits only")
	}

	targets, err := s.positions.OpenCloseTargets(ctx, userID)
	if err != nil {
		return nil, err
	}

	result := &CloseAllPositionsResponse{
		Orders:   []*Order{},
		Failures: []CloseAllPositionFailure{},
	}

	for _, target := range targets {
		if target.Lots == 0 || target.Status == "closed" {
			continue
		}
		result.Requested++
		side := "sell"
		if target.Lots < 0 {
			side = "buy"
		}
		quantity := absInt(target.Lots)

		request := CreateOrderRequest{
			MatchID:        target.MatchID,
			MarketID:       target.MarketID,
			Strike:         target.Strike,
			Side:           side,
			Type:           OrderTypeMarket,
			PositionEffect: PositionEffectClose,
			Quantity:       quantity,
		}
		if err := s.attachProviderFence(ctx, &request); err != nil {
			result.Failed++
			result.Failures = append(result.Failures, CloseAllPositionFailure{
				MatchID: target.MatchID, MarketID: target.MarketID, Strike: target.Strike,
				Quantity: quantity, Message: serviceErrorMessage(err),
			})
			continue
		}
		order, err := s.CreateOrder(ctx, userID, request)
		if errors.Is(err, ErrTradingStateChanged) {
			if fenceErr := s.attachProviderFence(ctx, &request); fenceErr == nil {
				order, err = s.CreateOrder(ctx, userID, request)
			}
		}
		if err != nil {
			result.Failed++
			result.Failures = append(result.Failures, CloseAllPositionFailure{
				MatchID:  target.MatchID,
				MarketID: target.MarketID,
				Strike:   target.Strike,
				Quantity: quantity,
				Message:  serviceErrorMessage(err),
			})
			continue
		}

		result.Submitted++
		result.Orders = append(result.Orders, order)
	}

	return result, nil
}

func (s *Service) attachProviderFence(ctx context.Context, request *CreateOrderRequest) error {
	if request == nil {
		return errors.New("order request is required")
	}
	match, err := s.matches.GetMatchByID(ctx, request.MatchID)
	if err != nil {
		return err
	}
	if !isProviderMatch(match) {
		return nil
	}
	if !isMatchTradable(match) {
		return ErrTradingStateChanged
	}
	request.ExpectedMatchStateVersion = match.StateVersion
	request.ExpectedTradingVersion = match.TradingVersion
	request.QuoteExpiresAt = time.Now().UTC().Add(5 * time.Second)
	return nil
}

func serviceErrorMessage(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Message
	}
	return err.Error()
}

// broadcastOrderAndPosition streams order + position updates for the order owner.
func (s *Service) broadcastOrderAndPosition(ctx context.Context, userID primitive.ObjectID, order *Order) {
	if s.publisher == nil || order == nil {
		return
	}

	side := strings.ToUpper(order.Side)
	s.publisher.Publish(realtime.UserOrdersTopic(userID.Hex()), map[string]any{
		"orderId":           order.ID.Hex(),
		"marketId":          order.MarketID,
		"strike":            order.Strike,
		"side":              side,
		"status":            order.Status,
		"filledQuantity":    order.FilledQuantity,
		"remainingQuantity": order.RemainingQuantity,
		"averageFillPrice":  order.AverageFillPrice,
		"positionEffect":    order.PositionEffect,
		"positionIntent":    order.PositionIntent,
	})

	if s.positions == nil {
		return
	}
	snap, ok := s.positions.PositionFor(ctx, userID, order.MatchID, order.MarketID, order.Strike)
	if !ok {
		return
	}
	entryPrice := snap.BuyPrice
	if snap.Lots < 0 && snap.SellPrice > 0 {
		entryPrice = snap.SellPrice
	}
	s.publisher.Publish(realtime.UserPositionsTopic(userID.Hex()), map[string]any{
		"userId":            userID.Hex(),
		"marketId":          snap.MarketID,
		"strike":            snap.Strike,
		"quantity":          snap.Lots,
		"lots":              snap.Lots,
		"averageEntryPrice": entryPrice,
		"buyPrice":          snap.BuyPrice,
		"sellPrice":         snap.SellPrice,
		"ltp":               snap.LTP,
		"pnl":               snap.PnL,
		"unrealizedPnL":     snap.PnL,
		"realizedPnl":       snap.RealizedPnL,
		"status":            snap.Status,
		"timestamp":         time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Service) applyFill(ctx context.Context, userID primitive.ObjectID, order *Order, fillPrice float64, fillQty int) (*Order, error) {
	return s.applyFillWithTradingGate(ctx, userID, order, fillPrice, fillQty, true)
}

func (s *Service) applySettlementFill(ctx context.Context, userID primitive.ObjectID, order *Order, fillPrice float64, fillQty int) (*Order, error) {
	return s.applyFillWithTradingGate(ctx, userID, order, fillPrice, fillQty, false)
}

func (s *Service) applyFillWithTradingGate(ctx context.Context, userID primitive.ObjectID, order *Order, fillPrice float64, fillQty int, enforceTradingGate bool) (*Order, error) {
	if fillQty <= 0 || fillQty > order.RemainingQuantity {
		return order, nil
	}

	fillNotional := round2(fillPrice * float64(fillQty))
	newFilled := order.FilledQuantity + fillQty
	newRemaining := order.RemainingQuantity - fillQty
	avgFill := fillPrice
	if newFilled > fillQty {
		prevNotional := order.AverageFillPrice * float64(order.FilledQuantity)
		avgFill = round2((prevNotional + fillPrice*float64(fillQty)) / float64(newFilled))
	}
	status := StatusPartiallyFilled
	if newRemaining == 0 {
		status = StatusExecuted
	}
	execution := executions.Execution{
		ID:              primitive.NewObjectID(),
		UserID:          userID,
		OrderID:         order.ID,
		MatchID:         order.MatchID,
		MarketID:        order.MarketID,
		Strike:          order.Strike,
		Side:            order.Side,
		Price:           fillPrice,
		Quantity:        fillQty,
		LiquiditySource: executions.LiquiditySystemMarketMaker,
		CreatedAt:       time.Now().UTC(),
	}
	if !enforceTradingGate && strings.HasPrefix(strings.ToLower(strings.TrimSpace(order.ClientOrderID)), voidClientOrderPrefix) {
		execution.LiquiditySource = executions.LiquidityProviderVoidReverse
	}
	providerVoidReversal := execution.LiquiditySource == executions.LiquidityProviderVoidReverse
	providerSettlement := !enforceTradingGate && strings.HasPrefix(strings.ToLower(strings.TrimSpace(order.ClientOrderID)), settlementClientOrderPrefix)

	var updated *Order
	err := s.repo.DoTx(ctx, func(txCtx context.Context) error {
		if enforceTradingGate {
			match, txErr := s.matches.GetMatchByID(txCtx, order.MatchID)
			if txErr != nil || match == nil || !isMatchTradable(match) {
				return ErrTradingStateChanged
			}
			// Use the live match gate versions. Ball-by-ball Sportmonks polls bump
			// stateVersion continuously; requiring the create-time versions here
			// makes market sells (especially exits) fail spuriously after a buy.
			if txErr := s.verifyTradingGates(txCtx, match, order.MarketID, match.StateVersion, match.TradingVersion); txErr != nil {
				return txErr
			}
		}

		transition := PositionTransition{}
		var txErr error
		if s.positionWriter != nil {
			transition, txErr = s.positionWriter.ApplyExecution(txCtx, execution, order.PositionEffect)
			if txErr != nil {
				return txErr
			}
		} else {
			// Legacy/manual test wiring may omit the projection. Production API
			// wiring always supplies the transactional projection writer.
			before, summaryErr := s.executions.PositionSummaryWithError(txCtx, userID, order.MatchID, order.MarketID, order.Strike)
			if summaryErr != nil {
				return summaryErr
			}
			transition.NetLotsBefore = before.NetLots
			transition.AverageSellBefore = before.AvgSellPrice
			transition.ShortCollateralBefore = round2(before.OpenShortNotional * (1 + ShortInitialMarginRate))
		}

		plan, txErr := buildPositionPlan(
			order.Side,
			fillQty,
			order.PositionEffect,
			transition.NetLotsBefore,
			order.Strike,
			order.Price,
		)
		if txErr != nil {
			return fmt.Errorf("%w: %v", ErrInsufficientPosition, txErr)
		}
		futureReserve := futureOrderReserve(order.Side, newRemaining, plan.ProjectedLots, order.Price)

		updated, txErr = s.repo.UpdateFill(txCtx, order.ID, FillUpdate{
			ExpectedFilledQuantity:    order.FilledQuantity,
			ExpectedRemainingQuantity: order.RemainingQuantity,
			ExpectedStatus:            order.Status,
			FilledQuantity:            newFilled,
			RemainingQuantity:         newRemaining,
			AverageFillPrice:          avgFill,
			OutstandingReserve:        futureReserve,
			ReserveReconciled:         true,
			Status:                    status,
		})
		if txErr != nil {
			return txErr
		}
		if updated == nil {
			return errors.New("order fill state changed")
		}
		if !providerVoidReversal {
			switch order.Side {
			case "buy":
				if plan.CoverShortQty > 0 {
					release := transition.ShortCollateralRelease
					if release <= 0 && transition.ShortCollateralBefore > 0 {
						release = proRataShortCollateralRelease(transition.ShortCollateralBefore, transition.NetLotsBefore, plan.CoverShortQty)
					}
					if release <= 0 {
						release = shortCollateralRelease(transition.AverageSellBefore, fillPrice, plan.CoverShortQty)
					}
					if release > 0 {
						fundsCtx := fillWalletOperationContext(txCtx, userID, order, execution.ID, !enforceTradingGate, "cover-collateral-release")
						if _, txErr := s.wallets.ReleaseOrderMargin(fundsCtx, userID, release, order.ID.Hex(), fmt.Sprintf("Release short collateral for cover order %s", order.ID.Hex())); txErr != nil {
							return txErr
						}
					}
				}
				heldReserve := order.RemainingReservedAmount()
				fillReserve := round2(fillPrice * float64(plan.OpenLongQty))
				targetReserve := round2(fillReserve + futureReserve)
				if topUp := round2(targetReserve - heldReserve); topUp > 0 {
					fundsCtx := fillWalletOperationContext(txCtx, userID, order, execution.ID, !enforceTradingGate, "buy-margin-topup")
					if _, txErr := s.wallets.ReserveOrderMargin(fundsCtx, userID, topUp, order.ID.Hex(), fmt.Sprintf("Top up buy margin for order %s", order.ID.Hex())); txErr != nil {
						return txErr
					}
					heldReserve = round2(heldReserve + topUp)
				}
				reserveRelease := round2(heldReserve - futureReserve)
				if fillNotional > 0 {
					fundsCtx := fillWalletOperationContext(txCtx, userID, order, execution.ID, !enforceTradingGate, "buy-fill")
					settleBuy := s.wallets.SettleBuyFill
					if providerSettlement && plan.CoverShortQty > 0 {
						settleBuy = s.wallets.SettleForcedBuyFill
					}
					if _, txErr := settleBuy(fundsCtx, userID, fillNotional, reserveRelease, order.ID.Hex(), fmt.Sprintf("Buy fill for order %s", order.ID.Hex())); txErr != nil {
						return txErr
					}
				} else if reserveRelease > 0 {
					fundsCtx := fillWalletOperationContext(txCtx, userID, order, execution.ID, !enforceTradingGate, "zero-price-reserve-release")
					if _, txErr := s.wallets.ReleaseOrderMargin(fundsCtx, userID, reserveRelease, order.ID.Hex(), fmt.Sprintf("Release margin for zero-price fill %s", order.ID.Hex())); txErr != nil {
						return txErr
					}
				}
			case "sell":
				heldReserve := order.RemainingReservedAmount()
				requiredInitialMargin := round2(fillPrice * float64(plan.OpenShortQty) * ShortInitialMarginRate)
				targetReserve := round2(requiredInitialMargin + futureReserve)
				if heldReserve > targetReserve {
					release := round2(heldReserve - targetReserve)
					fundsCtx := fillWalletOperationContext(txCtx, userID, order, execution.ID, !enforceTradingGate, "sell-reserve-release")
					if _, txErr := s.wallets.ReleaseOrderMargin(fundsCtx, userID, release, order.ID.Hex(), fmt.Sprintf("Release unused sell margin for order %s", order.ID.Hex())); txErr != nil {
						return txErr
					}
				}
				if topUp := round2(targetReserve - heldReserve); topUp > 0 {
					fundsCtx := fillWalletOperationContext(txCtx, userID, order, execution.ID, !enforceTradingGate, "short-margin-topup")
					if _, txErr := s.wallets.ReserveOrderMargin(fundsCtx, userID, topUp, order.ID.Hex(), fmt.Sprintf("Top up short margin for order %s", order.ID.Hex())); txErr != nil {
						return txErr
					}
				}
				if plan.CloseLongQty > 0 {
					proceeds := round2(fillPrice * float64(plan.CloseLongQty))
					if proceeds > 0 {
						fundsCtx := fillWalletOperationContext(txCtx, userID, order, execution.ID, !enforceTradingGate, "sell-fill")
						if _, txErr := s.wallets.SettleSellFill(fundsCtx, userID, proceeds, order.ID.Hex(), fmt.Sprintf("Sell fill for order %s", order.ID.Hex())); txErr != nil {
							return txErr
						}
					}
				}
				if plan.OpenShortQty > 0 {
					proceeds := round2(fillPrice * float64(plan.OpenShortQty))
					fundsCtx := fillWalletOperationContext(txCtx, userID, order, execution.ID, !enforceTradingGate, "short-open-proceeds")
					if _, txErr := s.wallets.SettleShortOpenFill(fundsCtx, userID, proceeds, order.ID.Hex(), fmt.Sprintf("Short sale proceeds for order %s", order.ID.Hex())); txErr != nil {
						return txErr
					}
				}
			}
		}

		_, txErr = s.executions.Create(txCtx, execution)
		if txErr != nil {
			return txErr
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func proRataShortCollateralRelease(collateral float64, netLotsBefore, coverQty int) float64 {
	if collateral <= 0 || netLotsBefore >= 0 || coverQty <= 0 {
		return 0
	}
	shortLots := -netLotsBefore
	if coverQty >= shortLots {
		return round2(collateral)
	}
	return round2(collateral * float64(coverQty) / float64(shortLots))
}

func normalizeCreateRequest(req CreateOrderRequest) CreateOrderRequest {
	req.MatchID = strings.TrimSpace(req.MatchID)
	req.MarketID = strings.TrimSpace(req.MarketID)
	req.Side = strings.ToLower(strings.TrimSpace(req.Side))
	req.Type = strings.ToUpper(strings.TrimSpace(req.Type))
	if req.Type == "" {
		req.Type = OrderTypeLimit
	}
	req.PositionEffect = strings.ToUpper(strings.TrimSpace(req.PositionEffect))
	if req.PositionEffect == "" {
		req.PositionEffect = PositionEffectAuto
	}
	req.ClientOrderID = strings.TrimSpace(req.ClientOrderID)
	return req
}

func validateCreateRequest(req CreateOrderRequest) error {
	if isInternalClientOrderID(req.ClientOrderID) {
		return ErrReservedClientOrderID
	}
	if req.MatchID == "" || req.MarketID == "" {
		return errors.New("matchId and marketId are required")
	}
	if req.Side != "buy" && req.Side != "sell" {
		return ErrInvalidSide
	}
	if req.Quantity <= 0 {
		return ErrInvalidQuantity
	}
	if req.Strike <= 0 {
		return ErrInvalidStrike
	}
	if req.Type != OrderTypeLimit && req.Type != OrderTypeMarket {
		return errors.New("only LIMIT and MARKET orders are supported")
	}
	if req.PositionEffect != PositionEffectAuto && req.PositionEffect != PositionEffectOpen && req.PositionEffect != PositionEffectClose {
		return newAPIError(http.StatusBadRequest, "positionEffect must be AUTO, OPEN, or CLOSE")
	}
	if req.Type == OrderTypeLimit && req.Price <= 0 {
		return ErrInvalidPrice
	}
	return nil
}

func shouldCancelFailedImmediateFill(err error) bool {
	return errors.Is(err, ErrTradingStateChanged) ||
		errors.Is(err, ErrInsufficientPosition) ||
		errors.Is(err, wallet.ErrInsufficientFunds) ||
		errors.Is(err, wallet.ErrIdempotencyConflict)
}

func isInternalClientOrderID(clientOrderID string) bool {
	clientOrderID = strings.ToLower(strings.TrimSpace(clientOrderID))
	return strings.HasPrefix(clientOrderID, settlementClientOrderPrefix) ||
		strings.HasPrefix(clientOrderID, voidClientOrderPrefix)
}

func isMatchTradable(match *matches.Match) bool {
	return matches.IsTradable(match)
}

func matchLimitOrder(side string, limitPrice, bid, ask float64) (fillPrice float64, ok bool) {
	const marketableTolerance = 0.005

	switch side {
	case "buy":
		if ask > 0 && limitPrice+marketableTolerance >= ask {
			return ask, true
		}
	case "sell":
		if bid > 0 && limitPrice-marketableTolerance <= bid {
			return bid, true
		}
	}
	return 0, false
}

func matchMarketOrder(side string, bid, ask float64) (fillPrice float64, ok bool) {
	switch side {
	case "buy":
		if ask > 0 {
			return ask, true
		}
	case "sell":
		if bid > 0 {
			return bid, true
		}
	}
	return 0, false
}
