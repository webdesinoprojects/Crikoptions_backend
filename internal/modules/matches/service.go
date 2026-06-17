package matches

import (
	"context"
	"errors"
	"strings"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

var (
	errMatchNotFound = errors.New("match not found")
)

var legacyMatchIDMap = map[string]string{
	"1":  "0000000000000000000000aa",
	"aa": "0000000000000000000000aa",
	"2":  "0000000000000000000000bb",
	"bb": "0000000000000000000000bb",
	"3":  "0000000000000000000000cc",
	"cc": "0000000000000000000000cc",
}

// Service is the matches domain service. The handler layer passes string IDs
// (hex or legacy short form) and the service is responsible for resolving
// them to primitive.ObjectID before hitting the repository.
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
		Status:       "upcoming",
		Innings:      1,
		CurrentScore: 0,
		WicketsLost:  0,
		BallsLeft:    120,
		OversText:    "0.0",
	}
	return s.repo.Create(ctx, match)
}

func (s *Service) UpdateMatchScore(ctx context.Context, id string, req UpdateScoreRequest) (*Match, error) {
	objID, err := resolveMatchID(ctx, s.repo, id)
	if err != nil {
		return nil, err
	}
	score := ScoreUpdate{
		Innings:      req.Innings,
		CurrentScore: req.CurrentScore,
		WicketsLost:  req.WicketsLost,
		BallsLeft:    req.BallsLeft,
		Status:       NormalizeStatus(req.Status),
	}
	if score.Status == "" {
		score.Status = StatusUpcoming
	}
	return s.repo.UpdateScore(ctx, objID, score)
}

// EnsureSingleLiveMatch demotes extra live matches on startup. Keeps the
// primary demo match (CSK vs MI) when it is live; otherwise keeps the most
// recently updated live match.
func (s *Service) EnsureSingleLiveMatch(ctx context.Context) error {
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

	keepID := live[0].ID
	for _, m := range live {
		if m.ID == primaryLiveMatchID {
			keepID = m.ID
			break
		}
	}
	if keepID != primaryLiveMatchID {
		latest := live[0]
		for _, m := range live[1:] {
			if m.UpdatedAt.After(latest.UpdatedAt) {
				latest = m
			}
		}
		keepID = latest.ID
	}
	return s.repo.DemoteOtherLiveMatches(ctx, keepID)
}

// RepairDemoMatches resets the three seeded demo fixtures to their canonical state.
func (s *Service) RepairDemoMatches(ctx context.Context) error {
	return s.repo.UpsertDemoMatches(ctx, getSampleMatches())
}

// resolveMatchID turns the path-supplied ID into a primitive.ObjectID. It
// accepts hex ObjectIDs directly and falls back to matching against either
// the full hex or the last two characters of any seeded match's hex — so
// legacy short IDs like "aa" continue to work for the demo seed.
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
