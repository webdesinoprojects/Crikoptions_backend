package matches

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	errProviderOwnedMatch = errors.New("provider-owned match cannot be mutated manually")
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
	"4":  "0000000000000000000000dd",
	"dd": "0000000000000000000000dd",
}

type Service struct {
	repo       Repository
	events     EventRepository
	publisher  EventPublisher
	settlement SettlementRunner
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
	live := make([]Match, 0, len(all))
	upcoming := make([]Match, 0, len(all))
	// Demo/simulator replays (e.g. CSK vs MI, RCB vs KKR). The fallback controller
	// only leaves these visible while no real Sportmonks match is in play, so they
	// act as a fallback that keeps the terminal populated between live fixtures.
	fallback := make([]Match, 0)
	for i := range all {
		if all[i].Hidden {
			continue
		}
		all[i].Status = NormalizeStatus(all[i].Status)
		all[i].OversText = calculateOvers(all[i].BallsLeft, all[i].Format)
		AnnotateTradable(&all[i])
		if all[i].DataSource != DataSourceSportmonks {
			// Non-provider matches are fallback demo games, surfaced below only
			// when there is no live Sportmonks fixture.
			switch all[i].Status {
			case StatusLive, StatusInningsBreak:
				fallback = append(fallback, all[i])
			}
			continue
		}
		switch all[i].Status {
		case StatusLive, StatusInningsBreak:
			live = append(live, all[i])
		case StatusUpcoming:
			upcoming = append(upcoming, all[i])
		}
	}
	// Prefer real live fixtures; when none are in play, surface the demo fallback
	// games; otherwise fall back to upcoming Sportmonks matches.
	if len(live) > 0 {
		return SortHomeMatches(live)
	}
	if len(fallback) > 0 {
		return SortHomeMatches(fallback)
	}
	return SortHomeMatches(upcoming)
}

// CountLiveProviderMatches returns how many real (Sportmonks) matches are in
// play (live or innings break) and visible on the home feed.
func (s *Service) CountLiveProviderMatches(ctx context.Context) (int, error) {
	return s.repo.CountLiveProviderMatches(ctx)
}

// ProviderMatchImminent reports whether a real (Sportmonks) match is already in
// play OR is scheduled to start within the given lead time. The fallback
// controller uses this to wind down the demo games ahead of a real fixture.
func (s *Service) ProviderMatchImminent(ctx context.Context, within time.Duration) (bool, error) {
	all := s.repo.GetAll(ctx)
	cutoff := time.Now().UTC().Add(within)
	for i := range all {
		m := all[i]
		if m.Hidden || m.DataSource != DataSourceSportmonks {
			continue
		}
		switch NormalizeStatus(m.Status) {
		case StatusLive, StatusInningsBreak:
			return true, nil
		case StatusUpcoming:
			if !m.StartTime.IsZero() && !m.StartTime.After(cutoff) {
				return true, nil
			}
		}
	}
	return false, nil
}

// SetDemoMatchesHidden toggles the hidden flag on the given demo/simulator match
// ids, so the fallback controller can reveal or hide them.
func (s *Service) SetDemoMatchesHidden(ctx context.Context, hidden bool, hexIDs ...string) error {
	ids := make([]primitive.ObjectID, 0, len(hexIDs))
	for _, hx := range hexIDs {
		id, err := primitive.ObjectIDFromHex(strings.TrimSpace(hx))
		if err != nil {
			return fmt.Errorf("invalid match id %q: %w", hx, err)
		}
		ids = append(ids, id)
	}
	return s.repo.SetHidden(ctx, hidden, ids...)
}

// GetUpcomingMatches returns Sportmonks fixtures that have not started yet,
// soonest start first. Unlike home, this never falls back to live matches.
func (s *Service) GetUpcomingMatches(ctx context.Context) []Match {
	all := s.repo.GetAll(ctx)
	upcoming := make([]Match, 0, len(all))
	for i := range all {
		if all[i].Hidden {
			continue
		}
		if all[i].DataSource != DataSourceSportmonks {
			continue
		}
		all[i].Status = NormalizeStatus(all[i].Status)
		if all[i].Status != StatusUpcoming {
			continue
		}
		all[i].OversText = calculateOvers(all[i].BallsLeft, all[i].Format)
		AnnotateTradable(&all[i])
		upcoming = append(upcoming, all[i])
	}
	return SortHomeMatches(upcoming)
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
	match.OversText = calculateOvers(match.BallsLeft, match.Format)
	AnnotateTradable(match)
	return match, nil
}

// VerifyTradingGate writes to the match document when the caller's versions
// still describe a healthy, open Sportmonks match. Callers should invoke this
// from their Mongo transaction so concurrent feed suspension forces a retry.
func (s *Service) VerifyTradingGate(ctx context.Context, id string, stateVersion, tradingVersion int64) (*Match, bool, error) {
	objID, err := resolveMatchID(ctx, s.repo, id)
	if err != nil {
		return nil, false, err
	}
	return s.repo.VerifyTradingGate(ctx, objID, stateVersion, tradingVersion)
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
		match.OversText = calculateOvers(match.BallsLeft, match.Format)
		matchCopy := match
		out[byHex[objID]] = &matchCopy
		out[objID.Hex()] = &matchCopy
	}
	return out, nil
}

func (s *Service) CreateMatch(ctx context.Context, req CreateMatchRequest) (*Match, error) {
	match := Match{
		DataSource:   DataSourceManual,
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
	if existing.DataSource == DataSourceSportmonks {
		return nil, errProviderOwnedMatch
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
	if !deferBallRealtime(ctx) {
		s.publishScore(match)
	}
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
	if existing.DataSource == DataSourceSportmonks {
		return nil, errProviderOwnedMatch
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
	match, _, err := s.recordBall(ctx, id, req)
	return match, err
}

// RecordBallDelivery is like RecordBall but also returns the persisted event.
// When ctx carries WithDeferredBallRealtime, callers must invoke PublishBallDelivery.
func (s *Service) RecordBallDelivery(ctx context.Context, id string, req BallEventRequest) (*Match, BallEvent, error) {
	return s.recordBall(ctx, id, req)
}

// PublishBallDelivery emits one score + commentary update for a recorded delivery.
func (s *Service) PublishBallDelivery(match *Match, event BallEvent, req BallEventRequest) {
	if match == nil {
		return
	}
	extra, _ := normalizeExtra(req.Extra)
	s.publishScore(match)
	s.publishCommentary(match.ID.Hex(), event, req, extra, match)
}

func (s *Service) recordBall(ctx context.Context, id string, req BallEventRequest) (*Match, BallEvent, error) {
	extra, ok := normalizeExtra(req.Extra)
	if !ok {
		return nil, BallEvent{}, errInvalidBallEvent
	}
	maxRuns := 6
	if extra != nil {
		maxRuns = 7 // supports a six hit from a no-ball (six bat runs + one extra)
	}
	if req.Runs < 0 || req.Runs > maxRuns {
		return nil, BallEvent{}, errInvalidBallEvent
	}
	legalBall := extra == nil

	objID, err := resolveMatchID(ctx, s.repo, id)
	if err != nil {
		return nil, BallEvent{}, err
	}

	existing, err := s.repo.GetByID(ctx, objID)
	if err != nil || existing == nil {
		return nil, BallEvent{}, errMatchNotFound
	}
	if existing.DataSource == DataSourceSportmonks {
		return nil, BallEvent{}, errProviderOwnedMatch
	}

	status := NormalizeStatus(existing.Status)
	if status != StatusLive && status != StatusInningsBreak {
		return nil, BallEvent{}, errMatchNotLiveBall
	}

	innings := existing.Innings
	if innings == 2 && existing.TargetScore > 0 && existing.CurrentScore >= existing.TargetScore {
		return nil, BallEvent{}, errMatchNotLiveBall
	}
	if legalBall && existing.BallsLeft <= 0 {
		return nil, BallEvent{}, errInningsOver
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
		return nil, BallEvent{}, errNextBatterRequired
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
		return match, BallEvent{}, err
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

	if !deferBallRealtime(ctx) {
		s.publishScore(match)
		s.publishCommentary(match.ID.Hex(), event, req, extra, match)
	}
	return match, event, nil
}

// GetRecentEvents returns deliveries for the current over only (plus extras in
// that over), oldest → newest — used by "This over" / recent-balls UI.
// limit is retained for API compatibility; current-over length is at most ~6 legal + extras.
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

	matchID := match.ID.Hex()
	legalCount, err := s.events.LegalBallCount(ctx, matchID, match.Innings)
	if err != nil {
		return nil, err
	}
	if legalCount == 0 {
		return []BallEventResponse{}, nil
	}

	// Fetch enough of the trail so filtering to the current over still includes
	// that over's balls when we are only a few deliveries into it.
	fetchLegal := limit
	if fetchLegal < 12 {
		fetchLegal = 12
	}
	events, err := s.events.RecentEvents(ctx, matchID, match.Innings, fetchLegal)
	if err != nil {
		return nil, err
	}

	currentOver := currentOverIndex(legalCount)
	filtered := filterEventsByOver(events, currentOver)

	return ballEventResponses(filtered), nil
}

func ballEventResponses(events []BallEvent) []BallEventResponse {
	out := make([]BallEventResponse, 0, len(events))
	for _, e := range events {
		eventID := e.ProviderEventID
		if eventID == "" {
			eventID = e.ID.Hex()
		}
		out = append(out, BallEventResponse{
			EventID:            eventID,
			Sequence:           e.Sequence,
			Revision:           e.Revision,
			Innings:            e.Innings,
			Over:               e.Over,
			Ball:               e.Ball,
			LegalBall:          e.LegalBall,
			Runs:               e.Runs,
			BatterRuns:         e.BatterRuns,
			IsWicket:           e.IsWicket,
			Extra:              e.Extra,
			Extras:             e.Extras,
			Dismissal:          e.Dismissal,
			StrikerName:        e.StrikerName,
			BowlerName:         e.BowlerName,
			Commentary:         e.Commentary,
			ProviderModifiedAt: e.ProviderUpdatedAt,
			ReceivedAt:         e.ReceivedAt,
			IsCorrection:       e.Revision > 1,
			Tombstoned:         e.Tombstoned,
			Superseded:         e.Tombstoned || (e.Provider != "" && !e.Active),
		})
	}
	return out
}

// currentOverIndex is the 0-based over that contains the latest legal delivery.
// After 3 balls → 0; after 6 → 0 (over just completed); after 7 → 1.
func currentOverIndex(legalBalls int) int {
	if legalBalls <= 0 {
		return 0
	}
	return (legalBalls - 1) / 6
}

func filterEventsByOver(events []BallEvent, over int) []BallEvent {
	out := make([]BallEvent, 0, 8)
	for _, e := range events {
		if e.Over == over {
			out = append(out, e)
		}
	}
	return out
}

func (s *Service) GetInningsEvents(ctx context.Context, id string, innings, limit int) ([]BallEvent, error) {
	if limit <= 0 {
		limit = 240
	}
	objID, err := resolveMatchID(ctx, s.repo, id)
	if err != nil {
		return nil, err
	}
	match, err := s.repo.GetByID(ctx, objID)
	if err != nil || match == nil {
		return nil, errMatchNotFound
	}
	if innings <= 0 {
		innings = match.Innings
	}
	if s.events == nil {
		return []BallEvent{}, nil
	}
	return s.events.InningsEvents(ctx, match.ID.Hex(), innings, limit)
}

func (s *Service) GetInningsEventsAfter(ctx context.Context, id string, innings int, afterSequence int64, limit int) ([]BallEventResponse, error) {
	objID, err := resolveMatchID(ctx, s.repo, id)
	if err != nil {
		return nil, err
	}
	match, err := s.repo.GetByID(ctx, objID)
	if err != nil || match == nil {
		return nil, errMatchNotFound
	}
	if innings <= 0 {
		innings = match.Innings
	}
	if s.events == nil {
		return []BallEventResponse{}, nil
	}
	repository, ok := s.events.(CursorEventRepository)
	if !ok {
		return nil, errors.New("sequence cursor is unavailable")
	}
	events, err := repository.InningsEventsAfter(ctx, match.ID.Hex(), innings, afterSequence, limit)
	if err != nil {
		return nil, err
	}
	return ballEventResponses(events), nil
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
	match, err := s.repo.GetByID(ctx, objID)
	if err != nil {
		return err
	}
	if match != nil && match.DataSource == DataSourceSportmonks {
		return errProviderOwnedMatch
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
	payload := map[string]any{
		"matchId":      match.ID.Hex(),
		"innings":      match.Innings,
		"currentScore": match.CurrentScore,
		"wicketsLost":  match.WicketsLost,
		"ballsLeft":    match.BallsLeft,
		"targetScore":  match.TargetScore,
		"oversText":    match.OversText,
		"status":       NormalizeStatus(match.Status),
		"liveContext":  match.LiveContext,
	}
	if match.MatchPulse != nil {
		payload["matchPulse"] = match.MatchPulse
	}
	if len(match.ThisOver) > 0 {
		payload["thisOver"] = match.ThisOver
	} else if balls := s.currentOverBallsPayload(context.Background(), match); balls != nil {
		payload["thisOver"] = balls
	}
	s.publisher.Publish(realtime.MatchScoreTopic(match.ID.Hex()), payload)
}

// currentOverBallsPayload builds the balls for the active over (same rules as GetRecentEvents).
func (s *Service) currentOverBallsPayload(ctx context.Context, match *Match) []map[string]any {
	if s.events == nil || match == nil {
		return nil
	}
	legalCount, err := s.events.LegalBallCount(ctx, match.ID.Hex(), match.Innings)
	if err != nil || legalCount <= 0 {
		return nil
	}
	events, err := s.events.RecentEvents(ctx, match.ID.Hex(), match.Innings, 12)
	if err != nil {
		return nil
	}
	filtered := filterEventsByOver(events, currentOverIndex(legalCount))
	out := make([]map[string]any, 0, len(filtered))
	for _, e := range filtered {
		item := map[string]any{
			"over":      e.Over,
			"ball":      e.Ball,
			"runs":      e.Runs,
			"isWicket":  e.IsWicket,
			"legalBall": e.LegalBall,
		}
		if e.Extra != nil {
			item["extra"] = *e.Extra
		}
		out = append(out, item)
	}
	return out
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
	if existing.DataSource == DataSourceSportmonks {
		return nil, errProviderOwnedMatch
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
	if existing.DataSource == DataSourceSportmonks {
		return nil, errProviderOwnedMatch
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
