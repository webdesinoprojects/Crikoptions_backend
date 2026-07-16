package markets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

var (
	errMarketNotFound        = errors.New("market not found")
	errInvalidMarket         = errors.New("invalid market payload")
	errInvalidStatus         = errors.New("invalid market status")
	ErrFinalRevisionConflict = errors.New("final score conflicts with the stored revision")
)

var legacyMarketIDMap = map[string]string{
	"market-1": "0000000000000000000000d1",
	"market-2": "0000000000000000000000d2",
	"market-3": "0000000000000000000000d3",
	"market-4": "0000000000000000000000d4",
	"market-5": "0000000000000000000000d5",
}

var legacyMatchIDMap = map[string]string{
	"0000000000000000000000aa": "1",
	"aa":                       "1",
	"0000000000000000000000bb": "2",
	"bb":                       "2",
	"0000000000000000000000cc": "3",
	"cc":                       "3",
	"0000000000000000000000dd": "4",
	"dd":                       "4",
}

type Service struct {
	repo                         Repository
	pricingConfig                PricingConfig
	providerManualGateController ProviderManualGateController
}

type ProviderManualGateController interface {
	SetProviderManualGate(context.Context, string, primitive.ObjectID, bool) (*Market, error)
}

// ProviderInningsMarketSpec is the immutable contract identity and initial
// trading projection supplied by the live-feed worker.
type ProviderInningsMarketSpec struct {
	MatchID         string
	Innings         int
	BattingTeamName string
	Format          string
	ScheduledBalls  int
	StateVersion    int64
	TradingVersion  int64
	FeedState       string
	Blockers        []string
}

func NewService(repo Repository) *Service {
	return &Service{
		repo:          repo,
		pricingConfig: DefaultPricingConfig(),
	}
}

func NewServiceWithConfig(repo Repository, cfg PricingConfig) *Service {
	return &Service{
		repo:          repo,
		pricingConfig: cfg,
	}
}

func (s *Service) SetProviderManualGateController(controller ProviderManualGateController) {
	s.providerManualGateController = controller
}

func (s *Service) SetProviderManualBlocker(ctx context.Context, id primitive.ObjectID, blocked bool) (*Market, error) {
	return s.repo.SetProviderManualBlocker(ctx, id, blocked)
}

func (s *Service) GetMarketsByMatchID(ctx context.Context, matchID string) []Market {
	return s.repo.GetByMatchID(ctx, normalizeLegacyMatchID(matchID))
}

// ListMarketsByMatchID is the error-preserving variant for provider and
// financial workflows. HTTP compatibility callers continue to use
// GetMarketsByMatchID.
func (s *Service) ListMarketsByMatchID(ctx context.Context, matchID string) ([]Market, error) {
	matchID = normalizeLegacyMatchID(strings.TrimSpace(matchID))
	if matchID == "" {
		return nil, errInvalidMarket
	}
	return s.repo.ListByMatchID(ctx, matchID)
}

// GetProviderSettlementMarket resolves one immutable provider contract and
// preserves storage errors so settlement cannot treat a failed read as empty.
func (s *Service) GetProviderSettlementMarket(ctx context.Context, matchID string, innings int, finalRevision int64) (*Market, error) {
	matchID = normalizeLegacyMatchID(strings.TrimSpace(matchID))
	if matchID == "" || innings < 1 || innings > 2 || finalRevision <= 0 {
		return nil, errInvalidMarket
	}
	return s.repo.GetProviderSettlementMarket(ctx, matchID, innings, finalRevision)
}

// EnsureProviderInningsMarket idempotently creates an innings score contract.
// The context may be a mongo.SessionContext so this write can participate in
// the same transaction as the authoritative match projection.
func (s *Service) EnsureProviderInningsMarket(ctx context.Context, spec ProviderInningsMarketSpec) error {
	spec.MatchID = normalizeLegacyMatchID(strings.TrimSpace(spec.MatchID))
	spec.BattingTeamName = strings.TrimSpace(spec.BattingTeamName)
	spec.Format = strings.TrimSpace(spec.Format)
	spec.Blockers = normalizeBlockers(spec.Blockers)
	if spec.MatchID == "" || spec.BattingTeamName == "" || spec.Innings < 1 || spec.Innings > 2 || spec.ScheduledBalls <= 0 {
		return errInvalidMarket
	}

	strikeMax, ok := providerStrikeMax(spec.Format)
	if !ok {
		return errInvalidMarket
	}
	lifecycle := MarketLifecyclePending
	status := MarketStatusSuspended
	if strings.EqualFold(strings.TrimSpace(spec.FeedState), matches.FeedStateHealthy) && len(spec.Blockers) == 0 {
		lifecycle = MarketLifecycleOpen
		status = MarketStatusActive
	}

	return s.repo.UpsertProviderInningsMarket(ctx, Market{
		MatchID:           spec.MatchID,
		Title:             fmt.Sprintf("%s Innings %d Score", spec.BattingTeamName, spec.Innings),
		Type:              "match_depth",
		Status:            status,
		Kind:              MarketKindInningsScore,
		Innings:           spec.Innings,
		Format:            spec.Format,
		ScheduledBalls:    spec.ScheduledBalls,
		StrikeMin:         10,
		StrikeMax:         strikeMax,
		StrikeStep:        10,
		FormulaVersion:    FormulaVersionInningsScoreV1,
		Lifecycle:         lifecycle,
		Blockers:          spec.Blockers,
		MatchStateVersion: spec.StateVersion,
		TradingVersion:    spec.TradingVersion,
	})
}

// SetProviderMarketGate changes only mutable provider-market state. Final
// inputs are stored monotonically by revision for deterministic settlement.
func (s *Service) SetProviderMarketGate(ctx context.Context, matchID string, innings int, lifecycle string, blockers []string, finalScore *int, finalRevision int64) error {
	matchID = normalizeLegacyMatchID(strings.TrimSpace(matchID))
	lifecycle = strings.TrimSpace(lifecycle)
	blockers = normalizeBlockers(blockers)
	if matchID == "" || innings < 1 || innings > 2 || !isValidMarketLifecycle(lifecycle) {
		return errInvalidMarket
	}
	if finalScore != nil && (*finalScore < 0 || finalRevision <= 0) {
		return errInvalidMarket
	}
	return s.repo.UpdateProviderMarketGate(ctx, matchID, innings, lifecycle, blockers, finalScore, finalRevision)
}

// ClaimProviderSettlement freezes the final revision before any wallet write.
func (s *Service) ClaimProviderSettlement(ctx context.Context, matchID string, innings int, finalRevision int64) (bool, error) {
	matchID = normalizeLegacyMatchID(strings.TrimSpace(matchID))
	if matchID == "" || innings < 1 || innings > 2 || finalRevision <= 0 {
		return false, errInvalidMarket
	}
	return s.repo.ClaimProviderSettlement(ctx, matchID, innings, finalRevision)
}

func providerStrikeMax(format string) (float64, bool) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "t20", "t20i":
		return 250, true
	case "odi", "list a", "lista", "list_a":
		return 350, true
	default:
		return 0, false
	}
}

// GetMarketByID accepts either a full hex ObjectID or a short hex tail
// (matching the seeded fixtures), and resolves it to a primitive.ObjectID.
func (s *Service) GetMarketByID(ctx context.Context, id string) (*Market, error) {
	objID, err := resolveMarketID(ctx, s.repo, id)
	if err != nil {
		return nil, err
	}
	return s.repo.GetByID(ctx, objID)
}

// VerifyProviderMarketGate conditionally touches an open provider market. It
// must be called with the order transaction context to conflict atomically
// with a concurrent provider/admin gate update.
func (s *Service) VerifyProviderMarketGate(ctx context.Context, id string, stateVersion, tradingVersion int64) (*Market, bool, error) {
	objID, err := resolveMarketID(ctx, s.repo, id)
	if err != nil {
		return nil, false, err
	}
	return s.repo.VerifyProviderMarketGate(ctx, objID, stateVersion, tradingVersion)
}

func (s *Service) GetMarketsByIDs(ctx context.Context, ids []string) (map[string]*Market, error) {
	out := make(map[string]*Market, len(ids))
	objectIDs := make([]primitive.ObjectID, 0, len(ids))
	byHex := make(map[primitive.ObjectID]string, len(ids))

	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if mapped, ok := legacyMarketIDMap[id]; ok {
			id = mapped
		}
		objID, err := primitive.ObjectIDFromHex(id)
		if err != nil {
			market, lookupErr := s.GetMarketByID(ctx, raw)
			if lookupErr != nil {
				return nil, lookupErr
			}
			out[raw] = market
			continue
		}
		objectIDs = append(objectIDs, objID)
		byHex[objID] = raw
	}

	marketsByID, err := s.repo.GetByIDs(ctx, objectIDs)
	if err != nil {
		return nil, err
	}
	for objID, market := range marketsByID {
		marketCopy := market
		out[byHex[objID]] = &marketCopy
		out[objID.Hex()] = &marketCopy
	}
	return out, nil
}

// CreateMarket creates a new tradable market (option/auction chain) for a
// match. Used by the admin console to attach markets to any match so users can
// open its chain and place orders.
func (s *Service) CreateMarket(ctx context.Context, req CreateMarketRequest) (*Market, error) {
	matchID := normalizeLegacyMatchID(strings.TrimSpace(req.MatchID))
	title := strings.TrimSpace(req.Title)
	if matchID == "" || title == "" {
		return nil, errInvalidMarket
	}

	mtype := strings.TrimSpace(req.Type)
	if mtype == "" {
		mtype = "match_depth"
	}

	status := strings.TrimSpace(req.Status)
	kind := strings.TrimSpace(req.Kind)
	lifecycle := strings.TrimSpace(req.Lifecycle)
	formulaVersion := strings.TrimSpace(req.FormulaVersion)
	if kind == MarketKindInningsScore {
		if req.Innings < 1 || req.Innings > 2 || req.ScheduledBalls <= 0 || req.StrikeMin <= 0 || req.StrikeMax < req.StrikeMin || req.StrikeStep <= 0 {
			return nil, errInvalidMarket
		}
		if formulaVersion == "" {
			formulaVersion = FormulaVersionInningsScoreV1
		}
		if formulaVersion != FormulaVersionInningsScoreV1 {
			return nil, errInvalidMarket
		}
		if lifecycle == "" {
			lifecycle = MarketLifecyclePending
		}
		if !isValidMarketLifecycle(lifecycle) {
			return nil, errInvalidMarket
		}
		if status == "" {
			status = compatibilityStatus(lifecycle, req.Blockers)
		}
	} else if lifecycle != "" || formulaVersion != "" || req.Innings != 0 {
		return nil, errInvalidMarket
	} else if status == "" {
		status = MarketStatusActive
	}
	if !isValidMarketStatus(status) {
		return nil, errInvalidStatus
	}

	if req.BuyerPrice < 0 || req.SellerPrice < 0 || req.LTP < 0 {
		return nil, errInvalidMarket
	}

	market := Market{
		MatchID:        matchID,
		Title:          title,
		Type:           mtype,
		Status:         status,
		Kind:           kind,
		Innings:        req.Innings,
		Format:         strings.TrimSpace(req.Format),
		ScheduledBalls: req.ScheduledBalls,
		StrikeMin:      round2(req.StrikeMin),
		StrikeMax:      round2(req.StrikeMax),
		StrikeStep:     round2(req.StrikeStep),
		FormulaVersion: formulaVersion,
		Lifecycle:      lifecycle,
		Blockers:       normalizeBlockers(req.Blockers),
		BuyerPrice:     round2(req.BuyerPrice),
		SellerPrice:    round2(req.SellerPrice),
		LTP:            round2(req.LTP),
		Open:           round2(req.Open),
		High:           round2(req.High),
		Low:            round2(req.Low),
		QuantityLadder: req.QuantityLadder,
	}
	return s.repo.Create(ctx, market)
}

func isValidMarketLifecycle(lifecycle string) bool {
	switch lifecycle {
	case MarketLifecyclePending, MarketLifecycleOpen, MarketLifecycleSettling, MarketLifecycleSettled, MarketLifecycleVoid:
		return true
	default:
		return false
	}
}

func compatibilityStatus(lifecycle string, blockers []string) string {
	if lifecycle == MarketLifecycleOpen && len(normalizeBlockers(blockers)) == 0 {
		return MarketStatusActive
	}
	if lifecycle == MarketLifecycleSettled || lifecycle == MarketLifecycleVoid {
		return MarketStatusClosed
	}
	return MarketStatusSuspended
}

func normalizeBlockers(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, blocker := range in {
		blocker = strings.ToLower(strings.TrimSpace(blocker))
		if blocker == "" {
			continue
		}
		if _, ok := seen[blocker]; ok {
			continue
		}
		seen[blocker] = struct{}{}
		out = append(out, blocker)
	}
	return out
}

// SetMarketStatus suspends/resumes/closes a market. Returns errMarketNotFound
// when the id does not resolve.
func (s *Service) SetMarketStatus(ctx context.Context, id, status string) (*Market, error) {
	status = strings.TrimSpace(status)
	if !isValidMarketStatus(status) {
		return nil, errInvalidStatus
	}
	objID, err := resolveMarketID(ctx, s.repo, id)
	if err != nil {
		return nil, err
	}
	existing, err := s.repo.GetByID(ctx, objID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, errMarketNotFound
	}
	if existing.Kind == MarketKindInningsScore {
		if status == MarketStatusClosed {
			return nil, errInvalidStatus
		}
		if s.providerManualGateController != nil {
			return s.providerManualGateController.SetProviderManualGate(ctx, existing.MatchID, objID, status == MarketStatusSuspended)
		}
		updated, err := s.repo.SetProviderManualBlocker(ctx, objID, status == MarketStatusSuspended)
		if err != nil {
			return nil, err
		}
		if updated == nil {
			return nil, errMarketNotFound
		}
		return updated, nil
	}
	updated, err := s.repo.UpdateStatus(ctx, objID, status)
	if err != nil {
		return nil, err
	}
	if updated == nil {
		return nil, errMarketNotFound
	}
	return updated, nil
}

func isValidMarketStatus(status string) bool {
	switch status {
	case MarketStatusActive, MarketStatusSuspended, MarketStatusClosed:
		return true
	default:
		return false
	}
}

func resolveMarketID(ctx context.Context, repo Repository, id string) (primitive.ObjectID, error) {
	id = strings.TrimSpace(id)
	if mapped, ok := legacyMarketIDMap[id]; ok {
		id = mapped
	}
	if objID, err := primitive.ObjectIDFromHex(id); err == nil {
		return objID, nil
	}

	// Fallback: scan all markets and look for a hex tail match.
	// This is only used to keep seeded short IDs working in dev.
	all := repo.GetAll(ctx)
	for i := range all {
		h := all[i].ID.Hex()
		if h == id || strings.HasSuffix(h, id) {
			return all[i].ID, nil
		}
	}
	return primitive.ObjectID{}, errMarketNotFound
}

func normalizeLegacyMatchID(id string) string {
	id = strings.TrimSpace(id)
	if mapped, ok := legacyMatchIDMap[id]; ok {
		return mapped
	}
	return id
}

// CalculatePrice runs the option-chain engine (T20 or ODI) and returns a PriceResponse
// containing buyer/seller/LTP/Open/High/Low plus the full strike chain.
//
// The shape mirrors the previous placeholder so existing frontend code keeps
// working; the optionChain + projectedS0 fields are additive.
func (s *Service) CalculatePrice(input PriceCalculationInput) (PriceResponse, error) {
	cfg := s.pricingConfig
	if IsODIFormat(input.Format, input.BallsLeft, input.BallsBowled) {
		cfg = DefaultODIPricingConfig()
	}

	pricingIn := PricingInput{
		Innings:     input.Innings,
		Wickets:     input.WicketsLost,
		BallsLeft:   input.BallsLeft,
		BallsBowled: input.BallsBowled,
		TargetScore: input.TargetScore,
		Score:       input.CurrentScore,
	}

	var chain []StrikePremium
	var projectedS0 float64

	switch input.Innings {
	case 1:
		res := CalculateFirstInnings(pricingIn, cfg)
		chain = res.Chain
		projectedS0 = res.S0
	case 2:
		res := CalculateSecondInnings(pricingIn, cfg)
		chain = res.Chain
		projectedS0 = res.S0
	default:
		chain = []StrikePremium{}
	}

	ltp, open, high, low := AggregateChainToOHLC(chain)

	buyer := ltp
	seller := round2(ltp + 1)
	if ltp == 0 {
		buyer, seller = 0, 0
	}

	return PriceResponse{
		BuyerPrice:  buyer,
		SellerPrice: seller,
		LTP:         ltp,
		Open:        open,
		High:        high,
		Low:         low,
		StrikeStep:  cfg.StrikeStep,
		MaxStrike:   cfg.MaxStrike,
		ProjectedS0: round2(projectedS0),
		OptionChain: chain,
	}, nil
}

func (s *Service) BuildOptionChainHistory(market Market, match matches.Match, events []matches.BallEvent) (OptionChainHistoryResponse, error) {
	const defaultStep = 15 * time.Second

	innings := match.Innings
	if innings <= 0 {
		innings = 1
	}
	totalBalls := totalBallsForFormat(match.Format)
	startedAt := inferInningsStart(match, events, defaultStep)

	state := inningsHistoryState{
		innings:     innings,
		ballsLeft:   totalBalls,
		targetScore: match.TargetScore,
	}

	odiOrT20 := PricingConfigForFormat(match.Format)
	points := make([]OptionChainHistoryPoint, 0, (len(events)+1)*int(odiOrT20.MaxStrike/odiOrT20.StrikeStep))
	appendSnapshot := func(timestamp time.Time) error {
		priced, err := s.CalculatePrice(state.priceInput(match.ID.Hex(), match.Format, totalBalls))
		if err != nil {
			return err
		}
		points = append(points, optionChainHistoryPoints(market, state.score, priced, timestamp)...)
		return nil
	}

	if err := appendSnapshot(startedAt); err != nil {
		return OptionChainHistoryResponse{}, err
	}

	for i, event := range events {
		state.apply(event, totalBalls)
		timestamp := event.CreatedAt
		if timestamp.IsZero() {
			timestamp = startedAt.Add(time.Duration(i+1) * defaultStep)
		}
		if err := appendSnapshot(timestamp); err != nil {
			return OptionChainHistoryResponse{}, err
		}
	}

	return OptionChainHistoryResponse{
		MarketID:  market.ID.Hex(),
		MatchID:   match.ID.Hex(),
		Innings:   innings,
		StartedAt: startedAt.UnixMilli(),
		Points:    points,
	}, nil
}

// PriceCalculationInput is the public request body for POST /api/v1/markets/{id}/calculate-price.
//
// Innings 1: pass Innings=1, CurrentScore, WicketsLost, BallsLeft.
// Innings 2: pass Innings=2, TargetScore, CurrentScore, WicketsLost, BallsBowled.
type PriceCalculationInput struct {
	MatchID      string `json:"matchId"`
	Format       string `json:"format,omitempty"`
	Innings      int    `json:"innings"`
	CurrentScore int    `json:"currentScore"`
	WicketsLost  int    `json:"wicketsLost"`
	BallsLeft    int    `json:"ballsLeft"`
	BallsBowled  int    `json:"ballsBowled"`
	TargetScore  int    `json:"targetScore"`
}

type inningsHistoryState struct {
	innings     int
	score       int
	wickets     int
	legalBalls  int
	ballsLeft   int
	targetScore int
}

func (s *inningsHistoryState) apply(event matches.BallEvent, totalBalls int) {
	s.score += event.Runs
	if event.IsWicket {
		s.wickets++
		if s.wickets > 10 {
			s.wickets = 10
		}
	}
	if event.LegalBall {
		s.legalBalls++
	}
	s.ballsLeft = totalBalls - s.legalBalls
	if s.ballsLeft < 0 {
		s.ballsLeft = 0
	}
}

func (s inningsHistoryState) priceInput(matchID, format string, totalBalls int) PriceCalculationInput {
	input := PriceCalculationInput{
		MatchID:      matchID,
		Format:       format,
		Innings:      s.innings,
		CurrentScore: s.score,
		WicketsLost:  s.wickets,
		TargetScore:  s.targetScore,
	}
	if s.innings == 2 {
		input.BallsBowled = totalBalls - s.ballsLeft
		if input.TargetScore <= 0 {
			input.TargetScore = max(s.score+1, 1)
		}
		return input
	}
	input.BallsLeft = s.ballsLeft
	return input
}

func optionChainHistoryPoints(market Market, score int, priced PriceResponse, timestamp time.Time) []OptionChainHistoryPoint {
	if len(priced.OptionChain) == 0 {
		return nil
	}

	chain := priced.OptionChain
	atmStrike := nearestStrike(chain, chainReference(score, priced.ProjectedS0))
	itmReference := priced.ProjectedS0
	if score > 0 {
		itmReference = float64(score)
	}

	points := make([]OptionChainHistoryPoint, 0, len(chain))
	for i, item := range chain {
		bid, ask := quoteFromPremium(item.Premium)
		bidQty, askQty := ladderQuantities(market, i)
		points = append(points, OptionChainHistoryPoint{
			MarketID:  market.ID.Hex(),
			Timestamp: timestamp.UnixMilli(),
			Strike:    item.Strike,
			Premium:   item.Premium,
			Bid:       bid,
			Ask:       ask,
			BidQty:    bidQty,
			AskQty:    askQty,
			Moneyness: moneyness(item.Strike, atmStrike, itmReference),
		})
	}
	return points
}

func ladderQuantities(market Market, index int) (int, int) {
	if len(market.QuantityLadder) == 0 {
		return 0, 0
	}
	entry := market.QuantityLadder[index%len(market.QuantityLadder)]
	return entry.BuyerQty, entry.SellerQty
}

func moneyness(strike, atmStrike, reference float64) string {
	if strike == atmStrike {
		return "ATM"
	}
	if strike < reference {
		return "ITM"
	}
	return "OTM"
}

func chainReference(score int, projected float64) float64 {
	if score > 0 {
		return roundScoreToNearestStrike(float64(score))
	}
	return projected
}

func roundScoreToNearestStrike(score float64) float64 {
	if score <= 0 {
		return 0
	}
	remainder := int(score) % 10
	if remainder <= 5 {
		return float64(int(score/10) * 10)
	}
	return float64((int(score/10) + 1) * 10)
}

func nearestStrike(chain []StrikePremium, reference float64) float64 {
	if len(chain) == 0 {
		return 0
	}
	closest := chain[0].Strike
	for _, item := range chain[1:] {
		if abs(item.Strike-reference) < abs(closest-reference) {
			closest = item.Strike
		}
	}
	return closest
}

func inferInningsStart(match matches.Match, events []matches.BallEvent, step time.Duration) time.Time {
	if match.Innings <= 1 && !match.StartTime.IsZero() {
		return match.StartTime
	}
	if len(events) > 0 && !events[0].CreatedAt.IsZero() {
		return events[0].CreatedAt.Add(-step)
	}
	if !match.UpdatedAt.IsZero() {
		return match.UpdatedAt
	}
	if !match.StartTime.IsZero() {
		return match.StartTime
	}
	return time.Now().UTC()
}

func totalBallsForFormat(format string) int {
	return matches.TotalBallsForFormat(format)
}

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
