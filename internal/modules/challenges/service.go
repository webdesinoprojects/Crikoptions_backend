package challenges

import (
	"context"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/positions"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type PositionService interface {
	ListPositions(ctx context.Context, userID primitive.ObjectID, filter positions.PositionFilter) ([]positions.Position, error)
}

type Service struct {
	positions PositionService
}

func NewService(positions PositionService) *Service {
	return &Service{positions: positions}
}

func (s *Service) Evaluate(ctx context.Context, userID primitive.ObjectID) ([]Challenge, error) {
	allPos, err := s.positions.ListPositions(ctx, userID, positions.PositionFilter{})
	if err != nil {
		return nil, err
	}

	return EvaluatePositions(allPos), nil
}
