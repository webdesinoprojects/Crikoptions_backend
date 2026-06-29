package portfolio

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/positions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
)

type PositionReader interface {
	GetUserOpenPositions(ctx context.Context, userID primitive.ObjectID) ([]positions.Position, error)
	GetUserClosedPositions(ctx context.Context, userID primitive.ObjectID) ([]positions.Position, error)
}

type PositionLister interface {
	ListUserPositions(ctx context.Context, userID primitive.ObjectID, filter positions.PositionFilter) ([]positions.Position, error)
}

type WalletReader interface {
	GetWallet(ctx context.Context, userID primitive.ObjectID) (*wallet.Account, error)
}

type MarketReader interface {
	GetMarketByID(ctx context.Context, id string) (*markets.Market, error)
}

type MatchReader interface {
	GetMatchByID(ctx context.Context, id string) (*matches.Match, error)
}

type BatchMarketReader interface {
	GetMarketsByIDs(ctx context.Context, ids []string) (map[string]*markets.Market, error)
}

type BatchMatchReader interface {
	GetMatchesByIDs(ctx context.Context, ids []string) (map[string]*matches.Match, error)
}

type Service struct {
	positions PositionReader
	wallets   WalletReader
	markets   MarketReader
	matches   MatchReader
}

func NewService(positions PositionReader, wallets WalletReader, markets MarketReader, matches MatchReader) *Service {
	return &Service{
		positions: positions,
		wallets:   wallets,
		markets:   markets,
		matches:   matches,
	}
}

func (s *Service) GetSummary(ctx context.Context, userID primitive.ObjectID) (*PortfolioSummary, error) {
	all, err := s.listPositions(ctx, userID)
	if err != nil {
		return nil, err
	}
	open := filterPositionsByStatus(all, "open")
	closed := filterPositionsByStatus(all, "closed")

	account, err := s.wallets.GetWallet(ctx, userID)
	if err != nil {
		return nil, err
	}

	lookup := newLookupCache(s)
	lookup.preload(ctx, all)
	portfolioPositions := make([]PortfolioPosition, 0, len(open))
	for _, position := range open {
		portfolioPositions = append(portfolioPositions, s.adaptOpenPosition(ctx, lookup, position))
	}
	fillAllocations(portfolioPositions)

	closedTrades := make([]ClosedTrade, 0, len(closed))
	for _, position := range closed {
		closedTrades = append(closedTrades, s.adaptClosedTrade(ctx, lookup, position))
	}

	totalUnrealizedPnL := sumOpenPnL(portfolioPositions)
	totalRealizedPnL := sumClosedPnL(closedTrades)
	totalPnL := round2(totalUnrealizedPnL + totalRealizedPnL)
	totalPositionValue := sumPositionValue(portfolioPositions)
	totalEquity := round2(account.CashBalance + totalPositionValue)
	dailyPnL := round2(computeDailyPnL(portfolioPositions, closedTrades, time.Now()))
	marginUsagePct := computeMarginUsage(account.ReservedBalance, account.AvailableBalance)

	return &PortfolioSummary{
		TotalEquity:        totalEquity,
		BaseCapital:        round2(account.CashBalance),
		TotalUnrealizedPnL: round2(totalUnrealizedPnL),
		TotalRealizedPnL:   round2(totalRealizedPnL),
		TotalPnL:           totalPnL,
		TotalPnLPct:        pct(totalPnL, account.CashBalance),
		DailyPnL:           dailyPnL,
		DailyPnLPct:        pct(dailyPnL, account.CashBalance),
		OpenPositionsCount: len(portfolioPositions),
		ClosedTradesCount:  len(closedTrades),
		WinRate:            computeWinRate(closedTrades),
		AvgWin:             average(winningPnLs(closedTrades)),
		AvgLoss:            average(losingPnLs(closedTrades)),
		ProfitFactor:       computeProfitFactor(closedTrades),
		AvailableMargin:    round2(account.AvailableBalance),
		UsedMargin:         round2(account.ReservedBalance),
		MarginUsagePct:     marginUsagePct,
		Wallet:             *account,
		Positions:          portfolioPositions,
		ClosedTrades:       closedTrades,
		EquityCurve:        buildEquityCurve(closedTrades),
		RiskMetrics:        computeRiskMetrics(portfolioPositions, totalEquity),
	}, nil
}

func (s *Service) GetDailyPnL(ctx context.Context, userID primitive.ObjectID) (*DailyPnLResponse, error) {
	summary, err := s.GetSummary(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &DailyPnLResponse{DailyPnL: summary.DailyPnL, DailyPnLPct: summary.DailyPnLPct}, nil
}

func (s *Service) GetRiskSummary(ctx context.Context, userID primitive.ObjectID) (*RiskSummaryResponse, error) {
	summary, err := s.GetSummary(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &RiskSummaryResponse{
		RiskMetrics:     summary.RiskMetrics,
		MarginUsagePct:  summary.MarginUsagePct,
		UsedMargin:      summary.UsedMargin,
		AvailableMargin: summary.AvailableMargin,
	}, nil
}

func (s *Service) GetMarketPnL(ctx context.Context, userID primitive.ObjectID, marketID string) (*MarketPnLResponse, error) {
	all, err := s.listPositions(ctx, userID)
	if err != nil {
		return nil, err
	}
	open := filterPositionsByStatus(all, "open")
	closed := filterPositionsByStatus(all, "closed")

	openPnL := 0.0
	for _, position := range open {
		if position.MarketID == marketID {
			openPnL += position.PnL
		}
	}

	closedPnL := 0.0
	for _, position := range closed {
		if position.MarketID == marketID {
			closedPnL += realizedPnL(position)
		}
	}

	return &MarketPnLResponse{
		MarketID:  marketID,
		OpenPnL:   round2(openPnL),
		ClosedPnL: round2(closedPnL),
		TotalPnL:  round2(openPnL + closedPnL),
	}, nil
}

func (s *Service) adaptOpenPosition(ctx context.Context, lookup *lookupCache, position positions.Position) PortfolioPosition {
	market := lookup.market(ctx, position.MarketID)
	match := lookup.match(ctx, position.MatchID)

	side := "BUY"
	if position.Lots < 0 {
		side = "SELL"
	}
	quantity := absInt(position.Lots)
	entryPrice := position.BuyPrice
	if side == "SELL" && position.SellPrice > 0 {
		entryPrice = position.SellPrice
	}
	notional := round2(float64(quantity) * position.LTP)
	pnl := round2(position.PnL)

	return PortfolioPosition{
		ID:                position.ID,
		MarketID:          position.MarketID,
		Symbol:            symbolFromMarket(market, position.MarketID),
		MatchName:         matchName(match, position.MatchID),
		Strike:            formatStrike(position.Strike),
		Side:              side,
		Quantity:          quantity,
		AverageEntryPrice: round2(entryPrice),
		CurrentPrice:      round2(position.LTP),
		UnrealizedPnL:     pnl,
		UnrealizedPnLPct:  pct(pnl, entryPrice*float64(quantity)),
		Notional:          notional,
		OpenedAt:          formatTime(position.CreatedAt),
	}
}

func (s *Service) adaptClosedTrade(ctx context.Context, lookup *lookupCache, position positions.Position) ClosedTrade {
	market := lookup.market(ctx, position.MarketID)
	match := lookup.match(ctx, position.MatchID)

	quantity := position.MatchedLots
	if quantity == 0 {
		quantity = absInt(position.Lots)
	}
	pnl := realizedPnL(position)
	openedAt := position.CreatedAt
	closedAt := position.UpdatedAt
	if closedAt.IsZero() {
		closedAt = openedAt
	}

	return ClosedTrade{
		OrderID:         position.ID,
		MarketID:        position.MarketID,
		Symbol:          symbolFromMarket(market, position.MarketID),
		MatchName:       matchName(match, position.MatchID),
		Side:            "BUY",
		Quantity:        quantity,
		EntryPrice:      round2(position.BuyPrice),
		ExitPrice:       round2(position.SellPrice),
		RealizedPnL:     pnl,
		RealizedPnLPct:  pct(pnl, position.BuyPrice*float64(quantity)),
		OpenedAt:        formatTime(openedAt),
		ClosedAt:        formatTime(closedAt),
		HoldingPeriodMs: maxInt64(0, closedAt.Sub(openedAt).Milliseconds()),
	}
}

type lookupCache struct {
	service *Service
	markets map[string]*markets.Market
	matches map[string]*matches.Match
}

func newLookupCache(service *Service) *lookupCache {
	return &lookupCache{
		service: service,
		markets: make(map[string]*markets.Market),
		matches: make(map[string]*matches.Match),
	}
}

func (c *lookupCache) preload(ctx context.Context, positions []positions.Position) {
	marketIDs := make([]string, 0, len(positions))
	matchIDs := make([]string, 0, len(positions))
	marketSeen := make(map[string]struct{})
	matchSeen := make(map[string]struct{})

	for _, position := range positions {
		if position.MarketID != "" {
			if _, ok := marketSeen[position.MarketID]; !ok {
				marketSeen[position.MarketID] = struct{}{}
				marketIDs = append(marketIDs, position.MarketID)
			}
		}
		if position.MatchID != "" {
			if _, ok := matchSeen[position.MatchID]; !ok {
				matchSeen[position.MatchID] = struct{}{}
				matchIDs = append(matchIDs, position.MatchID)
			}
		}
	}

	if batch, ok := c.service.markets.(BatchMarketReader); ok {
		if marketsByID, err := batch.GetMarketsByIDs(ctx, marketIDs); err == nil {
			for id, market := range marketsByID {
				c.markets[id] = market
			}
		}
	}
	if batch, ok := c.service.matches.(BatchMatchReader); ok {
		if matchesByID, err := batch.GetMatchesByIDs(ctx, matchIDs); err == nil {
			for id, match := range matchesByID {
				c.matches[id] = match
			}
		}
	}
}

func (c *lookupCache) market(ctx context.Context, id string) *markets.Market {
	if id == "" || c.service.markets == nil {
		return nil
	}
	if market, ok := c.markets[id]; ok {
		return market
	}
	market, err := c.service.markets.GetMarketByID(ctx, id)
	if err != nil {
		c.markets[id] = nil
		return nil
	}
	c.markets[id] = market
	return market
}

func (c *lookupCache) match(ctx context.Context, id string) *matches.Match {
	if id == "" || c.service.matches == nil {
		return nil
	}
	if match, ok := c.matches[id]; ok {
		return match
	}
	match, err := c.service.matches.GetMatchByID(ctx, id)
	if err != nil {
		c.matches[id] = nil
		return nil
	}
	c.matches[id] = match
	return match
}

func (s *Service) listPositions(ctx context.Context, userID primitive.ObjectID) ([]positions.Position, error) {
	if lister, ok := s.positions.(PositionLister); ok {
		return lister.ListUserPositions(ctx, userID, positions.PositionFilter{})
	}

	open, err := s.positions.GetUserOpenPositions(ctx, userID)
	if err != nil {
		return nil, err
	}
	closed, err := s.positions.GetUserClosedPositions(ctx, userID)
	if err != nil {
		return nil, err
	}
	all := make([]positions.Position, 0, len(open)+len(closed))
	all = append(all, open...)
	all = append(all, closed...)
	return all, nil
}

func filterPositionsByStatus(in []positions.Position, status string) []positions.Position {
	out := make([]positions.Position, 0, len(in))
	for _, position := range in {
		if position.Status == status {
			out = append(out, position)
		}
	}
	return out
}

func sumOpenPnL(positions []PortfolioPosition) float64 {
	total := 0.0
	for _, position := range positions {
		total += position.UnrealizedPnL
	}
	return round2(total)
}

func sumClosedPnL(trades []ClosedTrade) float64 {
	total := 0.0
	for _, trade := range trades {
		total += trade.RealizedPnL
	}
	return round2(total)
}

func sumPositionValue(positions []PortfolioPosition) float64 {
	total := 0.0
	for _, position := range positions {
		if position.Side == "SELL" {
			total -= position.Notional
		} else {
			total += position.Notional
		}
	}
	return round2(total)
}

func fillAllocations(positions []PortfolioPosition) {
	totalNotional := 0.0
	for _, position := range positions {
		totalNotional += position.Notional
	}
	for i := range positions {
		positions[i].Allocation = pct(positions[i].Notional, totalNotional)
	}
}

func computeDailyPnL(positions []PortfolioPosition, closedTrades []ClosedTrade, now time.Time) float64 {
	total := 0.0
	for _, position := range positions {
		if isSameLocalDay(parseTime(position.OpenedAt), now) {
			total += position.UnrealizedPnL
		}
	}
	for _, trade := range closedTrades {
		if isSameLocalDay(parseTime(trade.ClosedAt), now) {
			total += trade.RealizedPnL
		}
	}
	return round2(total)
}

func computeWinRate(closedTrades []ClosedTrade) float64 {
	if len(closedTrades) == 0 {
		return 0
	}
	wins := 0
	for _, trade := range closedTrades {
		if trade.RealizedPnL > 0 {
			wins++
		}
	}
	return round2(float64(wins) / float64(len(closedTrades)) * 100)
}

func winningPnLs(closedTrades []ClosedTrade) []float64 {
	values := make([]float64, 0, len(closedTrades))
	for _, trade := range closedTrades {
		if trade.RealizedPnL > 0 {
			values = append(values, trade.RealizedPnL)
		}
	}
	return values
}

func losingPnLs(closedTrades []ClosedTrade) []float64 {
	values := make([]float64, 0, len(closedTrades))
	for _, trade := range closedTrades {
		if trade.RealizedPnL <= 0 {
			values = append(values, math.Abs(trade.RealizedPnL))
		}
	}
	return values
}

func computeProfitFactor(closedTrades []ClosedTrade) float64 {
	grossWin := 0.0
	grossLoss := 0.0
	for _, trade := range closedTrades {
		if trade.RealizedPnL > 0 {
			grossWin += trade.RealizedPnL
		} else if trade.RealizedPnL < 0 {
			grossLoss += math.Abs(trade.RealizedPnL)
		}
	}
	if grossLoss == 0 {
		return 0
	}
	return round2(grossWin / grossLoss)
}

func buildEquityCurve(closedTrades []ClosedTrade) []EquityCurvePoint {
	if len(closedTrades) == 0 {
		return []EquityCurvePoint{{Timestamp: time.Now().Unix(), Equity: 0, Drawdown: 0}}
	}

	trades := append([]ClosedTrade(nil), closedTrades...)
	sort.SliceStable(trades, func(i, j int) bool {
		return parseTime(trades[i].ClosedAt).Before(parseTime(trades[j].ClosedAt))
	})

	equity := 0.0
	peak := 0.0
	points := make([]EquityCurvePoint, 0, len(trades))
	for _, trade := range trades {
		equity += trade.RealizedPnL
		if equity > peak {
			peak = equity
		}
		drawdown := 0.0
		if peak > 0 {
			drawdown = ((peak - equity) / peak) * 100
		}
		points = append(points, EquityCurvePoint{
			Timestamp: parseTime(trade.ClosedAt).Unix(),
			Equity:    round2(equity),
			Drawdown:  round2(drawdown),
		})
	}
	return points
}

func computeRiskMetrics(positions []PortfolioPosition, totalEquity float64) RiskMetrics {
	if len(positions) == 0 {
		return RiskMetrics{}
	}

	totalNotional := 0.0
	maxNotional := 0.0
	volatilityInputs := make([]float64, 0, len(positions))
	for _, position := range positions {
		totalNotional += position.Notional
		if position.Notional > maxNotional {
			maxNotional = position.Notional
		}
		volatilityInputs = append(volatilityInputs, math.Abs(position.UnrealizedPnLPct))
	}

	return RiskMetrics{
		MaxConcentration:     pct(maxNotional, totalNotional),
		DiversificationScore: float64(len(positions)),
		LeverageRatio:        ratio(totalNotional, totalEquity),
		PortfolioVolatility:  average(volatilityInputs),
		StressTestLoss:       round2(totalNotional * 0.2),
	}
}

func realizedPnL(position positions.Position) float64 {
	if position.RealizedPnL != 0 {
		return round2(position.RealizedPnL)
	}
	return round2(position.PnL)
}

func symbolFromMarket(market *markets.Market, fallback string) string {
	source := fallback
	if market != nil && strings.TrimSpace(market.Title) != "" {
		source = market.Title
	}
	parts := strings.FieldsFunc(source, func(r rune) bool {
		return r == ' ' || r == '/' || r == '_' || r == '-'
	})
	if len(parts) == 0 {
		return "0"
	}
	var b strings.Builder
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			b.WriteString(strings.ToUpper(part[:1]))
		}
	}
	if b.Len() == 0 {
		return "0"
	}
	return b.String()
}

func matchName(match *matches.Match, fallback string) string {
	if match == nil {
		return fallback
	}
	return strings.TrimSpace(match.TeamAName + " vs " + match.TeamBName)
}

func formatStrike(strike float64) string {
	if strike <= 0 {
		return "-"
	}
	return strconv.FormatFloat(strike, 'f', -1, 64)
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return time.Unix(0, 0).UTC().Format(time.RFC3339)
	}
	return value.UTC().Format(time.RFC3339)
}

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func isSameLocalDay(a, b time.Time) bool {
	if a.IsZero() {
		return false
	}
	aa := a.In(time.Local)
	bb := b.In(time.Local)
	return aa.Year() == bb.Year() && aa.Month() == bb.Month() && aa.Day() == bb.Day()
}

func computeMarginUsage(used, available float64) float64 {
	base := used + available
	if base <= 0 {
		return 0
	}
	return round2((used / base) * 100)
}

func average(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	total := 0.0
	for _, value := range values {
		total += value
	}
	return round2(total / float64(len(values)))
}

func pct(value, base float64) float64 {
	if base <= 0 {
		return 0
	}
	return round2((value / base) * 100)
}

func ratio(value, base float64) float64 {
	if base <= 0 {
		return 0
	}
	return round2(value / base)
}

func round2(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return math.Round(value*100) / 100
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
