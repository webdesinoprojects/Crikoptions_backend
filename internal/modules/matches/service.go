package matches

import (
	"context"
	"errors"
	"fmt"
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
	errInningsOver        = errors.New("innings over: no balls left")
)

// TerminalBallError reports whether a ball cannot be applied because the innings
// or match is already finished (simulator should stop, not skip ahead).
func TerminalBallError(err error) bool {
	return errors.Is(err, errMatchNotLiveBall) || errors.Is(err, errInningsOver)
}

// IsInningsOver reports that all legal deliveries for the current innings are done.
func IsInningsOver(err error) bool {
	return errors.Is(err, errInningsOver)
}

// EventPublisher pushes realtime WebSocket updates.
type EventPublisher interface {
	Publish(topic string, data any)
}

// SettlementRunner force-closes open positions when an innings or match ends.
type SettlementRunner interface {
	SquareOffInnings1(ctx context.Context, matchID string) error
	SquareOffMatch(ctx context.Context, matchID string) error
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
	repo        Repository
	events      EventRepository
	publisher   EventPublisher
	settlement  SettlementRunner
}

func NewService(repo Repository, events EventRepository, publisher EventPublisher) *Service {
	return &Service{repo: repo, events: events, publisher: publisher}
}

// SetSettlement wires auto square-off when innings or the match ends.
func (s *Service) SetSettlement(runner SettlementRunner) {
	s.settlement = runner
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

func (s *Service) GetMatchesByIDs(ctx context.Context, ids []string) (map[string]*Match, error) {
	out := make(map[string]*Match, len(ids))
	objectIDs := make([]primitive.ObjectID, 0, len(ids))
	byHex := make(map[primitive.ObjectID]string, len(ids))

	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if mapped, ok := legacyMatchIDMap[id]; ok {
			id = mapped
		}
		objID, err := primitive.ObjectIDFromHex(id)
		if err != nil {
			match, lookupErr := s.GetMatchByID(ctx, raw)
			if lookupErr != nil {
				return nil, lookupErr
			}
			out[raw] = match
			continue
		}
		objectIDs = append(objectIDs, objID)
		byHex[objID] = raw
	}

	matchesByID, err := s.repo.GetByIDs(ctx, objectIDs)
	if err != nil {
		return nil, err
	}
	for objID, match := range matchesByID {
		match.Status = NormalizeStatus(match.Status)
		match.OversText = calculateOvers(match.BallsLeft)
		matchCopy := match
		out[byHex[objID]] = &matchCopy
		out[objID.Hex()] = &matchCopy
	}
	return out, nil
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
	if innings == 2 && existing.TargetScore > 0 && existing.CurrentScore >= existing.TargetScore {
		return nil, errMatchNotLiveBall
	}
	if legalBall && existing.BallsLeft <= 0 {
		return nil, errInningsOver
	}

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

	// Capture pre-delivery player names for event history (before rotate/wicket).
	strikerName := ""
	bowlerName := ""
	if liveContext != nil {
		strikerName = liveContext.Striker.Name
		bowlerName = liveContext.Bowler.Name
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

	event := BallEvent{
		MatchID:     matchID,
		Innings:     innings,
		Over:        over,
		Ball:        ball,
		LegalBall:   legalBall,
		Runs:        req.Runs,
		IsWicket:    req.IsWicket,
		Extra:       extra,
		StrikerName: strikerName,
		BowlerName:  bowlerName,
		Commentary:  req.Description,
	}

	// Persist the ball AFTER mutating aggregate state so the critical match state
	// update succeeds first. This prevents dangling history events if the score
	// update were to fail, while still making history available before realtime
	// clients receive the matching score frame.
	if s.events != nil {
		_ = s.events.AppendEvent(ctx, event)
	}

	s.publishScore(match)
	s.publishCommentary(match.ID.Hex(), event, req, extra, match)
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
			Innings:     e.Innings,
			Over:        e.Over,
			Ball:        e.Ball,
			Runs:        e.Runs,
			IsWicket:    e.IsWicket,
			Extra:       e.Extra,
			StrikerName: e.StrikerName,
			BowlerName:  e.BowlerName,
			Commentary:  e.Commentary,
		})
	}
	return out, nil
}

// ClearMatchEvents deletes all persisted ball events for the given match.
// Used by the simulator on start/reset to ensure a clean slate.
func (s *Service) ClearMatchEvents(ctx context.Context, matchID string) error {
	if s.events == nil {
		return nil
	}
	objID, err := resolveMatchID(ctx, s.repo, matchID)
	if err != nil {
		return err
	}
	return s.events.DeleteByMatchID(ctx, objID.Hex())
}

// BallEventCount returns how many deliveries were persisted for a match innings.
func (s *Service) BallEventCount(ctx context.Context, matchID string, innings int) (int, error) {
	if s.events == nil {
		return 0, nil
	}
	objID, err := resolveMatchID(ctx, s.repo, matchID)
	if err != nil {
		return 0, err
	}
	return s.events.EventCount(ctx, objID.Hex(), innings)
}

// LegalBallCount returns how many legal deliveries were persisted for a match innings.
func (s *Service) LegalBallCount(ctx context.Context, matchID string, innings int) (int, error) {
	if s.events == nil {
		return 0, nil
	}
	objID, err := resolveMatchID(ctx, s.repo, matchID)
	if err != nil {
		return 0, err
	}
	return s.events.LegalBallCount(ctx, objID.Hex(), innings)
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
		switch *extra {
		case ExtraWide:
			batterRuns = 0
		case ExtraNoBall:
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

func (s *Service) publishCommentary(matchID string, event BallEvent, req BallEventRequest, extra *string, match *Match) {
	if s.publisher == nil {
		return
	}
	data := map[string]any{
		"matchId":      matchID,
		"innings":      event.Innings,
		"over":         event.Over,
		"ball":         event.Ball,
		"legalBall":    event.LegalBall,
		"runs":         req.Runs,
		"isWicket":     req.IsWicket,
		"wicketType":   req.WicketType,
		"extra":        extra,
		"currentScore": match.CurrentScore,
		"wicketsLost":  match.WicketsLost,
		"ballsLeft":    match.BallsLeft,
		"targetScore":  match.TargetScore,
		"oversText":    match.OversText,
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

	if s.settlement != nil {
		if err := s.settlement.SquareOffMatch(ctx, id); err != nil {
			return nil, fmt.Errorf("square off match positions: %w", err)
		}
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
