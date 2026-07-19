package positions

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

type markMarketStub struct {
	market *markets.Market
}

func (s markMarketStub) GetMarketByID(context.Context, string) (*markets.Market, error) {
	return s.market, nil
}

func (s markMarketStub) CalculatePrice(markets.PriceCalculationInput) (markets.PriceResponse, error) {
	// Chain-average LTP would be ~500 — that must NOT be used for strike 50 MTM.
	return markets.PriceResponse{
		LTP: 500,
		OptionChain: []markets.StrikePremium{
			{Strike: 10, Premium: 900},
			{Strike: 50, Premium: 238.08},
			{Strike: 100, Premium: 180},
		},
	}, nil
}

func (s markMarketStub) StrikeQuote(_ markets.PriceCalculationInput, strike float64) (bid, ask float64, ok bool) {
	if strike != 50 {
		return 0, 0, false
	}
	return 237.58, 238.58, true
}

type markMatchStub struct {
	match *matches.Match
}

func (s markMatchStub) GetMatchByID(context.Context, string) (*matches.Match, error) {
	return s.match, nil
}

func TestStrikeMarkPriceUsesBidForLongNotChainAverage(t *testing.T) {
	marketID := primitive.NewObjectID().Hex()
	matchID := primitive.NewObjectID()
	stub := markMarketStub{market: &markets.Market{
		ID: primitive.NewObjectID(), MatchID: matchID.Hex(), LTP: 500,
	}}
	svc := NewServiceWithProjection(nil, stub, NewMemoryProjectionRepository(), markMatchStub{
		match: &matches.Match{
			ID: matchID, Format: "ODI", Innings: 1, CurrentScore: 120,
			WicketsLost: 2, BallsLeft: 200, Status: matches.StatusLive,
		},
	}, stub)

	got := svc.strikeMarkPrice(context.Background(), marketID, 50, 10)
	if got != 237.58 {
		t.Fatalf("long mark = %.2f, want bid 237.58 (not chain avg 500)", got)
	}

	pos := Position{Status: "open", Lots: 10, BuyPrice: 238.24, LTP: got}
	pnl := computePnL(pos, 0)
	if pnl != -6.6 {
		t.Fatalf("pnl = %.2f, want -6.60 for buy 238.24 marked at bid 237.58 x10", pnl)
	}
}
