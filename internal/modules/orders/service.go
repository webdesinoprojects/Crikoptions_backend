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
	ErrMarketNotFound       = errors.New("market not found")
	ErrMatchNotFound        = errors.New("match not found")
	ErrInvalidSide          = errors.New("invalid side, must be 'buy' or 'sell'")
	ErrInvalidQuantity      = errors.New("quantity must be positive")
	ErrInvalidPrice         = errors.New("price must be positive")
	ErrInvalidStrike        = errors.New("strike must be positive")
	ErrMarketNotTradable    = errors.New("market is not open for trading")
	ErrMatchNotTradable     = errors.New("match is not live for trading")
	ErrInsufficientBalance  = errors.New("insufficient available wallet balance")
	ErrInsufficientPosition = errors.New("insufficient position to sell")
	ErrStrikeNotFound       = errors.New("strike not found in option chain")
	ErrNoLiquidity          = errors.New("no executable market quote available")
)

const ShortInitialMarginRate = 1.0

type MatchReader interface {
	GetMatchByID(ctx context.Context, id string) (*matches.Match, error)
}

type MarketReader interface {
	GetMarketByID(ctx context.Context, id string) (*markets.Market, error)
	GetMarketsByMatchID(ctx context.Context, matchID string) []markets.Market
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
}

type ExecutionWriter interface {
	Create(ctx context.Context, exec executions.Execution) (*executions.Execution, error)
	OpenLongQty(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) int
	PositionSummary(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) executions.PositionSummary
}

// EventPublisher streams realtime updates (implemented by realtime.Hub). It is
// optional; a nil publisher disables broadcasts.
type EventPublisher interface {
	Publish(topic string, data any)
}

// PositionSnapshot is the post-fill view of a user's position for a strike,
// used both for WebSocket broadcasts and the close endpoint.
type PositionSnapshot struct {
	UserID      primitive.ObjectID
	ID          string
	MatchID     string
	MarketID    string
	Strike      float64
	Lots        int
	BuyPrice    float64
	SellPrice   float64
	LTP         float64
	PnL         float64
	RealizedPnL float64
	Status      string
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
	ApplyExecution(ctx context.Context, exec executions.Execution) error
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
		reserved = round2(order.ReservedAmount * float64(openShortQty) / float64(order.ReservedQuantity))
	}
	if required <= reserved {
		return 0
	}
	return round2(required - reserved)
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
	if !s.markets.IsTradable(market) {
		if req.Side == "sell" {
			return nil, newAPIError(http.StatusBadRequest, "Market not active")
		}
		return nil, ErrMarketNotTradable
	}

	match, err := s.matches.GetMatchByID(ctx, req.MatchID)
	if err != nil || match == nil {
		return nil, ErrMatchNotFound
	}
	if !isMatchTradable(match) {
		return nil, ErrMatchNotTradable
	}

	pricingInput := markets.PricingInputFromMatch(*match)
	if req.PricingSnapshot != nil {
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
	}, nil
}

func (s *Service) CreateOrder(ctx context.Context, userID primitive.ObjectID, req CreateOrderRequest) (*Order, error) {
	req = normalizeCreateRequest(req)

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
	if !s.markets.IsTradable(market) {
		// Exit path uses the explicit error contract; buy path is unchanged.
		if req.Side == "sell" {
			return nil, newAPIError(http.StatusBadRequest, "Market not active")
		}
		return nil, ErrMarketNotTradable
	}

	// Serialize mutations for this (user, market, strike) so concurrent sells
	// cannot oversell the same position.
	unlock := s.lockFor(userID, market.ID.Hex(), req.Strike)
	defer unlock()

	match, err := s.matches.GetMatchByID(ctx, req.MatchID)
	if err != nil || match == nil {
		return nil, ErrMatchNotFound
	}
	if !isMatchTradable(match) {
		return nil, ErrMatchNotTradable
	}

	pricingInput := markets.PricingInputFromMatch(*match)
	if req.PricingSnapshot != nil {
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
	}

	var created *Order
	err = s.repo.DoTx(ctx, func(txCtx context.Context) error {
		var txErr error
		created, txErr = s.repo.Create(txCtx, order)
		if txErr != nil {
			return txErr
		}

		if plan.ReservedAmount > 0 {
			_, txErr = s.wallets.ReserveOrderMargin(txCtx, userID, plan.ReservedAmount, created.ID.Hex(), fmt.Sprintf("Reserve margin for order %s", created.ID.Hex()))
			if txErr != nil {
				return txErr
			}
		}
		return nil
	})

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

	filled, err := s.applyFill(ctx, userID, created, fillPrice, created.RemainingQuantity)
	if err != nil {
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
	if snapshot.BallsLeft > 120 {
		snapshot.BallsLeft = 120
	}
	if snapshot.BallsBowled < 0 {
		snapshot.BallsBowled = 0
	}
	if snapshot.BallsBowled > 120 {
		snapshot.BallsBowled = 120
	}
	return snapshot
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
	if existing.Status != StatusOpen && existing.Status != StatusPartiallyFilled {
		return nil, nil
	}

	var cancelled *Order
	err = s.repo.DoTx(ctx, func(txCtx context.Context) error {
		if remainingReserve := existing.RemainingReservedAmount(); remainingReserve > 0 {
			_, txErr := s.wallets.ReleaseOrderMargin(txCtx, userID, remainingReserve, existing.ID.Hex(), fmt.Sprintf("Release margin for cancelled order %s", existing.ID.Hex()))
			if txErr != nil {
				return txErr
			}
		}

		var txErr error
		cancelled, txErr = s.repo.Cancel(txCtx, id, userID)
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

	return s.CreateOrder(ctx, userID, CreateOrderRequest{
		MatchID:        target.MatchID,
		MarketID:       target.MarketID,
		Strike:         target.Strike,
		Side:           side,
		Type:           orderType,
		PositionEffect: PositionEffectClose,
		Quantity:       quantity,
		Price:          price,
	})
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

		order, err := s.CreateOrder(ctx, userID, CreateOrderRequest{
			MatchID:        target.MatchID,
			MarketID:       target.MarketID,
			Strike:         target.Strike,
			Side:           side,
			Type:           OrderTypeMarket,
			PositionEffect: PositionEffectClose,
			Quantity:       quantity,
		})
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
	if fillQty <= 0 || fillQty > order.RemainingQuantity {
		return order, nil
	}

	before := s.executions.PositionSummary(ctx, userID, order.MatchID, order.MarketID, order.Strike)
	closeLongQty := 0
	openShortQty := 0
	coverShortQty := 0
	openLongQty := 0
	switch order.Side {
	case "sell":
		if before.NetLots > 0 {
			closeLongQty = minInt(fillQty, before.NetLots)
		}
		openShortQty = fillQty - closeLongQty
	case "buy":
		if before.NetLots < 0 {
			coverShortQty = minInt(fillQty, -before.NetLots)
		}
		openLongQty = fillQty - coverShortQty
	}

	fillNotional := round2(fillPrice * float64(fillQty))

	var updated *Order
	err := s.repo.DoTx(ctx, func(txCtx context.Context) error {
		switch order.Side {
		case "buy":
			reserveRelease := order.ReservedReleaseForQuantity(openLongQty)
			if _, txErr := s.wallets.SettleBuyFill(txCtx, userID, fillNotional, reserveRelease, order.ID.Hex(), fmt.Sprintf("Buy fill for order %s", order.ID.Hex())); txErr != nil {
				return txErr
			}
			if coverShortQty > 0 {
				release := shortCollateralRelease(before.AvgSellPrice, fillPrice, coverShortQty)
				if release > 0 {
					if _, txErr := s.wallets.ReleaseOrderMargin(txCtx, userID, release, order.ID.Hex(), fmt.Sprintf("Release short collateral for cover order %s", order.ID.Hex())); txErr != nil {
						return txErr
					}
				}
			}
		case "sell":
			if closeLongQty > 0 {
				proceeds := round2(fillPrice * float64(closeLongQty))
				if _, txErr := s.wallets.SettleSellFill(txCtx, userID, proceeds, order.ID.Hex(), fmt.Sprintf("Sell fill for order %s", order.ID.Hex())); txErr != nil {
					return txErr
				}
			}
			if openShortQty > 0 {
				if topUp := shortInitialMarginTopUp(*order, fillPrice, openShortQty); topUp > 0 {
					if _, txErr := s.wallets.ReserveOrderMargin(txCtx, userID, topUp, order.ID.Hex(), fmt.Sprintf("Top up short margin for order %s", order.ID.Hex())); txErr != nil {
						return txErr
					}
				}
				proceeds := round2(fillPrice * float64(openShortQty))
				if _, txErr := s.wallets.SettleShortOpenFill(txCtx, userID, proceeds, order.ID.Hex(), fmt.Sprintf("Short sale proceeds for order %s", order.ID.Hex())); txErr != nil {
					return txErr
				}
			}
		}

		execution := executions.Execution{
			UserID:          userID,
			OrderID:         order.ID,
			MatchID:         order.MatchID,
			MarketID:        order.MarketID,
			Strike:          order.Strike,
			Side:            order.Side,
			Price:           fillPrice,
			Quantity:        fillQty,
			LiquiditySource: executions.LiquiditySystemMarketMaker,
		}
		createdExec, txErr := s.executions.Create(txCtx, execution)
		if txErr != nil {
			return txErr
		}
		if s.positionWriter != nil && createdExec != nil {
			if txErr := s.positionWriter.ApplyExecution(txCtx, *createdExec); txErr != nil {
				return txErr
			}
		}

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

		updated, txErr = s.repo.UpdateFill(txCtx, order.ID, FillUpdate{
			FilledQuantity:    newFilled,
			RemainingQuantity: newRemaining,
			AverageFillPrice:  avgFill,
			Status:            status,
		})
		return txErr
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
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

func isMatchTradable(match *matches.Match) bool {
	if match == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(match.Status)) {
	case "live", "innings_break":
		return true
	default:
		return false
	}
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
