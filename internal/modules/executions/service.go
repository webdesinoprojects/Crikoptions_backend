package executions

import (
	"context"
	"errors"
	"math"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Service struct {
	repo Repository
}

type PositionSummary struct {
	NetLots      int
	BuyLots      int
	SellLots     int
	BuyNotional  float64
	SellNotional float64
	AvgBuyPrice  float64
	AvgSellPrice float64
	// OpenShortNotional is the remaining sale notional for the current short
	// lot only; it excludes sells that merely closed a long position.
	OpenShortNotional float64
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) Create(ctx context.Context, exec Execution) (*Execution, error) {
	return s.repo.Create(ctx, exec)
}

func (s *Service) GetByID(ctx context.Context, id primitive.ObjectID) (*Execution, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *Service) ListUserExecutions(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, limit int64) []Execution {
	return s.repo.List(ctx, Filter{
		UserID:   userID,
		MatchID:  matchID,
		MarketID: marketID,
		Limit:    limit,
	})
}

func (s *Service) ListExecutions(ctx context.Context, filter Filter) []Execution {
	return s.repo.List(ctx, filter)
}

func (s *Service) List(ctx context.Context, filter Filter) []Execution {
	return s.repo.List(ctx, filter)
}

func (s *Service) ListWithError(ctx context.Context, filter Filter) ([]Execution, error) {
	return s.repo.ListWithError(ctx, filter)
}

func (s *Service) OpenLongQty(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) int {
	net := s.PositionSummary(ctx, userID, matchID, marketID, strike).NetLots
	if net < 0 {
		return 0
	}
	return net
}

func (s *Service) NetLots(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) int {
	return s.PositionSummary(ctx, userID, matchID, marketID, strike).NetLots
}

func (s *Service) PositionSummary(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) PositionSummary {
	execs := s.repo.List(ctx, Filter{UserID: userID, MatchID: matchID, MarketID: marketID, Limit: 500})
	return summarizePosition(execs, strike)
}

func (s *Service) PositionSummaryWithError(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) (PositionSummary, error) {
	execs, err := s.repo.ListWithError(ctx, Filter{UserID: userID, MatchID: matchID, MarketID: marketID, Limit: 10000})
	if err != nil {
		return PositionSummary{}, err
	}
	if len(execs) >= 10000 {
		return PositionSummary{}, errors.New("position execution history exceeds the safe aggregate limit")
	}
	return summarizePosition(execs, strike), nil
}

func summarizePosition(execs []Execution, strike float64) PositionSummary {
	summary := PositionSummary{}
	for _, e := range execs {
		if e.Strike != strike {
			continue
		}
		switch e.Side {
		case "buy":
			summary.BuyLots += e.Quantity
			summary.BuyNotional += e.Price * float64(e.Quantity)
		case "sell":
			summary.SellLots += e.Quantity
			summary.SellNotional += e.Price * float64(e.Quantity)
		}
	}
	// Repositories return newest-first. Replay oldest-first to isolate the
	// current short lot across partial covers, closes, and position flips.
	lots := 0
	for i := len(execs) - 1; i >= 0; i-- {
		e := execs[i]
		if e.Strike != strike || e.Quantity <= 0 {
			continue
		}
		switch e.Side {
		case "buy":
			if lots < 0 {
				shortLots := -lots
				coverQty := e.Quantity
				if coverQty > shortLots {
					coverQty = shortLots
				}
				if coverQty == shortLots {
					summary.OpenShortNotional = 0
				} else if coverQty > 0 {
					release := summary.OpenShortNotional * float64(coverQty) / float64(shortLots)
					summary.OpenShortNotional = round2(summary.OpenShortNotional - release)
				}
			}
			lots += e.Quantity
		case "sell":
			closeLongQty := 0
			if lots > 0 {
				closeLongQty = e.Quantity
				if closeLongQty > lots {
					closeLongQty = lots
				}
			}
			openShortQty := e.Quantity - closeLongQty
			if openShortQty > 0 {
				summary.OpenShortNotional = round2(summary.OpenShortNotional + e.Price*float64(openShortQty))
			}
			lots -= e.Quantity
		}
	}
	summary.NetLots = summary.BuyLots - summary.SellLots
	if summary.BuyLots > 0 {
		summary.AvgBuyPrice = round2(summary.BuyNotional / float64(summary.BuyLots))
	}
	if summary.SellLots > 0 {
		summary.AvgSellPrice = round2(summary.SellNotional / float64(summary.SellLots))
	}
	if summary.NetLots < 0 && summary.OpenShortNotional > 0 {
		summary.AvgSellPrice = round2(summary.OpenShortNotional / float64(-summary.NetLots))
	}
	return summary
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
