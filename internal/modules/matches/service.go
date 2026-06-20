package matches

import (
	"context"
	"errors"
	"strings"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/realtime"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

var (
	errMatchNotFound      = errors.New("match not found")
	errInvalidTransition  = errors.New("invalid match status transition")
	errMatchAlreadyLive   = errors.New("match is already live")
	errMatchNotLive       = errors.New("match is not live")
	errMatchNotLiveBall   = errors.New("match must be live to record balls")
	errInvalidBallEvent   = errors.New("invalid ball event")
	errNextBatterRequired = errors.New("next batter is required for a wicket")
	errInvalidLiveContext = errors.New("invalid live match context")
)

// EventPublisher pushes realtime WebSocket updates.
type EventPublisher interface {
	Publish(topic string, data any)
}

// normalizeExtra validates and canonicalizes the extra field. Returns the
// stored value (nil for a legal delivery) and ok=false for an invalid value.
func normalizeExtra(raw string) (*string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return nil, true
	case ExtraWide:
		v := ExtraWide
		return &v, true
	case ExtraNoBall, "no_ball", "no-ball":
		v := ExtraNoBall
		return &v, true
	default:
		return nil, false
	}
}

var legacyMatchIDMap = map[string]string{
	"1":  "0000000000000000000000aa",
	"aa": "0000000000000000000000aa",
	"2":  "0000000000000000000000bb",
	"bb": "0000000000000000000000bb",
	"3":  "0000000000000000000000cc",
	"cc": "0000000000000000000000cc",
}

type Service struct {
	repo      Repository
	events    EventRepository
	publisher EventPublisher
}

func NewService(repo Repository, events EventRepository, publisher EventPublisher) *Service {
	return &Service{repo: repo, events: events, publisher: publisher}
}

func (s *Service) GetHomeMatches(ctx context.Context) []Match {
	all := s.repo.GetAll(ctx)
	for i := range all {
		all[i].Status = NormalizeStatus(all[i].Status)
		all[i].OversText = calculateOvers(all[i].BallsLeft)
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
	match.OversText = calculateOvers(match.BallsLeft)
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

	targetScore := existing.TargetScore
	if req.TargetScore != nil {
		targetScore = *req.TargetScore
	}

	score := ScoreUpdate{
		Innings:      req.Innings,
		CurrentScore: req.CurrentScore,
		WicketsLost:  req.WicketsLost,
		BallsLeft:    req.BallsLeft,
		TargetScore:  targetScore,
		Status:       status,
	}
	if req.CurrentScore == 0 && req.WicketsLost == 0 && req.BallsLeft == 120 && existing.LiveContext != nil {
		score.LiveContext = resetLiveContext(existing.LiveContext)
	}
	match, err := s.repo.UpdateScore(ctx, objID, score)
	if err != nil || match == nil {
		return match, err
	}
	s.publishScore(match)
	return match, nil
}

func (s *Service) UpdateLiveContext(ctx context.Context, id string, req UpdateLiveContextRequest) (*Match, error) {
	objID, err := resolveMatchID(ctx, s.repo, id)
	if err != nil {
		return nil, err
	}

	existing, err := s.repo.GetByID(ctx, objID)
	if err != nil || existing == nil {
		return nil, errMatchNotFound
	}

	liveContext := LiveMatchContext{
		Striker:     req.Striker,
		NonStriker:  req.NonStriker,
		Bowler:      req.Bowler,
		Partnership: req.Partnership,
	}
	liveContext.Striker.Name = strings.TrimSpace(liveContext.Striker.Name)
	liveContext.NonStriker.Name = strings.TrimSpace(liveContext.NonStriker.Name)
	liveContext.Bowler.Name = strings.TrimSpace(liveContext.Bowler.Name)

	if !validLiveContext(liveContext) {
		return nil, errInvalidLiveContext
	}

	match, err := s.repo.UpdateScore(ctx, objID, ScoreUpdate{
		Innings:      existing.Innings,
		CurrentScore: existing.CurrentScore,
		WicketsLost:  existing.WicketsLost,
		BallsLeft:    existing.BallsLeft,
		TargetScore:  existing.TargetScore,
		Status:       existing.Status,
		LiveContext:  &liveContext,
	})
	if err != nil || match == nil {
		return match, err
	}
	s.publishScore(match)
	return match, nil
}

// RecordBall applies one ball event, persists it to the event history, updates
// match state, and publishes separate score + commentary WebSocket topics.
// A "wide"/"noball" extra does not consume a legal delivery (ballsLeft stays).
func (s *Service) RecordBall(ctx context.Context, id string, req BallEventRequest) (*Match, error) {
	extra, ok := normalizeExtra(req.Extra)
	if !ok {
		return nil, errInvalidBallEvent
	}
	maxRuns := 6
	if extra != nil {
		maxRuns = 7 // supports a six hit from a no-ball (six bat runs + one extra)
	}
	if req.Runs < 0 || req.Runs > maxRuns {
		return nil, errInvalidBallEvent
	}
	legalBall := extra == nil

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
		return nil, errMatchNotLiveBall
	}

	innings := existing.Innings
	matchID := existing.ID.Hex()

	// Ball position is derived from the persisted legal-ball count for this
	// innings, so it is exact regardless of how the aggregate score was set.
	bowled := 0
	if s.events != nil {
		if c, cErr := s.events.LegalBallCount(ctx, matchID, innings); cErr == nil {
			bowled = c
		}
	}
	over := bowled / 6
	ball := bowled%6 + 1

	currentScore := existing.CurrentScore + req.Runs
	wicketsLost := existing.WicketsLost
	ballsLeft := existing.BallsLeft
	targetScore := existing.TargetScore
	liveContext := cloneLiveContext(existing.LiveContext)
	if req.IsWicket && liveContext != nil && strings.TrimSpace(req.NextBatterName) == "" {
		return nil, errNextBatterRequired
	}

	if req.IsWicket {
		wicketsLost++
		if wicketsLost > 10 {
			wicketsLost = 10
		}
	}
	if legalBall && ballsLeft > 0 {
		ballsLeft--
	}
	if liveContext != nil {
		applyDeliveryToLiveContext(liveContext, req, extra, legalBall, bowled)
	}

	// Persist the ball before mutating aggregate state so the history is the
	// source of truth for "This over".
	if s.events != nil {
		if appendErr := s.events.AppendEvent(ctx, BallEvent{
			MatchID:   matchID,
			Innings:   innings,
			Over:      over,
			Ball:      ball,
			LegalBall: legalBall,
			Runs:      req.Runs,
			IsWicket:  req.IsWicket,
			Extra:     extra,
		}); appendErr != nil {
			return nil, appendErr
		}
	}

	match, err := s.repo.UpdateScore(ctx, objID, ScoreUpdate{
		Innings:      innings,
		CurrentScore: currentScore,
		WicketsLost:  wicketsLost,
		BallsLeft:    ballsLeft,
		TargetScore:  targetScore,
		Status:       status,
		LiveContext:  liveContext,
	})
	if err != nil || match == nil {
		return match, err
	}

	s.publishCommentary(match.ID.Hex(), req, extra)
	s.publishScore(match)
	return match, nil
}

// GetRecentEvents returns the last `limit` legal deliveries (plus interleaved
// extras) for a match's current innings, oldest → newest.
func (s *Service) GetRecentEvents(ctx context.Context, id string, limit int) ([]BallEventResponse, error) {
	if limit <= 0 {
		limit = 6
	}
	objID, err := resolveMatchID(ctx, s.repo, id)
	if err != nil {
		return nil, err
	}
	match, err := s.repo.GetByID(ctx, objID)
	if err != nil || match == nil {
		return nil, errMatchNotFound
	}
	if s.events == nil {
		return []BallEventResponse{}, nil
	}

	events, err := s.events.RecentEvents(ctx, match.ID.Hex(), match.Innings, limit)
	if err != nil {
		return nil, err
	}

	out := make([]BallEventResponse, 0, len(events))
	for _, e := range events {
		out = append(out, BallEventResponse{
			Innings:  e.Innings,
			Over:     e.Over,
			Ball:     e.Ball,
			Runs:     e.Runs,
			IsWicket: e.IsWicket,
			Extra:    e.Extra,
		})
	}
	return out, nil
}

func (s *Service) publishScore(match *Match) {
	if s.publisher == nil || match == nil {
		return
	}
	s.publisher.Publish(realtime.MatchScoreTopic(match.ID.Hex()), map[string]any{
		"matchId":      match.ID.Hex(),
		"innings":      match.Innings,
		"currentScore": match.CurrentScore,
		"wicketsLost":  match.WicketsLost,
		"ballsLeft":    match.BallsLeft,
		"targetScore":  match.TargetScore,
		"oversText":    match.OversText,
		"status":       NormalizeStatus(match.Status),
		"liveContext":  match.LiveContext,
	})
}

func validLiveContext(value LiveMatchContext) bool {
	if value.Striker.Name == "" || value.NonStriker.Name == "" || value.Bowler.Name == "" {
		return false
	}
	return value.Striker.Runs >= 0 && value.Striker.Balls >= 0 &&
		value.NonStriker.Runs >= 0 && value.NonStriker.Balls >= 0 &&
		value.Bowler.Balls >= 0 && value.Bowler.Maidens >= 0 && value.Bowler.Runs >= 0 && value.Bowler.Wickets >= 0 &&
		value.Partnership.Runs >= 0 && value.Partnership.Balls >= 0
}

func cloneLiveContext(value *LiveMatchContext) *LiveMatchContext {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func resetLiveContext(value *LiveMatchContext) *LiveMatchContext {
	if value == nil {
		return nil
	}
	return &LiveMatchContext{
		Striker:    BatterStats{Name: value.Striker.Name},
		NonStriker: BatterStats{Name: value.NonStriker.Name},
		Bowler:     BowlerStats{Name: value.Bowler.Name},
	}
}

func applyDeliveryToLiveContext(
	live *LiveMatchContext,
	req BallEventRequest,
	extra *string,
	legalBall bool,
	legalBallsBefore int,
) {
	if live == nil {
		return
	}

	batterRuns := req.Runs
	if extra != nil {
		if *extra == ExtraWide {
			batterRuns = 0
		} else if *extra == ExtraNoBall {
			batterRuns = max(0, req.Runs-1)
		}
	}

	live.Striker.Runs += batterRuns
	if legalBall {
		live.Striker.Balls++
		live.Bowler.Balls++
		live.Partnership.Balls++
	}
	live.Bowler.Runs += req.Runs
	live.Bowler.CurrentOverRuns += req.Runs
	live.Partnership.Runs += req.Runs

	if req.IsWicket {
		live.Bowler.Wickets++
		live.Striker = BatterStats{Name: strings.TrimSpace(req.NextBatterName)}
		live.Partnership = PartnershipStats{}
	} else if batterRuns%2 == 1 {
		live.Striker, live.NonStriker = live.NonStriker, live.Striker
	}

	if legalBall && (legalBallsBefore+1)%6 == 0 {
		if live.Bowler.CurrentOverRuns == 0 {
			live.Bowler.Maidens++
		}
		live.Bowler.CurrentOverRuns = 0
		live.Striker, live.NonStriker = live.NonStriker, live.Striker
	}
}

func (s *Service) publishCommentary(matchID string, req BallEventRequest, extra *string) {
	if s.publisher == nil {
		return
	}
	data := map[string]any{
		"runs":     req.Runs,
		"isWicket": req.IsWicket,
		"extra":    extra,
	}
	if req.BallNumber > 0 {
		data["ballNumber"] = req.BallNumber
	}
	if strings.TrimSpace(req.Description) != "" {
		data["description"] = req.Description
	}
	s.publisher.Publish(realtime.MatchCommentaryTopic(matchID), data)
}

// StartMatch transitions upcoming → live. Multiple matches may be live at the
// same time (real exchanges run many concurrent live games), so starting one
// does not demote any other live match.
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

	match, err := s.repo.UpdateScore(ctx, objID, ScoreUpdate{
		Innings:      existing.Innings,
		CurrentScore: existing.CurrentScore,
		WicketsLost:  existing.WicketsLost,
		BallsLeft:    existing.BallsLeft,
		TargetScore:  existing.TargetScore,
		Status:       StatusLive,
	})
	if err != nil || match == nil {
		return match, err
	}
	s.publishScore(match)
	return match, nil
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

	match, err := s.repo.UpdateScore(ctx, objID, ScoreUpdate{
		Innings:      existing.Innings,
		CurrentScore: existing.CurrentScore,
		WicketsLost:  existing.WicketsLost,
		BallsLeft:    existing.BallsLeft,
		TargetScore:  existing.TargetScore,
		Status:       StatusCompleted,
	})
	if err != nil || match == nil {
		return match, err
	}
	s.publishScore(match)
	return match, nil
}

// ReconcileOnStartup only normalizes legacy status labels (e.g. "active" →
// "upcoming"). It no longer demotes duplicate live matches: multiple concurrent
// live matches are supported and intentional.
func (s *Service) ReconcileOnStartup(ctx context.Context) error {
	return s.repo.NormalizeLegacyStatuses(ctx)
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
