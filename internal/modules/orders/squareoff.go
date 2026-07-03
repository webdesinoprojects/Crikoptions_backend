package orders

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

const (
	SquareOffScopeInnings1 = "innings1" // settle all open positions; close future markets only
	SquareOffScopeMatch    = "match"    // every open market for the match
)

// SquareOffResult summarizes a forced settlement run.
type SquareOffResult struct {
	MatchID           string  `json:"matchId"`
	Scope             string  `json:"scope"`
	MarketsClosed     int     `json:"marketsClosed"`
	OrdersCancelled   int     `json:"ordersCancelled"`
	PositionsSettled  int     `json:"positionsSettled"`
	PositionsFailed   int     `json:"positionsFailed"`
	TotalRealizedPnL  float64 `json:"totalRealizedPnl"`
}

// SquareOffInnings1 settles open positions in 1st-innings score markets when
// innings 1 ends. Implements matches.SettlementRunner.
func (s *Service) SquareOffInnings1(ctx context.Context, matchID string) error {
	_, err := s.SquareOff(ctx, matchID, SquareOffScopeInnings1)
	return err
}

// SquareOffMatch settles every open position for the match when the match ends.
// Implements matches.SettlementRunner.
func (s *Service) SquareOffMatch(ctx context.Context, matchID string) error {
	_, err := s.SquareOff(ctx, matchID, SquareOffScopeMatch)
	return err
}

// SquareOff finds open positions for a match, cancels working orders, force-closes
// positions at the current settlement quote, updates wallets, and closes markets.
func (s *Service) SquareOff(ctx context.Context, matchID string, scope string) (*SquareOffResult, error) {
	matchID = strings.TrimSpace(matchID)
	if matchID == "" {
		return nil, fmt.Errorf("matchId is required")
	}
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope != SquareOffScopeInnings1 && scope != SquareOffScopeMatch {
		return nil, fmt.Errorf("invalid square-off scope %q", scope)
	}
	if s.positions == nil {
		return nil, fmt.Errorf("position lookup is unavailable")
	}

	match, err := s.matches.GetMatchByID(ctx, matchID)
	if err != nil || match == nil {
		return nil, ErrMatchNotFound
	}

	marketList := s.markets.GetMarketsByMatchID(ctx, matchID)
	settleMarketIDs := activeMarketIDs(marketList)
	closeMarketIDs := marketsToClose(marketList, scope)
	if len(settleMarketIDs) == 0 {
		return &SquareOffResult{MatchID: matchID, Scope: scope}, nil
	}

	result := &SquareOffResult{MatchID: matchID, Scope: scope}

	result.OrdersCancelled = s.cancelWorkingOrders(ctx, match, settleMarketIDs)

	openPositions, err := s.collectOpenPositions(ctx, match)
	if err != nil {
		return nil, err
	}
	openPositions = filterPositionsByMarkets(openPositions, settleMarketIDs)

	marketByID := mapMarketsByHex(marketList)

	for _, target := range openPositions {
		if target.Lots <= 0 || target.UserID.IsZero() {
			continue
		}
		market := marketByID[target.MarketID]
		if market == nil {
			if m, mErr := s.markets.GetMarketByID(ctx, target.MarketID); mErr == nil && m != nil {
				market = m
			}
		}
		pnl, settleErr := s.forceSettlePosition(ctx, target.UserID, target, match, market)
		if settleErr != nil {
			result.PositionsFailed++
			log.Printf("square-off[%s]: settle user=%s market=%s strike=%v: %v",
				matchID, target.UserID.Hex(), target.MarketID, target.Strike, settleErr)
			continue
		}
		result.PositionsSettled++
		result.TotalRealizedPnL = round2(result.TotalRealizedPnL + pnl)
	}

	for marketID := range closeMarketIDs {
		if _, err := s.markets.SetMarketStatus(ctx, marketID, markets.MarketStatusClosed); err == nil {
			result.MarketsClosed++
		}
	}

	log.Printf("square-off[%s] scope=%s settled=%d failed=%d cancelled_orders=%d closed_markets=%d pnl=%.2f",
		matchID, scope, result.PositionsSettled, result.PositionsFailed,
		result.OrdersCancelled, result.MarketsClosed, result.TotalRealizedPnL)

	return result, nil
}

// ReopenMatchMarkets sets closed markets back to active (e.g. after simulator auto-loop reset).
func (s *Service) ReopenMatchMarkets(ctx context.Context, matchID string) error {
	for _, m := range s.markets.GetMarketsByMatchID(ctx, matchID) {
		if m.Status != markets.MarketStatusClosed {
			continue
		}
		if _, err := s.markets.SetMarketStatus(ctx, m.ID.Hex(), markets.MarketStatusActive); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) forceSettlePosition(
	ctx context.Context,
	userID primitive.ObjectID,
	target PositionSnapshot,
	match *matches.Match,
	market *markets.Market,
) (realizedPnL float64, err error) {
	unlock := s.lockFor(userID, target.MarketID, target.Strike)
	defer unlock()

	openQty := s.executions.OpenLongQty(ctx, userID, target.MatchID, target.MarketID, target.Strike)
	if openQty <= 0 {
		return 0, nil
	}

	fillPrice := s.settlementPrice(match, market, target)
	if fillPrice <= 0 {
		return 0, fmt.Errorf("no settlement price available")
	}

	now := time.Now().UTC()
	order := Order{
		UserID:            userID,
		MatchID:           target.MatchID,
		MarketID:          target.MarketID,
		Strike:            target.Strike,
		Side:              "sell",
		Type:              OrderTypeMarket,
		Quantity:          openQty,
		Price:             fillPrice,
		FilledQuantity:    0,
		RemainingQuantity: openQty,
		Status:            StatusOpen,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	var filled *Order
	err = s.repo.DoTx(ctx, func(txCtx context.Context) error {
		created, txErr := s.repo.Create(txCtx, order)
		if txErr != nil {
			return txErr
		}
		filled, txErr = s.applyFill(txCtx, userID, created, fillPrice, openQty)
		return txErr
	})
	if err != nil {
		return 0, err
	}

	buyPrice := target.BuyPrice
	if buyPrice <= 0 {
		buyPrice = fillPrice
	}
	realizedPnL = round2((fillPrice - buyPrice) * float64(openQty))

	if filled != nil {
		s.broadcastSell(ctx, userID, filled)
	}
	return realizedPnL, nil
}

func (s *Service) settlementPrice(match *matches.Match, market *markets.Market, target PositionSnapshot) float64 {
	if match != nil {
		input := markets.PricingInputFromMatch(*match)
		if bid, _, ok := s.markets.StrikeQuote(input, target.Strike); ok && bid > 0 {
			return round2(bid)
		}
	}
	if market != nil {
		if market.LTP > 0 {
			return round2(market.LTP)
		}
		if market.BuyerPrice > 0 {
			return round2(market.BuyerPrice)
		}
	}
	if target.LTP > 0 {
		return round2(target.LTP)
	}
	if target.BuyPrice > 0 {
		return round2(target.BuyPrice)
	}
	return 0
}

func (s *Service) cancelWorkingOrders(ctx context.Context, match *matches.Match, marketIDs map[string]struct{}) int {
	cancelled := 0
	matchKeys := matchIDKeys(match)
	for marketID := range marketIDs {
		for _, matchKey := range matchKeys {
			for _, status := range []string{StatusOpen, StatusPartiallyFilled} {
				orders := s.repo.List(ctx, OrderFilter{MatchID: matchKey, MarketID: marketID, Status: status})
				for i := range orders {
					if err := s.cancelWorkingOrder(ctx, &orders[i]); err != nil {
						log.Printf("square-off: cancel order %s: %v", orders[i].ID.Hex(), err)
						continue
					}
					cancelled++
				}
			}
		}
	}
	return cancelled
}

func (s *Service) cancelWorkingOrder(ctx context.Context, order *Order) error {
	if order == nil {
		return nil
	}
	if order.Status != StatusOpen && order.Status != StatusPartiallyFilled {
		return nil
	}
	return s.repo.DoTx(ctx, func(txCtx context.Context) error {
		if order.Side == "buy" && order.ReservedAmount() > 0 {
			if _, txErr := s.wallets.ReleaseOrderMargin(txCtx, order.UserID, order.ReservedAmount(), order.ID.Hex(),
				fmt.Sprintf("Release margin for cancelled order %s (square-off)", order.ID.Hex())); txErr != nil {
				return txErr
			}
		}
		_, txErr := s.repo.Cancel(txCtx, order.ID, order.UserID)
		return txErr
	})
}

func (s *Service) collectOpenPositions(ctx context.Context, match *matches.Match) ([]PositionSnapshot, error) {
	keySet := matchIDKeySet(match)
	batch, err := s.positions.ListOpenByMatch(ctx, "")
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	out := make([]PositionSnapshot, 0, len(batch))
	for _, p := range batch {
		if p.Lots <= 0 || !positionMatchesMatch(p, keySet) {
			continue
		}
		dedupe := p.UserID.Hex() + "|" + p.MatchID + "|" + p.MarketID + "|" + formatStrike(p.Strike)
		if _, ok := seen[dedupe]; ok {
			continue
		}
		seen[dedupe] = struct{}{}
		out = append(out, p)
	}
	return out, nil
}

// activeMarketIDs is every non-closed market for the match — used to settle positions.
func activeMarketIDs(marketList []markets.Market) map[string]struct{} {
	out := map[string]struct{}{}
	for _, m := range marketList {
		if m.Status == markets.MarketStatusClosed {
			continue
		}
		out[m.ID.Hex()] = struct{}{}
	}
	return out
}

// marketsToClose is the subset whose trading status becomes closed after settlement.
// Innings 1 end closes only 1st-innings score (future) markets; match end closes all.
func marketsToClose(marketList []markets.Market, scope string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, m := range marketList {
		if m.Status == markets.MarketStatusClosed {
			continue
		}
		if scope == SquareOffScopeInnings1 && m.Type != "future" {
			continue
		}
		out[m.ID.Hex()] = struct{}{}
	}
	return out
}

func matchIDKeySet(match *matches.Match) map[string]struct{} {
	set := map[string]struct{}{}
	for _, k := range matchIDKeys(match) {
		set[k] = struct{}{}
	}
	return set
}

func positionMatchesMatch(p PositionSnapshot, keySet map[string]struct{}) bool {
	if _, ok := keySet[p.MatchID]; ok {
		return true
	}
	hex := strings.TrimSpace(p.MatchID)
	if len(hex) >= 2 {
		if _, ok := keySet[hex[len(hex)-2:]]; ok {
			return true
		}
	}
	return false
}

func filterPositionsByMarkets(in []PositionSnapshot, marketIDs map[string]struct{}) []PositionSnapshot {
	if len(marketIDs) == 0 {
		return nil
	}
	out := make([]PositionSnapshot, 0, len(in))
	for _, p := range in {
		if _, ok := marketIDs[p.MarketID]; ok {
			out = append(out, p)
		}
	}
	return out
}

func mapMarketsByHex(marketList []markets.Market) map[string]*markets.Market {
	out := make(map[string]*markets.Market, len(marketList))
	for i := range marketList {
		m := &marketList[i]
		out[m.ID.Hex()] = m
	}
	return out
}

func matchIDKeys(match *matches.Match) []string {
	if match == nil {
		return nil
	}
	hex := match.ID.Hex()
	keys := []string{hex}
	if len(hex) >= 2 {
		keys = append(keys, hex[len(hex)-2:])
	}
	legacy := map[string]string{
		"0000000000000000000000aa": "1",
		"0000000000000000000000bb": "2",
		"0000000000000000000000cc": "3",
	}
	if short, ok := legacy[hex]; ok {
		keys = append(keys, short)
	}
	seen := map[string]struct{}{}
	unique := make([]string, 0, len(keys))
	for _, k := range keys {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		unique = append(unique, k)
	}
	return unique
}
