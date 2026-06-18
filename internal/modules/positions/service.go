package positions

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/orders"
)

var errInvalidUserID = errors.New("invalid userId")

type ExecutionReader interface {
	List(ctx context.Context, filter executions.Filter) []executions.Execution
}

type MarketReader interface {
	GetMarketByID(ctx context.Context, id string) (*markets.Market, error)
}

type Service struct {
	executions ExecutionReader
	markets    MarketReader
}

func NewService(executions ExecutionReader, markets MarketReader) *Service {
	return &Service{executions: executions, markets: markets}
}

func (s *Service) GetUserOpenPositions(ctx context.Context, userID primitive.ObjectID) ([]Position, error) {
	all, err := s.computeForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return filterOpen(all), nil
}

func (s *Service) GetUserClosedPositions(ctx context.Context, userID primitive.ObjectID) ([]Position, error) {
	all, err := s.computeForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return filterClosed(all), nil
}

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

func (s *Service) ListAdminPositions(ctx context.Context, filter PositionFilter) ([]Position, error) {
	var userIDFilter primitive.ObjectID
	if filter.UserID != "" {
		parsed, err := primitive.ObjectIDFromHex(filter.UserID)
		if err != nil {
			return nil, errInvalidUserID
		}
		userIDFilter = parsed
	}

	execFilter := executions.Filter{Limit: 1000}
	allExecs := s.executions.List(ctx, execFilter)
	positions := s.aggregate(ctx, allExecs)

	if !userIDFilter.IsZero() {
		filtered := positions[:0]
		for _, p := range positions {
			if p.UserID == userIDFilter {
				filtered = append(filtered, p)
			}
		}
		positions = filtered
	}

	return applyStaticFilters(positions, filter), nil
}

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

// PositionFor returns a snapshot for a user's (match, market, strike) position,
// including closed positions (lots == 0). Implements orders.PositionView so the
// orders service can broadcast position updates after a sell fill.
func (s *Service) PositionFor(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) (orders.PositionSnapshot, bool) {
	all, err := s.computeForUser(ctx, userID)
	if err != nil {
		return orders.PositionSnapshot{}, false
	}
	for i := range all {
		p := all[i]
		if p.MatchID == matchID && p.MarketID == marketID && p.Strike == strike {
			return toSnapshot(p), true
		}
	}
	return orders.PositionSnapshot{}, false
}

// ResolveCloseTarget resolves a derived position id to its snapshot so the
// orders service can build an exit order. Implements orders.PositionView.
func (s *Service) ResolveCloseTarget(ctx context.Context, userID primitive.ObjectID, positionID string) (orders.PositionSnapshot, bool) {
	p, err := s.GetUserPosition(ctx, userID, positionID)
	if err != nil || p == nil {
		return orders.PositionSnapshot{}, false
	}
	return toSnapshot(*p), true
}

func toSnapshot(p Position) orders.PositionSnapshot {
	return orders.PositionSnapshot{
		MatchID:     p.MatchID,
		MarketID:    p.MarketID,
		Strike:      p.Strike,
		Lots:        p.Lots,
		BuyPrice:    p.BuyPrice,
		LTP:         p.LTP,
		PnL:         p.PnL,
		RealizedPnL: p.RealizedPnL,
		Status:      p.Status,
	}
}

func (s *Service) computeForUser(ctx context.Context, userID primitive.ObjectID) ([]Position, error) {
	execs := s.executions.List(ctx, executions.Filter{UserID: userID, Limit: 1000})
	return s.aggregate(ctx, execs), nil
}

func (s *Service) aggregate(ctx context.Context, fills []executions.Execution) []Position {
	type key struct {
		userID   primitive.ObjectID
		matchID  string
		marketID string
		strike   float64
	}

	groups := make(map[key]*aggregateBucket)
	order := make([]key, 0)

	for _, fill := range fills {
		k := key{userID: fill.UserID, matchID: fill.MatchID, marketID: fill.MarketID, strike: fill.Strike}
		b, ok := groups[k]
		if !ok {
			b = &aggregateBucket{firstSeen: fill.CreatedAt}
			groups[k] = b
			order = append(order, k)
		}
		b.add(fill)
		if fill.CreatedAt.Before(b.firstSeen) {
			b.firstSeen = fill.CreatedAt
		}
	}

	out := make([]Position, 0, len(order))
	for _, k := range order {
		b := groups[k]
		p := b.toPosition()
		p.ID = derivePositionID(k.userID, k.matchID, k.marketID, k.strike, b.firstSeen)
		p.UserID = k.userID
		p.MatchID = k.matchID
		p.MarketID = k.marketID
		p.Strike = k.strike
		p.CreatedAt = b.firstSeen
		p.UpdatedAt = b.lastUpdated

		if m, err := s.markets.GetMarketByID(ctx, k.marketID); err == nil && m != nil {
			p.LTP = m.LTP
		}
		p.PnL = computePnL(p, b.matchedQty())
		p.RealizedPnL = computeRealized(b)
		out = append(out, p)
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

type aggregateBucket struct {
	buyQty       int
	buyNotional  float64
	sellQty      int
	sellNotional float64
	firstSeen    time.Time
	lastUpdated  time.Time
}

func (b *aggregateBucket) add(fill executions.Execution) {
	switch fill.Side {
	case "buy":
		b.buyQty += fill.Quantity
		b.buyNotional += fill.Price * float64(fill.Quantity)
	case "sell":
		b.sellQty += fill.Quantity
		b.sellNotional += fill.Price * float64(fill.Quantity)
	}
	if fill.CreatedAt.After(b.lastUpdated) {
		b.lastUpdated = fill.CreatedAt
	}
	if b.firstSeen.IsZero() || fill.CreatedAt.Before(b.firstSeen) {
		b.firstSeen = fill.CreatedAt
	}
}

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

// computeRealized returns the realized PnL on the matched (closed) slice of a
// position: (avgSell - avgBuy) * min(buyQty, sellQty).
func computeRealized(b *aggregateBucket) float64 {
	matched := b.matchedQty()
	if matched == 0 || b.buyQty == 0 || b.sellQty == 0 {
		return 0
	}
	avgBuy := b.buyNotional / float64(b.buyQty)
	avgSell := b.sellNotional / float64(b.sellQty)
	return round2((avgSell - avgBuy) * float64(matched))
}

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

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

func derivePositionID(userID primitive.ObjectID, matchID, marketID string, strike float64, firstSeen time.Time) string {
	h := sha1.New()
	h.Write([]byte(userID.Hex()))
	h.Write([]byte("|"))
	h.Write([]byte(matchID))
	h.Write([]byte("|"))
	h.Write([]byte(marketID))
	h.Write([]byte("|"))
	h.Write([]byte(fmt.Sprintf("%g", strike)))
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
