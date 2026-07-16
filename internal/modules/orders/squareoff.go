package orders

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
)

const (
	SquareOffScopeInnings1 = "innings1" // settle all open positions; close future markets only
	SquareOffScopeMatch    = "match"    // every open market for the match
)

// SquareOffResult summarizes a forced settlement run.
type SquareOffResult struct {
	MatchID          string  `json:"matchId"`
	Scope            string  `json:"scope"`
	MarketsClosed    int     `json:"marketsClosed"`
	OrdersCancelled  int     `json:"ordersCancelled"`
	PositionsSettled int     `json:"positionsSettled"`
	PositionsFailed  int     `json:"positionsFailed"`
	TotalRealizedPnL float64 `json:"totalRealizedPnl"`
}

type providerMarketGateWriter interface {
	SetProviderMarketGate(ctx context.Context, matchID string, innings int, lifecycle string, blockers []string, finalScore *int, finalRevision int64) error
}

type providerSettlementClaimer interface {
	ClaimProviderSettlement(ctx context.Context, matchID string, innings int, finalRevision int64) (bool, error)
}

type providerSettlementMarketReader interface {
	GetProviderSettlementMarket(ctx context.Context, matchID string, innings int, finalRevision int64) (*markets.Market, error)
}

type providerSettlementContract struct {
	innings       int
	finalRevision int64
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

// SettleProviderInnings settles exactly the contract revision held by the
// durable provider settlement job.
func (s *Service) SettleProviderInnings(ctx context.Context, matchID string, innings int, finalRevision int64) error {
	if innings < 1 || innings > 2 || finalRevision <= 0 {
		return errors.New("invalid provider settlement contract")
	}
	scope := SquareOffScopeMatch
	if innings == 1 {
		scope = SquareOffScopeInnings1
	}
	_, err := s.squareOff(ctx, matchID, scope, &providerSettlementContract{
		innings: innings, finalRevision: finalRevision,
	})
	return err
}

// CancelProviderWorkingOrders releases every working order after the feed
// transaction has closed the match gate. It is safe to retry.
func (s *Service) CancelProviderWorkingOrders(ctx context.Context, matchID string) (int, error) {
	match, err := s.matches.GetMatchByID(ctx, strings.TrimSpace(matchID))
	if err != nil || match == nil {
		return 0, ErrMatchNotFound
	}
	if match.DataSource != matches.DataSourceSportmonks {
		return 0, errors.New("working-order cancellation requires a provider match")
	}
	marketList, err := s.markets.ListMarketsByMatchID(ctx, match.ID.Hex())
	if err != nil {
		return 0, fmt.Errorf("list provider markets: %w", err)
	}
	marketIDs := make(map[string]struct{})
	for _, market := range marketList {
		marketIDs[market.ID.Hex()] = struct{}{}
	}
	cancelled, failed, err := s.cancelWorkingOrders(ctx, match, marketIDs)
	if err != nil {
		return cancelled, fmt.Errorf("list provider working orders: %w", err)
	}
	if failed > 0 {
		return cancelled, fmt.Errorf("%d working orders could not be cancelled", failed)
	}
	return cancelled, nil
}

func (s *Service) VoidProviderInningsMarket(ctx context.Context, matchID string, innings int) error {
	match, err := s.matches.GetMatchByID(ctx, strings.TrimSpace(matchID))
	if err != nil || match == nil {
		return ErrMatchNotFound
	}
	if match.DataSource != matches.DataSourceSportmonks || innings < 1 || innings > 2 {
		return errors.New("invalid provider void contract")
	}
	var market *markets.Market
	marketList, err := s.markets.ListMarketsByMatchID(ctx, match.ID.Hex())
	if err != nil {
		return fmt.Errorf("list provider markets: %w", err)
	}
	for i := range marketList {
		candidate := marketList[i]
		if candidate.Kind == markets.MarketKindInningsScore && candidate.Innings == innings &&
			candidate.FormulaVersion == markets.FormulaVersionInningsScoreV1 {
			copy := candidate
			market = &copy
			break
		}
	}
	if market == nil {
		return errors.New("provider void market not found")
	}
	if market.Lifecycle == markets.MarketLifecycleVoid {
		return nil
	}
	if market.Lifecycle != markets.MarketLifecycleSettling || market.SettlementRevision > 0 {
		return errors.New("provider market is not voidable")
	}
	marketIDs := map[string]struct{}{market.ID.Hex(): {}}
	_, failed, err := s.cancelWorkingOrders(ctx, match, marketIDs)
	if err != nil {
		return fmt.Errorf("list provider working orders: %w", err)
	}
	if failed > 0 {
		return fmt.Errorf("%d working orders could not be cancelled before void", failed)
	}
	if err := s.reverseProviderMarketExecutions(ctx, match, market); err != nil {
		return err
	}
	remaining, err := s.collectOpenPositions(ctx, match)
	if err != nil {
		return err
	}
	if len(filterPositionsByMarkets(remaining, marketIDs)) != 0 {
		return errors.New("provider void still has open positions")
	}
	writer, ok := s.markets.(providerMarketGateWriter)
	if !ok {
		return errors.New("provider market void writer is unavailable")
	}
	return writer.SetProviderMarketGate(ctx, match.ID.Hex(), innings, markets.MarketLifecycleVoid, nil, nil, 0)
}

// reverseProviderMarketExecutions unwinds every fill in reverse chronological
// order at its original price. This covers fully closed and partially closed
// positions, restores cash and collateral, and leaves aggregate contract P&L
// at zero. Stable per-execution order IDs make the unwind resumable.
func (s *Service) reverseProviderMarketExecutions(ctx context.Context, match *matches.Match, market *markets.Market) error {
	if match == nil || market == nil {
		return errors.New("provider void contract is unavailable")
	}
	originals, err := s.executions.ListWithError(ctx, executions.Filter{
		MatchID: match.ID.Hex(), MarketID: market.ID.Hex(), Limit: 10000,
		ExcludeLiquiditySource: executions.LiquidityProviderVoidReverse,
	})
	if err != nil {
		return fmt.Errorf("list provider executions for void: %w", err)
	}
	if len(originals) >= 10000 {
		return errors.New("provider void execution history exceeds the safe batch limit")
	}
	openPositions, err := s.collectOpenPositions(ctx, match)
	if err != nil {
		return fmt.Errorf("read provider positions for void: %w", err)
	}
	shortCollateral := make(map[primitive.ObjectID]float64)
	for _, position := range filterPositionsByMarkets(openPositions, map[string]struct{}{market.ID.Hex(): {}}) {
		if position.Lots >= 0 {
			continue
		}
		if position.ShortCollateral <= 0 || math.IsNaN(position.ShortCollateral) || math.IsInf(position.ShortCollateral, 0) {
			return fmt.Errorf("provider short position %s/%s has no authoritative collateral", position.UserID.Hex(), formatStrike(position.Strike))
		}
		shortCollateral[position.UserID] = round2(shortCollateral[position.UserID] + position.ShortCollateral)
	}
	compensations, err := providerVoidCompensations(originals, match.ID.Hex(), market.ID.Hex(), shortCollateral)
	if err != nil {
		return err
	}
	for i := range compensations {
		frozen, freezeErr := s.repo.FreezeProviderVoidCompensation(ctx, ProviderVoidCompensation{
			MarketID: market.ID.Hex(), UserID: compensations[i].userID,
			CashDelta: compensations[i].cashDelta, ReservedDelta: compensations[i].reservedDelta,
			ExecutionHash: compensations[i].executionHash,
		})
		if freezeErr != nil {
			return fmt.Errorf("freeze provider void compensation for user %s: %w", compensations[i].userID.Hex(), freezeErr)
		}
		compensations[i].cashDelta = frozen.CashDelta
		compensations[i].reservedDelta = frozen.ReservedDelta
		compensations[i].executionHash = frozen.ExecutionHash
	}
	for _, compensation := range compensations {
		if compensation.cashDelta == 0 && compensation.reservedDelta == 0 {
			continue
		}
		operationKey := fmt.Sprintf(
			"wallet:user:%s:contract:void:%s:0:%s:reverse",
			compensation.userID.Hex(), market.ID.Hex(), market.FormulaVersion,
		)
		fundsCtx := wallet.WithOperationKey(ctx, operationKey)
		if _, err := s.wallets.ReverseProviderContract(
			fundsCtx,
			compensation.userID,
			compensation.cashDelta,
			compensation.reservedDelta,
			market.ID.Hex(),
			fmt.Sprintf("Reverse voided provider market %s", market.ID.Hex()),
		); err != nil {
			return fmt.Errorf("reverse provider wallet for user %s: %w", compensation.userID.Hex(), err)
		}
	}
	sort.SliceStable(originals, func(i, j int) bool {
		if originals[i].CreatedAt.Equal(originals[j].CreatedAt) {
			return originals[i].ID.Hex() > originals[j].ID.Hex()
		}
		return originals[i].CreatedAt.After(originals[j].CreatedAt)
	})
	for _, original := range originals {
		side := "buy"
		if original.Side == "buy" {
			side = "sell"
		}
		clientOrderID := fmt.Sprintf(
			"void:%s:execution:%s:0:%s:reverse",
			market.ID.Hex(), original.ID.Hex(), market.FormulaVersion,
		)
		order, err := s.repo.FindByClientOrderID(ctx, original.UserID, clientOrderID)
		if err != nil {
			return err
		}
		if order == nil {
			now := time.Now().UTC()
			order, err = s.repo.Create(ctx, Order{
				ClientOrderID: clientOrderID, UserID: original.UserID,
				MatchID: match.ID.Hex(), MarketID: market.ID.Hex(), Strike: original.Strike,
				Side: side, Type: OrderTypeMarket, PositionEffect: PositionEffectAuto,
				PositionIntent: positionIntentProviderVoidReverse,
				Quantity:       original.Quantity, Price: original.Price,
				OutstandingReserve: 0, ReserveReconciled: true,
				RemainingQuantity: original.Quantity, Status: StatusOpen,
				CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				return err
			}
		}
		if order.Status == StatusExecuted && order.RemainingQuantity == 0 {
			continue
		}
		if order.Status != StatusOpen && order.Status != StatusPartiallyFilled {
			return fmt.Errorf("provider void order %s is %s", order.ID.Hex(), order.Status)
		}
		if order.RemainingQuantity <= 0 {
			return fmt.Errorf("provider void order %s has invalid remaining quantity", order.ID.Hex())
		}
		filled, err := s.applySettlementFill(ctx, original.UserID, order, original.Price, order.RemainingQuantity)
		if err != nil {
			return err
		}
		s.broadcastOrderAndPosition(ctx, original.UserID, filled)
	}
	return nil
}

type providerVoidCompensation struct {
	userID        primitive.ObjectID
	cashDelta     float64
	reservedDelta float64
	executionHash string
}

// providerVoidCompensations reverses cash from immutable fills and collateral
// from the committed position projection. Collateral must not be reconstructed
// from wall-clock execution timestamps because cross-replica commit order can
// differ from CreatedAt order.
func providerVoidCompensations(originals []executions.Execution, matchID, marketID string, shortCollateral map[primitive.ObjectID]float64) ([]providerVoidCompensation, error) {
	byUser := make(map[primitive.ObjectID]*providerVoidCompensation)
	executionInputs := make(map[primitive.ObjectID][]string)
	for _, original := range originals {
		if original.ID.IsZero() || original.UserID.IsZero() || original.MatchID != matchID || original.MarketID != marketID ||
			original.Strike <= 0 || original.Quantity <= 0 || original.Price < 0 || math.IsNaN(original.Price) || math.IsInf(original.Price, 0) ||
			(original.Side != "buy" && original.Side != "sell") {
			return nil, fmt.Errorf("invalid provider execution %s", original.ID.Hex())
		}

		compensation := byUser[original.UserID]
		if compensation == nil {
			compensation = &providerVoidCompensation{userID: original.UserID}
			byUser[original.UserID] = compensation
		}
		executionInputs[original.UserID] = append(executionInputs[original.UserID], strings.Join([]string{
			original.ID.Hex(), original.OrderID.Hex(), original.MatchID, original.MarketID,
			strconv.FormatFloat(original.Strike, 'g', -1, 64), strconv.Itoa(original.Quantity),
			original.Side, strconv.FormatFloat(original.Price, 'g', -1, 64),
		}, "|"))
		switch original.Side {
		case "buy":
			compensation.cashDelta = round2(compensation.cashDelta + round2(original.Price*float64(original.Quantity)))
		case "sell":
			compensation.cashDelta = round2(compensation.cashDelta - round2(original.Price*float64(original.Quantity)))
		}
	}

	for userID, collateral := range shortCollateral {
		compensation := byUser[userID]
		if compensation == nil {
			return nil, fmt.Errorf("provider collateral for user %s has no execution history", userID.Hex())
		}
		if collateral <= 0 || math.IsNaN(collateral) || math.IsInf(collateral, 0) {
			return nil, fmt.Errorf("provider collateral for user %s is invalid", userID.Hex())
		}
		compensation.reservedDelta = round2(-collateral)
	}

	out := make([]providerVoidCompensation, 0, len(byUser))
	for _, compensation := range byUser {
		compensation.cashDelta = round2(compensation.cashDelta)
		compensation.reservedDelta = round2(compensation.reservedDelta)
		inputs := executionInputs[compensation.userID]
		sort.Strings(inputs)
		sum := sha256.Sum256([]byte(strings.Join(inputs, "\n")))
		compensation.executionHash = hex.EncodeToString(sum[:])
		out = append(out, *compensation)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].userID.Hex() < out[j].userID.Hex() })
	return out, nil
}

// SquareOff finds open positions for a match, cancels working orders, force-closes
// positions at the current settlement quote, updates wallets, and closes markets.
func (s *Service) SquareOff(ctx context.Context, matchID string, scope string) (*SquareOffResult, error) {
	return s.squareOff(ctx, matchID, scope, nil)
}

func (s *Service) squareOff(ctx context.Context, matchID string, scope string, expected *providerSettlementContract) (*SquareOffResult, error) {
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

	providerMatch := match.DataSource == matches.DataSourceSportmonks
	var marketList []markets.Market
	if providerMatch {
		if expected == nil {
			return nil, errors.New("provider settlement requires an expected contract revision")
		}
		reader, ok := s.markets.(providerSettlementMarketReader)
		if !ok {
			return nil, errors.New("provider settlement market reader is unavailable")
		}
		market, lookupErr := reader.GetProviderSettlementMarket(ctx, match.ID.Hex(), expected.innings, expected.finalRevision)
		if lookupErr != nil {
			return nil, fmt.Errorf("read provider settlement market: %w", lookupErr)
		}
		if market == nil {
			return nil, errors.New("expected provider settlement market was not found")
		}
		if market.MatchID != match.ID.Hex() || market.Kind != markets.MarketKindInningsScore ||
			market.Innings != expected.innings || market.FormulaVersion != markets.FormulaVersionInningsScoreV1 ||
			market.FinalRevision != expected.finalRevision {
			return nil, errors.New("provider settlement market does not match the expected contract")
		}
		marketList = []markets.Market{*market}
	} else {
		if expected != nil {
			return nil, errors.New("provider settlement requires a provider-owned match")
		}
		marketList = s.markets.GetMarketsByMatchID(ctx, matchID)
	}
	settleMarketIDs := activeMarketIDs(marketList)
	closeMarketIDs := marketsToClose(marketList, scope)
	if providerMatch {
		settleMarketIDs = providerSettlementMarketIDs(marketList, scope)
		closeMarketIDs = settleMarketIDs
	}
	if len(settleMarketIDs) == 0 {
		if providerMatch {
			market := marketList[0]
			if market.Lifecycle != markets.MarketLifecycleSettled || market.SettlementRevision != expected.finalRevision {
				return nil, errors.New("provider settlement market is not settling or settled at the expected revision")
			}
		}
		return &SquareOffResult{MatchID: matchID, Scope: scope}, nil
	}

	marketByID := mapMarketsByHex(marketList)
	result := &SquareOffResult{MatchID: matchID, Scope: scope}
	if providerMatch {
		claimer, ok := s.markets.(providerSettlementClaimer)
		if !ok {
			return result, errors.New("provider settlement claimer is unavailable")
		}
		for marketID := range settleMarketIDs {
			market := marketByID[marketID]
			if market == nil {
				return result, errors.New("provider settlement market is unavailable")
			}
			claimed, claimErr := claimer.ClaimProviderSettlement(ctx, market.MatchID, market.Innings, market.FinalRevision)
			if claimErr != nil {
				return result, claimErr
			}
			if !claimed {
				return result, errors.New("provider final revision changed before settlement")
			}
		}
	}

	var cancellationFailures int
	result.OrdersCancelled, cancellationFailures, err = s.cancelWorkingOrders(ctx, match, settleMarketIDs)
	if err != nil {
		if providerMatch {
			return result, fmt.Errorf("list provider working orders: %w", err)
		}
		return result, err
	}
	if providerMatch && cancellationFailures > 0 {
		return result, fmt.Errorf("provider settlement has %d order cancellation failures", cancellationFailures)
	}

	openPositions, err := s.collectOpenPositions(ctx, match)
	if err != nil {
		return nil, err
	}
	openPositions = filterPositionsByMarkets(openPositions, settleMarketIDs)

	for _, target := range openPositions {
		if target.Lots == 0 || target.UserID.IsZero() {
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
	if providerMatch && result.PositionsFailed > 0 {
		return result, fmt.Errorf("provider settlement has %d position failures", result.PositionsFailed)
	}
	if providerMatch {
		remaining, listErr := s.collectOpenPositions(ctx, match)
		if listErr != nil {
			return result, listErr
		}
		if len(filterPositionsByMarkets(remaining, settleMarketIDs)) != 0 {
			return result, errors.New("provider settlement still has open positions")
		}
	}

	for marketID := range closeMarketIDs {
		if providerMatch {
			writer, ok := s.markets.(providerMarketGateWriter)
			market := marketByID[marketID]
			if !ok || market == nil {
				return result, errors.New("provider market settlement writer is unavailable")
			}
			finalScore := market.FinalScore
			if err := writer.SetProviderMarketGate(ctx, match.ID.Hex(), market.Innings, markets.MarketLifecycleSettled, nil, &finalScore, market.FinalRevision); err != nil {
				return result, err
			}
			result.MarketsClosed++
		} else if _, err := s.markets.SetMarketStatus(ctx, marketID, markets.MarketStatusClosed); err == nil {
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
	return s.forceClosePosition(ctx, userID, target, match, market)
}

func (s *Service) forceClosePosition(
	ctx context.Context,
	userID primitive.ObjectID,
	target PositionSnapshot,
	match *matches.Match,
	market *markets.Market,
) (realizedPnL float64, err error) {
	unlock := s.lockFor(userID, target.MarketID, target.Strike)
	defer unlock()

	net := target.Lots
	if net == 0 {
		return 0, nil
	}
	closeQty := absInt(net)

	fillPrice, hasSettlementPrice := s.settlementPrice(match, market, target)
	if !hasSettlementPrice || fillPrice < 0 {
		return 0, fmt.Errorf("no settlement price available")
	}
	side := "sell"
	if net < 0 {
		side = "buy"
	}

	now := time.Now().UTC()
	order := Order{
		UserID:             userID,
		MatchID:            target.MatchID,
		MarketID:           target.MarketID,
		Strike:             target.Strike,
		Side:               side,
		Type:               OrderTypeMarket,
		PositionEffect:     PositionEffectClose,
		PositionIntent:     planIntent(side, positionPlan{CloseLongQty: maxInt(net, 0), CoverShortQty: maxInt(-net, 0)}),
		Quantity:           closeQty,
		Price:              fillPrice,
		ReservedAmount:     0,
		ReservedQuantity:   0,
		OutstandingReserve: 0,
		ReserveReconciled:  true,
		FilledQuantity:     0,
		RemainingQuantity:  closeQty,
		Status:             StatusOpen,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if market != nil && market.Kind == markets.MarketKindInningsScore && market.FinalRevision > 0 {
		order.ClientOrderID = fmt.Sprintf(
			"settlement:%s:%s:%d:%s:close",
			market.ID.Hex(), formatStrike(target.Strike), market.FinalRevision, market.FormulaVersion,
		)
	}
	var created *Order
	if order.ClientOrderID != "" {
		created, err = s.repo.FindByClientOrderID(ctx, userID, order.ClientOrderID)
		if err != nil {
			return 0, err
		}
	}
	if created == nil {
		created, err = s.repo.Create(ctx, order)
		if err != nil {
			return 0, err
		}
	}
	if created.Status != StatusOpen && created.Status != StatusPartiallyFilled {
		return 0, fmt.Errorf("settlement order %s is %s", created.ID.Hex(), created.Status)
	}

	filled, err := s.applySettlementFill(ctx, userID, created, fillPrice, created.RemainingQuantity)
	if err != nil {
		return 0, err
	}

	if net > 0 {
		buyPrice := target.BuyPrice
		if buyPrice <= 0 {
			buyPrice = fillPrice
		}
		realizedPnL = round2((fillPrice - buyPrice) * float64(closeQty))
	} else {
		sellPrice := target.SellPrice
		if sellPrice <= 0 {
			sellPrice = fillPrice
		}
		realizedPnL = round2((sellPrice - fillPrice) * float64(closeQty))
	}

	if filled != nil {
		s.broadcastOrderAndPosition(ctx, userID, filled)
	}
	return realizedPnL, nil
}

func (s *Service) settlementPrice(match *matches.Match, market *markets.Market, target PositionSnapshot) (float64, bool) {
	if market != nil && market.Kind == markets.MarketKindInningsScore && market.FormulaVersion == markets.FormulaVersionInningsScoreV1 && market.FinalRevision > 0 {
		payoff := float64(market.FinalScore) - target.Strike
		if payoff < 0 {
			payoff = 0
		}
		return round2(payoff), true
	}
	if match != nil {
		input := markets.PricingInputFromMatch(*match)
		bid, ask, ok := s.markets.StrikeQuote(input, target.Strike)
		if target.Lots < 0 && ok && ask > 0 {
			return round2(ask), true
		}
		if ok && bid > 0 {
			return round2(bid), true
		}
	}
	if market != nil {
		if target.Lots < 0 && market.SellerPrice > 0 {
			return round2(market.SellerPrice), true
		}
		if market.LTP > 0 {
			return round2(market.LTP), true
		}
		if market.BuyerPrice > 0 {
			return round2(market.BuyerPrice), true
		}
	}
	if target.LTP > 0 {
		return round2(target.LTP), true
	}
	if target.Lots < 0 && target.SellPrice > 0 {
		return round2(target.SellPrice), true
	}
	if target.BuyPrice > 0 {
		return round2(target.BuyPrice), true
	}
	return 0, false
}

func (s *Service) cancelWorkingOrders(ctx context.Context, match *matches.Match, marketIDs map[string]struct{}) (cancelled, failed int, err error) {
	matchKeys := matchIDKeys(match)
	for marketID := range marketIDs {
		for _, matchKey := range matchKeys {
			for _, status := range []string{StatusOpen, StatusPartiallyFilled} {
				orders, listErr := s.repo.ListWithError(ctx, OrderFilter{MatchID: matchKey, MarketID: marketID, Status: status})
				if listErr != nil {
					return cancelled, failed, listErr
				}
				for i := range orders {
					if isInternalClientOrderID(orders[i].ClientOrderID) {
						continue
					}
					if err := s.cancelWorkingOrder(ctx, &orders[i]); err != nil {
						log.Printf("square-off: cancel order %s: %v", orders[i].ID.Hex(), err)
						failed++
						continue
					}
					cancelled++
				}
			}
		}
	}
	return cancelled, failed, nil
}

func (s *Service) cancelWorkingOrder(ctx context.Context, order *Order) error {
	if order == nil {
		return nil
	}
	if order.Status != StatusOpen && order.Status != StatusPartiallyFilled {
		return nil
	}
	return s.repo.DoTx(ctx, func(txCtx context.Context) error {
		cancelled, txErr := s.repo.Cancel(txCtx, order.ID, order.UserID)
		if txErr != nil || cancelled == nil {
			return txErr
		}
		if remainingReserve := cancelled.RemainingReservedAmount(); remainingReserve > 0 {
			fundsCtx := orderFundsOperationContext(txCtx, order.UserID, order.ID, "cancel-release")
			if _, txErr := s.wallets.ReleaseOrderMargin(fundsCtx, order.UserID, remainingReserve, order.ID.Hex(),
				fmt.Sprintf("Release margin for cancelled order %s (square-off)", order.ID.Hex())); txErr != nil {
				return txErr
			}
		}
		return nil
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
		if p.Lots == 0 || !positionMatchesMatch(p, keySet) {
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

func providerSettlementMarketIDs(marketList []markets.Market, scope string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, market := range marketList {
		if market.Kind != markets.MarketKindInningsScore ||
			market.FormulaVersion != markets.FormulaVersionInningsScoreV1 ||
			market.Lifecycle != markets.MarketLifecycleSettling ||
			market.FinalRevision <= 0 {
			continue
		}
		if scope == SquareOffScopeInnings1 && market.Innings != 1 {
			continue
		}
		out[market.ID.Hex()] = struct{}{}
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
	if match.DataSource == matches.DataSourceSportmonks {
		return []string{hex}
	}
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
