package markets

import (
	"context"
	"errors"
	"strings"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

var (
	errMarketNotFound = errors.New("market not found")
	errInvalidMarket  = errors.New("invalid market payload")
	errInvalidStatus  = errors.New("invalid market status")
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
}

type Service struct {
	repo          Repository
	pricingConfig PricingConfig
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

func (s *Service) GetMarketsByMatchID(ctx context.Context, matchID string) []Market {
	return s.repo.GetByMatchID(ctx, normalizeLegacyMatchID(matchID))
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
	if status == "" {
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

// CalculatePrice runs the T20 option-chain engine and returns a PriceResponse
// containing buyer/seller/LTP/Open/High/Low plus the full strike chain.
//
// The shape mirrors the previous placeholder so existing frontend code keeps
// working; the optionChain + projectedS0 fields are additive.
func (s *Service) CalculatePrice(input PriceCalculationInput) (PriceResponse, error) {
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
		res := CalculateFirstInnings(pricingIn, s.pricingConfig)
		chain = res.Chain
		projectedS0 = res.S0
	case 2:
		res := CalculateSecondInnings(pricingIn, s.pricingConfig)
		chain = res.Chain
		projectedS0 = res.S0
	default:
		// Unknown innings: return an empty chain rather than failing.
		chain = []StrikePremium{}
	}

	ltp, open, high, low := AggregateChainToOHLC(chain)

	// Buyer/seller spread: 1 Rs wide around LTP (matches existing convention).
	buyer := ltp
	seller := round2(ltp + 1)
	if ltp == 0 {
		// Empty chain: fall back to whatever the market currently has cached.
		buyer, seller = 0, 0
	}

	return PriceResponse{
		BuyerPrice:  buyer,
		SellerPrice: seller,
		LTP:         ltp,
		Open:        open,
		High:        high,
		Low:         low,
		StrikeStep:  s.pricingConfig.StrikeStep,
		MaxStrike:   s.pricingConfig.MaxStrike,
		ProjectedS0: round2(projectedS0),
		OptionChain: chain,
	}, nil
}

// PriceCalculationInput is the public request body for POST /api/v1/markets/{id}/calculate-price.
//
// Innings 1: pass Innings=1, CurrentScore, WicketsLost, BallsLeft.
// Innings 2: pass Innings=2, TargetScore, CurrentScore, WicketsLost, BallsBowled.
type PriceCalculationInput struct {
	MatchID      string `json:"matchId"`
	Innings      int    `json:"innings"`
	CurrentScore int    `json:"currentScore"`
	WicketsLost  int    `json:"wicketsLost"`
	BallsLeft    int    `json:"ballsLeft"`
	BallsBowled  int    `json:"ballsBowled"`
	TargetScore  int    `json:"targetScore"`
}
