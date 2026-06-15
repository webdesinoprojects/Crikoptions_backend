package markets

import "github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"

const (
	MarketStatusActive    = "active"
	MarketStatusSuspended = "suspended"
	MarketStatusClosed    = "closed"
)

// PricingInputFromMatch maps live match state into pricing-engine input.
func PricingInputFromMatch(match matches.Match) PriceCalculationInput {
	input := PriceCalculationInput{
		MatchID:      match.ID.Hex(),
		Innings:      match.Innings,
		CurrentScore: match.CurrentScore,
		WicketsLost:  match.WicketsLost,
	}
	if match.Innings == 2 {
		input.BallsBowled = 120 - match.BallsLeft
		if input.BallsBowled < 0 {
			input.BallsBowled = 0
		}
	} else {
		input.BallsLeft = match.BallsLeft
	}
	return input
}

// StrikeQuote returns synthetic bid/ask for a strike from the option chain.
func (s *Service) StrikeQuote(input PriceCalculationInput, strike float64) (bid, ask float64, ok bool) {
	if strike <= 0 {
		return 0, 0, false
	}

	priced, err := s.CalculatePrice(input)
	if err != nil {
		return 0, 0, false
	}

	for _, row := range priced.OptionChain {
		if row.Strike != strike {
			continue
		}
		bid, ask = quoteFromPremium(row.Premium)
		return bid, ask, true
	}
	return 0, 0, false
}

func quoteFromPremium(premium float64) (bid, ask float64) {
	spread := 0.1
	if premium >= 20 {
		spread = 1
	} else if premium >= 5 {
		spread = 0.5
	}

	halfSpread := spread / 2
	bid = round2(premium - halfSpread)
	ask = round2(premium + halfSpread)
	if bid < 0 {
		bid = 0
	}
	if ask < bid {
		ask = bid
	}
	return bid, ask
}

func (s *Service) IsTradable(market *Market) bool {
	if market == nil {
		return false
	}
	switch market.Status {
	case "", MarketStatusActive:
		return true
	default:
		return false
	}
}
