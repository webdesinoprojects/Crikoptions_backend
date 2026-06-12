package positions

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/orders"
)

var errInvalidUserID = errors.New("invalid userId")

// OrderReader is the slice of the orders repository that positions needs.
// Defined here (not imported) so positions does not depend on the orders
// package internals.
type OrderReader interface {
	GetByUserID(ctx context.Context, userID primitive.ObjectID, status, matchID string) []orders.Order
	List(ctx context.Context, filter orders.OrderFilter) []orders.Order
}

// MarketReader is the slice of the markets service that positions needs.
type MarketReader interface {
	GetMarketByID(ctx context.Context, id string) (*markets.Market, error)
}

type Service struct {
	orders   OrderReader
	markets MarketReader
}

func NewService(orders OrderReader, markets MarketReader) *Service {
	return &Service{orders: orders, markets: markets}
}

// GetUserOpenPositions returns the user's currently-open positions (net
// quantity != 0). It is the implementation of PRD API #13.
func (s *Service) GetUserOpenPositions(ctx context.Context, userID primitive.ObjectID) ([]Position, error) {
	all, err := s.computeForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return filterOpen(all), nil
}

// GetUserClosedPositions returns the user's positions that have been fully
// squared off (net quantity == 0). It is the implementation of PRD API #14.
func (s *Service) GetUserClosedPositions(ctx context.Context, userID primitive.ObjectID) ([]Position, error) {
	all, err := s.computeForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return filterClosed(all), nil
}

// GetUserPosition returns a single position by its derived ID. It is the
// implementation of PRD API #15. Returns nil, nil if not found.
func (s *Service) GetUserPosition(ctx context.Context, userID primitive.ObjectID, positionID string) (*Position, error) {
	all, err := s.computeForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == positionID {
			return &all[i], nil
		}
	}
	return nil, nil
}

// ListAdminPositions returns positions across all users, filtered by the
// optional filter struct. userID may be a hex ObjectID; an empty string means
// "all users". It is the implementation of PRD API #28.
func (s *Service) ListAdminPositions(ctx context.Context, filter PositionFilter) ([]Position, error) {
	var userIDFilter primitive.ObjectID
	if filter.UserID != "" {
		parsed, err := primitive.ObjectIDFromHex(filter.UserID)
		if err != nil {
			return nil, errInvalidUserID
		}
		userIDFilter = parsed
	}

	// Pull all executed orders in one go; positions are aggregated in-memory.
	orderFilter := orders.OrderFilter{Status: "executed"}
	allOrders := s.orders.List(ctx, orderFilter)

	positions := s.aggregate(ctx, allOrders)

	// Apply userID filter.
	if !userIDFilter.IsZero() {
		filtered := positions[:0]
		for _, p := range positions {
			if p.UserID == userIDFilter {
				filtered = append(filtered, p)
			}
		}
		positions = filtered
	}

	positions = applyStaticFilters(positions, filter)
	return positions, nil
}

// GetAdminPosition returns any position by derived ID. It is the
// implementation of PRD API #28's per-id detail endpoint.
func (s *Service) GetAdminPosition(ctx context.Context, positionID string) (*Position, error) {
	all, err := s.ListAdminPositions(ctx, PositionFilter{})
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == positionID {
			return &all[i], nil
		}
	}
	return nil, nil
}

func (s *Service) computeForUser(ctx context.Context, userID primitive.ObjectID) ([]Position, error) {
	userOrders := s.orders.GetByUserID(ctx, userID, "executed", "")
	return s.aggregate(ctx, userOrders), nil
}

// aggregate groups the supplied executed orders by (userId, matchId, marketId)
// and returns one Position per group. LTP and PnL are computed at call time.
func (s *Service) aggregate(ctx context.Context, executedOrders []orders.Order) []Position {
	type key struct {
		userID   primitive.ObjectID
		matchID  string
		marketID string
	}

	groups := make(map[key]*aggregateBucket)
	order := make([]key, 0)

	for _, o := range executedOrders {
		k := key{userID: o.UserID, matchID: o.MatchID, marketID: o.MarketID}
		b, ok := groups[k]
		if !ok {
			b = &aggregateBucket{firstSeen: o.CreatedAt}
			groups[k] = b
			order = append(order, k)
		}
		b.add(o)
		if o.CreatedAt.Before(b.firstSeen) {
			b.firstSeen = o.CreatedAt
		}
	}

	out := make([]Position, 0, len(order))
	for _, k := range order {
		b := groups[k]
		p := b.toPosition()
		p.ID = derivePositionID(k.userID, k.matchID, k.marketID, b.firstSeen)
		p.UserID = k.userID
		p.MatchID = k.matchID
		p.MarketID = k.marketID
		p.CreatedAt = b.firstSeen
		p.UpdatedAt = b.lastUpdated

		// Pull LTP from the market. If the market is missing, leave LTP = 0.
		if m, err := s.markets.GetMarketByID(ctx, k.marketID); err == nil && m != nil {
			p.LTP = m.LTP
		}
		p.PnL = computePnL(p, b.matchedQty())
		out = append(out, p)
	}

	// Stable order: most recently updated first.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

type aggregateBucket struct {
	buyQty       int
	buyNotional  float64 // sum(price * qty) for buys
	sellQty      int
	sellNotional float64 // sum(price * qty) for sells
	firstSeen    time.Time
	lastUpdated  time.Time
}

func (b *aggregateBucket) add(o orders.Order) {
	switch o.Side {
	case "buy":
		b.buyQty += o.Quantity
		b.buyNotional += o.Price * float64(o.Quantity)
	case "sell":
		b.sellQty += o.Quantity
		b.sellNotional += o.Price * float64(o.Quantity)
	}
	if o.UpdatedAt.After(b.lastUpdated) {
		b.lastUpdated = o.UpdatedAt
	}
	if b.firstSeen.IsZero() || o.CreatedAt.Before(b.firstSeen) {
		b.firstSeen = o.CreatedAt
	}
}

// matchedQty returns the number of lots that have been squared off
// (min of buy/sell quantities).
func (b *aggregateBucket) matchedQty() int {
	if b.buyQty < b.sellQty {
		return b.buyQty
	}
	return b.sellQty
}

func (b *aggregateBucket) toPosition() Position {
	net := b.buyQty - b.sellQty
	status := "open"
	if net == 0 {
		status = "closed"
	}
	avgBuy := 0.0
	if b.buyQty > 0 {
		avgBuy = b.buyNotional / float64(b.buyQty)
	}
	avgSell := 0.0
	if b.sellQty > 0 {
		avgSell = b.sellNotional / float64(b.sellQty)
	}
	return Position{
		Status:    status,
		Lots:      net,
		BuyPrice:  round2(avgBuy),
		SellPrice: round2(avgSell),
	}
}

// computePnL returns the PnL for a position. The "lots" argument is the
// absolute value of the open portion of the position; "matched" is the
// already-squared portion.
//
// For an open long (Lots > 0): unrealized = (LTP - BuyPrice) * |Lots|
// For an open short (Lots < 0): unrealized = (SellPrice - LTP) * |Lots|
// For a closed position: realized = (SellPrice - BuyPrice) * matched
func computePnL(p Position, matched int) float64 {
	absLots := p.Lots
	if absLots < 0 {
		absLots = -absLots
	}
	switch {
	case p.Status == "open" && p.Lots > 0 && p.BuyPrice > 0:
		return round2((p.LTP - p.BuyPrice) * float64(absLots))
	case p.Status == "open" && p.Lots < 0 && p.SellPrice > 0:
		return round2((p.SellPrice - p.LTP) * float64(absLots))
	case p.Status == "closed" && p.BuyPrice > 0 && p.SellPrice > 0:
		return round2((p.SellPrice - p.BuyPrice) * float64(matched))
	}
	return 0
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func minAbs(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func round2(f float64) float64 {
	// 2-decimal rounding without importing math (keeps the package lean).
	return float64(int64(f*100+0.5)) / 100
}

// derivePositionID returns a stable synthetic ID for a position. Using
// (userId|matchId|marketId|firstSeen) gives us a stable reference that does
// not change as more orders are added to the same position, and is short
// enough to be human-readable in API responses.
func derivePositionID(userID primitive.ObjectID, matchID, marketID string, firstSeen time.Time) string {
	h := sha1.New()
	h.Write([]byte(userID.Hex()))
	h.Write([]byte("|"))
	h.Write([]byte(matchID))
	h.Write([]byte("|"))
	h.Write([]byte(marketID))
	h.Write([]byte("|"))
	h.Write([]byte(firstSeen.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(h.Sum(nil))[:24]
}

func filterOpen(in []Position) []Position {
	out := make([]Position, 0, len(in))
	for _, p := range in {
		if p.Status == "open" {
			out = append(out, p)
		}
	}
	return out
}

func filterClosed(in []Position) []Position {
	out := make([]Position, 0, len(in))
	for _, p := range in {
		if p.Status == "closed" {
			out = append(out, p)
		}
	}
	return out
}

func applyStaticFilters(in []Position, f PositionFilter) []Position {
	out := make([]Position, 0, len(in))
	for _, p := range in {
		if f.MatchID != "" && p.MatchID != f.MatchID {
			continue
		}
		if f.MarketID != "" && p.MarketID != f.MarketID {
			continue
		}
		if f.Status != "" && p.Status != f.Status {
			continue
		}
		out = append(out, p)
	}
	return out
}
