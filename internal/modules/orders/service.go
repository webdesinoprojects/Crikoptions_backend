package orders

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

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
}

type ExecutionWriter interface {
	Create(ctx context.Context, exec executions.Execution) (*executions.Execution, error)
	OpenLongQty(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) int
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

	if req.Side == "sell" {
		openQty := s.executions.OpenLongQty(ctx, userID, req.MatchID, market.ID.Hex(), req.Strike)
		if openQty <= 0 {
			return nil, newAPIError(http.StatusBadRequest, fmt.Sprintf("No open position for strike %s", formatStrike(req.Strike)))
		}
		if req.Quantity > openQty {
			return nil, newAPIError(http.StatusBadRequest, fmt.Sprintf("Cannot sell %d lots; only %d open", req.Quantity, openQty))
		}
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

	account, err := s.wallets.GetWallet(ctx, userID)
	if err != nil {
		return nil, err
	}

	notional := round2(orderPrice * float64(req.Quantity))
	marginRequired := 0.0
	if req.Side == "buy" {
		marginRequired = notional
	}
	sufficientBalance := req.Side != "buy" || account.AvailableBalance >= marginRequired

	message := "Preview available"
	if !sufficientBalance {
		message = "Insufficient available wallet balance"
	} else if shouldFill {
		message = "Order is marketable at current quote"
	} else {
		message = "Limit order would rest on the book"
	}

	return &OrderPreviewResponse{
		MatchID:           req.MatchID,
		MarketID:          market.ID.Hex(),
		Strike:            req.Strike,
		Side:              req.Side,
		Type:              req.Type,
		Quantity:          req.Quantity,
		RequestedPrice:    round2(req.Price),
		OrderPrice:        orderPrice,
		ExecutablePrice:   round2(fillPrice),
		Bid:               bid,
		Ask:               ask,
		Notional:          notional,
		MarginRequired:    marginRequired,
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

	if req.Side == "sell" {
		openQty := s.executions.OpenLongQty(ctx, userID, req.MatchID, market.ID.Hex(), req.Strike)
		if openQty <= 0 {
			return nil, newAPIError(http.StatusBadRequest, fmt.Sprintf("No open position for strike %s", formatStrike(req.Strike)))
		}
		if req.Quantity > openQty {
			return nil, newAPIError(http.StatusBadRequest, fmt.Sprintf("Cannot sell %d lots; only %d open", req.Quantity, openQty))
		}
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

	reserveAmount := round2(orderPrice * float64(req.Quantity))
	order := Order{
		ClientOrderID:     req.ClientOrderID,
		UserID:            userID,
		MatchID:           req.MatchID,
		MarketID:          market.ID.Hex(),
		Strike:            req.Strike,
		Side:              req.Side,
		Type:              req.Type,
		Quantity:          req.Quantity,
		Price:             orderPrice,
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

		if req.Side == "buy" {
			_, txErr = s.wallets.ReserveOrderMargin(txCtx, userID, reserveAmount, created.ID.Hex(), fmt.Sprintf("Reserve margin for order %s", created.ID.Hex()))
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
		s.broadcastSell(ctx, userID, created)
		return created, nil
	}

	filled, err := s.applyFill(ctx, userID, created, fillPrice, created.RemainingQuantity)
	if err != nil {
		return nil, err
	}
	s.broadcastSell(ctx, userID, filled)
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
		if existing.Side == "buy" && existing.ReservedAmount() > 0 {
			_, txErr := s.wallets.ReleaseOrderMargin(txCtx, userID, existing.ReservedAmount(), existing.ID.Hex(), fmt.Sprintf("Release margin for cancelled order %s", existing.ID.Hex()))
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
	if cancelled != nil && cancelled.Side == "sell" {
		// Position is unchanged on cancel, but the order state changed.
		s.broadcastSell(ctx, userID, cancelled)
	}
	return cancelled, nil
}

// ClosePosition resolves a derived position by id and submits a SELL order to
// exit it, reusing the full validation + matching of the sell path. quantity
// defaults to the full open lots when zero.
func (s *Service) ClosePosition(ctx context.Context, userID primitive.ObjectID, positionID, orderType string, quantity int, price float64) (*Order, error) {
	if s.positions == nil {
		return nil, newAPIError(http.StatusBadRequest, "Position lookup is unavailable")
	}

	target, ok := s.positions.ResolveCloseTarget(ctx, userID, positionID)
	if !ok || target.Lots <= 0 {
		return nil, newAPIError(http.StatusBadRequest, "No open position to close")
	}

	if quantity <= 0 {
		quantity = target.Lots
	}

	orderType = strings.ToUpper(strings.TrimSpace(orderType))
	if orderType == "" {
		orderType = OrderTypeMarket
	}

	return s.CreateOrder(ctx, userID, CreateOrderRequest{
		MatchID:  target.MatchID,
		MarketID: target.MarketID,
		Strike:   target.Strike,
		Side:     "sell",
		Type:     orderType,
		Quantity: quantity,
		Price:    price,
	})
}

// CloseAllPositions submits MARKET sell-to-close orders for every open position
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
		if target.Lots <= 0 || target.Status == "closed" {
			continue
		}
		result.Requested++

		order, err := s.CreateOrder(ctx, userID, CreateOrderRequest{
			MatchID:  target.MatchID,
			MarketID: target.MarketID,
			Strike:   target.Strike,
			Side:     "sell",
			Type:     OrderTypeMarket,
			Quantity: target.Lots,
		})
		if err != nil {
			result.Failed++
			result.Failures = append(result.Failures, CloseAllPositionFailure{
				MatchID:  target.MatchID,
				MarketID: target.MarketID,
				Strike:   target.Strike,
				Quantity: target.Lots,
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

// broadcastSell streams order + position updates for a sell order's owner.
func (s *Service) broadcastSell(ctx context.Context, userID primitive.ObjectID, order *Order) {
	if s.publisher == nil || order == nil || order.Side != "sell" {
		return
	}

	s.publisher.Publish(realtime.UserOrdersTopic(userID.Hex()), map[string]any{
		"orderId":           order.ID.Hex(),
		"marketId":          order.MarketID,
		"strike":            order.Strike,
		"side":              "SELL",
		"status":            order.Status,
		"filledQuantity":    order.FilledQuantity,
		"remainingQuantity": order.RemainingQuantity,
		"averageFillPrice":  order.AverageFillPrice,
	})

	if s.positions == nil {
		return
	}
	snap, ok := s.positions.PositionFor(ctx, userID, order.MatchID, order.MarketID, order.Strike)
	if !ok {
		return
	}
	s.publisher.Publish(realtime.UserPositionsTopic(userID.Hex()), map[string]any{
		"marketId":    snap.MarketID,
		"strike":      snap.Strike,
		"lots":        snap.Lots,
		"buyPrice":    snap.BuyPrice,
		"ltp":         snap.LTP,
		"pnl":         snap.PnL,
		"realizedPnl": snap.RealizedPnL,
		"status":      snap.Status,
	})
}

func (s *Service) applyFill(ctx context.Context, userID primitive.ObjectID, order *Order, fillPrice float64, fillQty int) (*Order, error) {
	if fillQty <= 0 || fillQty > order.RemainingQuantity {
		return order, nil
	}

	fillCost := round2(fillPrice * float64(fillQty))
	reserveRelease := round2(order.Price * float64(fillQty))

	var updated *Order
	err := s.repo.DoTx(ctx, func(txCtx context.Context) error {
		switch order.Side {
		case "buy":
			if _, txErr := s.wallets.SettleBuyFill(txCtx, userID, fillCost, reserveRelease, order.ID.Hex(), fmt.Sprintf("Buy fill for order %s", order.ID.Hex())); txErr != nil {
				return txErr
			}
		case "sell":
			if _, txErr := s.wallets.SettleSellFill(txCtx, userID, fillCost, order.ID.Hex(), fmt.Sprintf("Sell fill for order %s", order.ID.Hex())); txErr != nil {
				return txErr
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
