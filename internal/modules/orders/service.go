package orders

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
)

var (
	ErrMarketNotFound        = errors.New("market not found")
	ErrMatchNotFound         = errors.New("match not found")
	ErrInvalidSide           = errors.New("invalid side, must be 'buy' or 'sell'")
	ErrInvalidQuantity       = errors.New("quantity must be positive")
	ErrInvalidPrice          = errors.New("price must be positive")
	ErrInvalidStrike         = errors.New("strike must be positive")
	ErrMarketNotTradable     = errors.New("market is not open for trading")
	ErrMatchNotTradable      = errors.New("match is not live for trading")
	ErrInsufficientBalance   = errors.New("insufficient available wallet balance")
	ErrInsufficientPosition  = errors.New("insufficient position to sell")
	ErrStrikeNotFound        = errors.New("strike not found in option chain")
)

type MatchReader interface {
	GetMatchByID(ctx context.Context, id string) (*matches.Match, error)
}

type MarketReader interface {
	GetMarketByID(ctx context.Context, id string) (*markets.Market, error)
	StrikeQuote(input markets.PriceCalculationInput, strike float64) (bid, ask float64, ok bool)
	IsTradable(market *markets.Market) bool
}

type WalletPort interface {
	ReserveOrderMargin(ctx context.Context, userID primitive.ObjectID, amount float64, orderID, description string) (*wallet.AdjustmentResult, error)
	ReleaseOrderMargin(ctx context.Context, userID primitive.ObjectID, amount float64, orderID, description string) (*wallet.AdjustmentResult, error)
	SettleBuyFill(ctx context.Context, userID primitive.ObjectID, fillCost, reserveRelease float64, orderID, description string) (*wallet.AdjustmentResult, error)
	SettleSellFill(ctx context.Context, userID primitive.ObjectID, proceeds float64, orderID, description string) (*wallet.AdjustmentResult, error)
}

type ExecutionWriter interface {
	Create(ctx context.Context, exec executions.Execution) (*executions.Execution, error)
	OpenLongQty(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) int
}

type Service struct {
	repo        Repository
	markets     MarketReader
	matches     MatchReader
	wallets     WalletPort
	executions  ExecutionWriter
}

func NewService(
	repo Repository,
	markets MarketReader,
	matches MatchReader,
	wallets WalletPort,
	executions ExecutionWriter,
) *Service {
	return &Service{
		repo:       repo,
		markets:    markets,
		matches:    matches,
		wallets:    wallets,
		executions: executions,
	}
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
	bid, ask, ok := s.markets.StrikeQuote(pricingInput, req.Strike)
	if !ok {
		return nil, ErrStrikeNotFound
	}

	if req.Side == "sell" {
		openQty := s.executions.OpenLongQty(ctx, userID, req.MatchID, market.ID.Hex(), req.Strike)
		if openQty < req.Quantity {
			return nil, ErrInsufficientPosition
		}
	}

	reserveAmount := round2(req.Price * float64(req.Quantity))
	order := Order{
		ClientOrderID:     req.ClientOrderID,
		UserID:            userID,
		MatchID:           req.MatchID,
		MarketID:          market.ID.Hex(),
		Strike:            req.Strike,
		Side:              req.Side,
		Type:              req.Type,
		Quantity:          req.Quantity,
		Price:             req.Price,
		FilledQuantity:    0,
		RemainingQuantity: req.Quantity,
		Status:            StatusOpen,
	}

	created, err := s.repo.Create(ctx, order)
	if err != nil {
		return nil, err
	}

	if req.Side == "buy" {
		_, err = s.wallets.ReserveOrderMargin(ctx, userID, reserveAmount, created.ID.Hex(), fmt.Sprintf("Reserve margin for order %s", created.ID.Hex()))
		if err != nil {
			_, _ = s.repo.Cancel(ctx, created.ID, userID)
			if errors.Is(err, wallet.ErrInsufficientFunds) {
				return nil, ErrInsufficientBalance
			}
			return nil, err
		}
	}

	fillPrice, shouldFill := matchLimitOrder(req.Side, req.Price, bid, ask)
	if !shouldFill {
		return created, nil
	}

	return s.applyFill(ctx, userID, created, fillPrice, created.RemainingQuantity)
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

	if existing.Side == "buy" && existing.ReservedAmount() > 0 {
		_, err := s.wallets.ReleaseOrderMargin(ctx, userID, existing.ReservedAmount(), existing.ID.Hex(), fmt.Sprintf("Release margin for cancelled order %s", existing.ID.Hex()))
		if err != nil {
			return nil, err
		}
	}

	return s.repo.Cancel(ctx, id, userID)
}

func (s *Service) applyFill(ctx context.Context, userID primitive.ObjectID, order *Order, fillPrice float64, fillQty int) (*Order, error) {
	if fillQty <= 0 || fillQty > order.RemainingQuantity {
		return order, nil
	}

	fillCost := round2(fillPrice * float64(fillQty))
	reserveRelease := round2(order.Price * float64(fillQty))

	switch order.Side {
	case "buy":
		if _, err := s.wallets.SettleBuyFill(ctx, userID, fillCost, reserveRelease, order.ID.Hex(), fmt.Sprintf("Buy fill for order %s", order.ID.Hex())); err != nil {
			return nil, err
		}
	case "sell":
		if _, err := s.wallets.SettleSellFill(ctx, userID, fillCost, order.ID.Hex(), fmt.Sprintf("Sell fill for order %s", order.ID.Hex())); err != nil {
			return nil, err
		}
	}

	if _, err := s.executions.Create(ctx, executions.Execution{
		UserID:          userID,
		OrderID:         order.ID,
		MatchID:         order.MatchID,
		MarketID:        order.MarketID,
		Strike:          order.Strike,
		Side:            order.Side,
		Price:           fillPrice,
		Quantity:        fillQty,
		LiquiditySource: executions.LiquiditySystemMarketMaker,
	}); err != nil {
		return nil, err
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

	updated, err := s.repo.UpdateFill(ctx, order.ID, FillUpdate{
		FilledQuantity:    newFilled,
		RemainingQuantity: newRemaining,
		AverageFillPrice:  avgFill,
		Status:            status,
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
	if req.Price <= 0 {
		return ErrInvalidPrice
	}
	if req.Strike <= 0 {
		return ErrInvalidStrike
	}
	if req.Type != OrderTypeLimit {
		return errors.New("only LIMIT orders are supported")
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
	switch side {
	case "buy":
		if ask > 0 && limitPrice >= ask {
			return ask, true
		}
	case "sell":
		if bid > 0 && limitPrice <= bid {
			return bid, true
		}
	}
	return 0, false
}
