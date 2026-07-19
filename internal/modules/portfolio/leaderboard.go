package portfolio

import (
	"context"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/positions"
)

type leaderboardCandidate struct {
	userID primitive.ObjectID
	name   string
	roi    float64
}

type adminPositionLister interface {
	ListAdminPositions(ctx context.Context, filter positions.PositionFilter) ([]positions.Position, error)
}

// GetLeaderboard ranks all users by portfolio totalPnLPct (same ROI shown on the dashboard).
func (s *Service) GetLeaderboard(ctx context.Context) ([]LeaderboardEntry, error) {
	if s.users == nil {
		return []LeaderboardEntry{}, nil
	}

	users, err := s.users.ListAll(ctx)
	if err != nil {
		return nil, err
	}

	if allPositions, ok, err := s.allLeaderboardPositions(ctx); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return s.buildLeaderboardFromPositions(ctx, users, allPositions)
	}

	candidates := make([]leaderboardCandidate, 0, len(users))
	for _, user := range users {
		summary, sumErr := s.GetSummary(ctx, user.ID)
		if sumErr != nil {
			continue
		}
		name := strings.TrimSpace(user.Name)
		if name == "" {
			name = strings.TrimSpace(user.Email)
		}
		roi := round2(summary.TotalPnLPct)
		candidates = append(candidates, leaderboardCandidate{userID: user.ID, name: name, roi: roi})
	}

	return rankLeaderboardCandidates(candidates), nil
}

func (s *Service) allLeaderboardPositions(ctx context.Context) ([]positions.Position, bool, error) {
	lister, ok := s.positions.(adminPositionLister)
	if !ok {
		return nil, false, nil
	}
	all, err := lister.ListAdminPositions(ctx, positions.PositionFilter{})
	return all, true, err
}

func (s *Service) buildLeaderboardFromPositions(ctx context.Context, users []auth.User, allPositions []positions.Position) ([]LeaderboardEntry, error) {
	pnlByUser := make(map[primitive.ObjectID]float64, len(users))
	for _, position := range allPositions {
		switch strings.ToLower(strings.TrimSpace(position.Status)) {
		case "open":
			// Match GetSummary: unrealized open MTM + realized slice on the open row.
			pnlByUser[position.UserID] = round2(pnlByUser[position.UserID] + position.PnL + position.RealizedPnL)
		case "closed":
			pnlByUser[position.UserID] = round2(pnlByUser[position.UserID] + realizedPnL(position))
		}
	}

	candidates := make([]leaderboardCandidate, 0, len(users))
	for _, user := range users {
		name := strings.TrimSpace(user.Name)
		if name == "" {
			name = strings.TrimSpace(user.Email)
		}

		totalPnL := round2(pnlByUser[user.ID])
		roi := 0.0
		if totalPnL != 0 {
			if s.wallets == nil {
				continue
			}
			account, err := s.wallets.GetWallet(ctx, user.ID)
			if err != nil || account == nil {
				continue
			}
			roi = round2(pct(totalPnL, account.CashBalance))
		}
		candidates = append(candidates, leaderboardCandidate{userID: user.ID, name: name, roi: roi})
	}

	return rankLeaderboardCandidates(candidates), nil
}

func rankLeaderboardCandidates(candidates []leaderboardCandidate) []LeaderboardEntry {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].roi == candidates[j].roi {
			return strings.ToLower(candidates[i].name) < strings.ToLower(candidates[j].name)
		}
		return candidates[i].roi > candidates[j].roi
	})

	out := make([]LeaderboardEntry, 0, len(candidates))
	for i, c := range candidates {
		out = append(out, LeaderboardEntry{
			Rank:    i + 1,
			Name:    c.name,
			Country: "India",
			ROI:     c.roi,
			UserID:  c.userID.Hex(),
		})
	}
	return out
}
