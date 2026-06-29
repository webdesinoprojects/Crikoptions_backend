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

type BatchMarketReader interface {
	GetMarketsByIDs(ctx context.Context, ids []string) (map[string]*markets.Market, error)
}

type Service struct {
	executions  ExecutionReader
	markets     MarketReader
	projections ProjectionRepository
}

func NewService(executions ExecutionReader, markets MarketReader) *Service {
	return &Service{executions: executions, markets: markets}
}

func NewServiceWithProjection(executions ExecutionReader, markets MarketReader, projections ProjectionRepository) *Service {
	return &Service{executions: executions, markets: markets, projections: projections}
}

func (s *Service) GetUserOpenPositions(ctx context.Context, userID primitive.ObjectID) ([]Position, error) {
	return s.ListUserPositions(ctx, userID, PositionFilter{Status: "open"})
}

func (s *Service) GetUserClosedPositions(ctx context.Context, userID primitive.ObjectID) ([]Position, error) {
	return s.ListUserPositions(ctx, userID, PositionFilter{Status: "closed"})
}

func (s *Service) ListUserPositions(ctx context.Context, userID primitive.ObjectID, filter PositionFilter) ([]Position, error) {
	if s.projections != nil {
		projected, found, err := s.projectedUserPositions(ctx, userID, filter)
		if err != nil {
			return nil, err
		}
		if found {
			return projected, nil
		}
	}

	all, err := s.computeForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	filter.UserID = ""
	return applyStaticFilters(all, filter), nil
}

func (s *Service) GetUserPosition(ctx context.Context, userID primitive.ObjectID, positionID string) (*Position, error) {
	if s.projections != nil {
		projection, err := s.projections.GetByID(ctx, userID, positionID)
		if err != nil {
			return nil, err
		}
		if projection != nil {
			positions, err := s.positionsFromProjections(ctx, []PositionProjection{*projection})
			if err != nil {
				return nil, err
			}
			if len(positions) == 1 {
				return &positions[0], nil
			}
		}
		if found, err := s.hasProjectionRows(ctx, userID); err != nil {
			return nil, err
		} else if found {
			return nil, nil
		}
	}

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

	if s.projections != nil {
		projected, err := s.projectedAdminPositions(ctx, userIDFilter, filter)
		if err != nil {
			return nil, err
		}
		if len(projected) > 0 {
			return projected, nil
		}
		if found, err := s.hasProjectionRows(ctx, userIDFilter); err != nil {
			return nil, err
		} else if found {
			return projected, nil
		}
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
	if s.projections != nil {
		projection, err := s.projections.GetOpenByKey(ctx, userID, matchID, marketID, strike)
		if err == nil && projection != nil {
			positions, posErr := s.positionsFromProjections(ctx, []PositionProjection{*projection})
			if posErr == nil && len(positions) == 1 {
				return toSnapshot(positions[0]), true
			}
		}
	}

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

// OpenCloseTargets returns all open user positions in the snapshot shape needed
// by the orders service bulk exit path.
func (s *Service) OpenCloseTargets(ctx context.Context, userID primitive.ObjectID) ([]orders.PositionSnapshot, error) {
	open, err := s.GetUserOpenPositions(ctx, userID)
	if err != nil {
		return nil, err
	}
	targets := make([]orders.PositionSnapshot, 0, len(open))
	for _, p := range open {
		if p.Lots > 0 {
			targets = append(targets, toSnapshot(p))
		}
	}
	return targets, nil
}

func (s *Service) ApplyExecution(ctx context.Context, exec executions.Execution) error {
	if s.projections == nil {
		return nil
	}
	return s.projections.ApplyExecution(ctx, exec)
}

func toSnapshot(p Position) orders.PositionSnapshot {
	return orders.PositionSnapshot{
		ID:          p.ID,
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

	// Executions are fetched newest-first. Reverse them to oldest-first to replay history.
	chronological := make([]executions.Execution, len(fills))
	for i := range fills {
		chronological[i] = fills[len(fills)-1-i]
	}

	activeBuckets := make(map[key]*aggregateBucket)
	var allBuckets []*aggregateBucket
	var bucketKeys []key

	for _, fill := range chronological {
		k := key{userID: fill.UserID, matchID: fill.MatchID, marketID: fill.MarketID, strike: fill.Strike}
		b, ok := activeBuckets[k]
		if !ok {
			b = &aggregateBucket{firstSeen: fill.CreatedAt}
			activeBuckets[k] = b
			allBuckets = append(allBuckets, b)
			bucketKeys = append(bucketKeys, k)
		}
		b.add(fill)

		// Seal bucket if position is closed
		if b.buyQty == b.sellQty {
			delete(activeBuckets, k)
		}
	}

	marketIDs := make([]string, 0, len(bucketKeys))
	for _, key := range bucketKeys {
		marketIDs = append(marketIDs, key.marketID)
	}
	ltps := s.marketLTPs(ctx, marketIDs)
	out := make([]Position, 0, len(allBuckets))
	for i, b := range allBuckets {
		k := bucketKeys[i]
		p := b.toPosition()
		p.ID = derivePositionID(k.userID, k.matchID, k.marketID, k.strike, b.firstSeen)
		p.UserID = k.userID
		p.MatchID = k.matchID
		p.MarketID = k.marketID
		p.Strike = k.strike
		p.CreatedAt = b.firstSeen
		p.UpdatedAt = b.lastUpdated

		p.LTP = ltps[k.marketID]
		p.PnL = computePnL(p, b.matchedQty())
		p.RealizedPnL = computeRealized(b)
		out = append(out, p)
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

func (s *Service) projectedUserPositions(ctx context.Context, userID primitive.ObjectID, filter PositionFilter) ([]Position, bool, error) {
	projections, err := s.projections.List(ctx, ProjectionFilter{
		UserID:   userID,
		MatchID:  filter.MatchID,
		MarketID: filter.MarketID,
		Status:   filter.Status,
	})
	if err != nil {
		return nil, false, err
	}
	if len(projections) > 0 {
		positions, err := s.positionsFromProjections(ctx, projections)
		return positions, true, err
	}

	found, err := s.hasProjectionRows(ctx, userID)
	if err != nil {
		return nil, false, err
	}
	return []Position{}, found, nil
}

func (s *Service) projectedAdminPositions(ctx context.Context, userID primitive.ObjectID, filter PositionFilter) ([]Position, error) {
	return s.positionsFromProjectionFilter(ctx, ProjectionFilter{
		UserID:   userID,
		MatchID:  filter.MatchID,
		MarketID: filter.MarketID,
		Status:   filter.Status,
	})
}

func (s *Service) positionsFromProjectionFilter(ctx context.Context, filter ProjectionFilter) ([]Position, error) {
	projections, err := s.projections.List(ctx, filter)
	if err != nil {
		return nil, err
	}
	return s.positionsFromProjections(ctx, projections)
}

func (s *Service) positionsFromProjections(ctx context.Context, projections []PositionProjection) ([]Position, error) {
	marketIDs := make([]string, 0, len(projections))
	for _, projection := range projections {
		marketIDs = append(marketIDs, projection.MarketID)
	}
	ltps := s.marketLTPs(ctx, marketIDs)

	positions := make([]Position, 0, len(projections))
	for _, projection := range projections {
		positions = append(positions, projection.ToPosition(ltps[projection.MarketID]))
	}
	return positions, nil
}

func (s *Service) hasProjectionRows(ctx context.Context, userID primitive.ObjectID) (bool, error) {
	rows, err := s.projections.List(ctx, ProjectionFilter{UserID: userID})
	return len(rows) > 0, err
}

func (s *Service) marketLTPs(ctx context.Context, ids []string) map[string]float64 {
	marketIDs := make([]string, 0, len(ids))
	seen := make(map[string]struct{})
	for _, marketID := range ids {
		if marketID == "" {
			continue
		}
		if _, ok := seen[marketID]; ok {
			continue
		}
		seen[marketID] = struct{}{}
		marketIDs = append(marketIDs, marketID)
	}

	ltps := make(map[string]float64, len(marketIDs))
	if s.markets == nil {
		return ltps
	}
	if batch, ok := s.markets.(BatchMarketReader); ok {
		marketsByID, err := batch.GetMarketsByIDs(ctx, marketIDs)
		if err == nil {
			for id, market := range marketsByID {
				if market != nil {
					ltps[id] = market.LTP
				}
			}
			return ltps
		}
	}

	for _, marketID := range marketIDs {
		market, err := s.markets.GetMarketByID(ctx, marketID)
		if err == nil && market != nil {
			ltps[marketID] = market.LTP
		}
	}
	return ltps
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
		Status:      status,
		Lots:        net,
		BuyPrice:    round2(avgBuy),
		SellPrice:   round2(avgSell),
		MatchedLots: b.matchedQty(),
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
