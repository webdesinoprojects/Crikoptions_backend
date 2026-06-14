package executions

import (
	"context"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Service struct {
	repo Repository
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
	execs := s.repo.List(ctx, Filter{UserID: userID, MatchID: matchID, MarketID: marketID, Limit: 500})
	net := 0
	for _, e := range execs {
		if e.Strike != strike {
			continue
		}
		switch e.Side {
		case "buy":
			net += e.Quantity
		case "sell":
			net -= e.Quantity
		}
	}
	if net < 0 {
		return 0
	}
	return net
}
