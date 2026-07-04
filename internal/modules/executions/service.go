package executions

import (
	"context"

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
	summary.NetLots = summary.BuyLots - summary.SellLots
	if summary.BuyLots > 0 {
		summary.AvgBuyPrice = round2(summary.BuyNotional / float64(summary.BuyLots))
	}
	if summary.SellLots > 0 {
		summary.AvgSellPrice = round2(summary.SellNotional / float64(summary.SellLots))
	}
	return summary
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
