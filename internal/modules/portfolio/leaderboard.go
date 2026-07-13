package portfolio

import (
	"context"
	"sort"
	"strings"
)

type leaderboardCandidate struct {
	name string
	roi  float64
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
		candidates = append(candidates, leaderboardCandidate{name: name, roi: roi})
	}

	sort.Slice(candidates, func(i, j int) bool {
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
		})
	}
	return out, nil
}
