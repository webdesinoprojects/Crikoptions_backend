package matches

import (
	"context"
	"errors"
	"strings"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

var (
	errMatchNotFound     = errors.New("match not found")
	errInvalidTransition = errors.New("invalid match status transition")
	errMatchAlreadyLive  = errors.New("match is already live")
	errMatchNotLive      = errors.New("match is not live")
)

var legacyMatchIDMap = map[string]string{
	"1":  "0000000000000000000000aa",
	"aa": "0000000000000000000000aa",
	"2":  "0000000000000000000000bb",
	"bb": "0000000000000000000000bb",
	"3":  "0000000000000000000000cc",
	"cc": "0000000000000000000000cc",
}

type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) GetHomeMatches(ctx context.Context) []Match {
	all := s.repo.GetAll(ctx)
	for i := range all {
		all[i].Status = NormalizeStatus(all[i].Status)
	}
	return SortHomeMatches(all)
}

func (s *Service) GetMatchByID(ctx context.Context, id string) (*Match, error) {
	objID, err := resolveMatchID(ctx, s.repo, id)
	if err != nil {
		return nil, err
	}
	match, err := s.repo.GetByID(ctx, objID)
	if err != nil || match == nil {
		return match, err
	}
	match.Status = NormalizeStatus(match.Status)
	return match, nil
}

func (s *Service) CreateMatch(ctx context.Context, req CreateMatchRequest) (*Match, error) {
	match := Match{
		TournamentID: req.TournamentID,
		Format:       "T20",
		TeamAID:      req.TeamAID,
		TeamBID:      req.TeamBID,
		TeamAName:    req.TeamAName,
		TeamBName:    req.TeamBName,
		TeamALogo:    req.TeamALogo,
		TeamBLogo:    req.TeamBLogo,
		StartTime:    req.StartTime,
		Status:       StatusUpcoming,
		Innings:      1,
		CurrentScore: 0,
		WicketsLost:  0,
		BallsLeft:    120,
		OversText:    "0.0",
	}
	return s.repo.Create(ctx, match)
}

// UpdateMatchScore updates innings/score fields. Status changes only when
// status is explicitly provided in the request.
func (s *Service) UpdateMatchScore(ctx context.Context, id string, req UpdateScoreRequest) (*Match, error) {
	objID, err := resolveMatchID(ctx, s.repo, id)
	if err != nil {
		return nil, err
	}

	existing, err := s.repo.GetByID(ctx, objID)
	if err != nil || existing == nil {
		return nil, errMatchNotFound
	}

	status := NormalizeStatus(existing.Status)
	if strings.TrimSpace(req.Status) != "" {
		status = NormalizeStatus(req.Status)
	}

	score := ScoreUpdate{
		Innings:      req.Innings,
		CurrentScore: req.CurrentScore,
		WicketsLost:  req.WicketsLost,
		BallsLeft:    req.BallsLeft,
		Status:       status,
	}
	return s.repo.UpdateScore(ctx, objID, score)
}

// StartMatch transitions upcoming → live and ensures only this match stays live.
func (s *Service) StartMatch(ctx context.Context, id string) (*Match, error) {
	objID, err := resolveMatchID(ctx, s.repo, id)
	if err != nil {
		return nil, err
	}

	existing, err := s.repo.GetByID(ctx, objID)
	if err != nil || existing == nil {
		return nil, errMatchNotFound
	}

	status := NormalizeStatus(existing.Status)
	switch status {
	case StatusLive:
		return nil, errMatchAlreadyLive
	case StatusCompleted, StatusAbandoned:
		return nil, errInvalidTransition
	}

	if err := s.repo.DemoteOtherLiveMatches(ctx, objID); err != nil {
		return nil, err
	}

	return s.repo.UpdateScore(ctx, objID, ScoreUpdate{
		Innings:      existing.Innings,
		CurrentScore: existing.CurrentScore,
		WicketsLost:  existing.WicketsLost,
		BallsLeft:    existing.BallsLeft,
		Status:       StatusLive,
	})
}

// CompleteMatch transitions live → completed.
func (s *Service) CompleteMatch(ctx context.Context, id string) (*Match, error) {
	objID, err := resolveMatchID(ctx, s.repo, id)
	if err != nil {
		return nil, err
	}

	existing, err := s.repo.GetByID(ctx, objID)
	if err != nil || existing == nil {
		return nil, errMatchNotFound
	}

	status := NormalizeStatus(existing.Status)
	if status != StatusLive && status != StatusInningsBreak {
		return nil, errMatchNotLive
	}

	return s.repo.UpdateScore(ctx, objID, ScoreUpdate{
		Innings:      existing.Innings,
		CurrentScore: existing.CurrentScore,
		WicketsLost:  existing.WicketsLost,
		BallsLeft:    existing.BallsLeft,
		Status:       StatusCompleted,
	})
}

// ReconcileOnStartup normalizes legacy status labels and resolves duplicate
// live matches without resetting seed data or preferring any fixed match ID.
func (s *Service) ReconcileOnStartup(ctx context.Context) error {
	if err := s.repo.NormalizeLegacyStatuses(ctx); err != nil {
		return err
	}
	return s.ReconcileDuplicateLiveMatches(ctx)
}

// ReconcileDuplicateLiveMatches keeps the most recently updated live match and
// demotes any other live matches to upcoming.
func (s *Service) ReconcileDuplicateLiveMatches(ctx context.Context) error {
	all := s.repo.GetAll(ctx)
	var live []Match
	for _, m := range all {
		if isLiveStatus(m.Status) {
			live = append(live, m)
		}
	}
	if len(live) <= 1 {
		return nil
	}

	keep := live[0]
	for _, m := range live[1:] {
		if m.UpdatedAt.After(keep.UpdatedAt) {
			keep = m
		}
	}
	return s.repo.DemoteOtherLiveMatches(ctx, keep.ID)
}

func resolveMatchID(ctx context.Context, repo Repository, id string) (primitive.ObjectID, error) {
	id = strings.TrimSpace(id)
	if mapped, ok := legacyMatchIDMap[id]; ok {
		id = mapped
	}
	if objID, err := primitive.ObjectIDFromHex(id); err == nil {
		return objID, nil
	}

	matches := repo.GetAll(ctx)
	for i := range matches {
		h := matches[i].ID.Hex()
		if h == id || strings.HasSuffix(h, id) {
			return matches[i].ID, nil
		}
	}
	return primitive.ObjectID{}, errMatchNotFound
}
