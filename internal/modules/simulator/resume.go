package simulator

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

type resumePlan struct {
	freshStart bool
	skip       bool
	skipReason string
	innings    int
	cursor     int
}

// deriveResumePlan decides whether to fresh-start, resume, or skip based on the
// persisted match document and ball-event counts.
func deriveResumePlan(match *matches.Match, ds *CSVDataset, counts map[int]int, legalCounts map[int]int, autoLoop bool) resumePlan {
	if match == nil {
		return resumePlan{freshStart: true}
	}

	status := matches.NormalizeStatus(match.Status)
	if status == matches.StatusCompleted {
		if autoLoop {
			return resumePlan{freshStart: true}
		}
		return resumePlan{skip: true, skipReason: "match already completed"}
	}
	if match.Innings == 2 && match.TargetScore > 0 && match.CurrentScore >= match.TargetScore {
		if autoLoop {
			return resumePlan{freshStart: true}
		}
		return resumePlan{skip: true, skipReason: "chase target already reached"}
	}
	if match.Innings == 2 && match.BallsLeft <= 0 && counts[2] > 0 {
		if autoLoop {
			return resumePlan{freshStart: true}
		}
		return resumePlan{skip: true, skipReason: "innings 2 overs complete"}
	}

	total := counts[1] + counts[2]
	if total == 0 {
		return resumePlan{freshStart: true}
	}

	innings := match.Innings
	if innings < 1 {
		innings = 1
	}
	expectedLegal := 120 - match.BallsLeft
	if expectedLegal < 0 {
		expectedLegal = 0
	}
	if legalCounts[innings] != expectedLegal {
		return resumePlan{freshStart: true}
	}
	cursor := counts[innings]

	// Innings 1 CSV exhausted but match doc may not have flipped to innings 2 yet
	// (e.g. server stopped right after the last ball of innings 1).
	inn1 := ds.Events[1]
	if innings == 1 && len(inn1) > 0 && cursor >= len(inn1) && ds.HasInnings2 && len(ds.Events[2]) > 0 {
		innings = 2
		cursor = counts[2]
	}

	events := ds.Events[innings]
	if len(events) == 0 {
		return resumePlan{skip: true, skipReason: "no CSV events for current innings"}
	}
	if cursor >= len(events) {
		if autoLoop {
			return resumePlan{freshStart: true}
		}
		return resumePlan{skip: true, skipReason: "all CSV events already applied"}
	}

	return resumePlan{innings: innings, cursor: cursor}
}

// beginInnings2 updates the match document for the second innings without
// replaying any balls (used when resuming across an innings break).
func beginInnings2(ctx context.Context, svc MatchService, matchID string, ds *CSVDataset, firstInningsScore int) error {
	if !ds.HasInnings2 {
		return fmt.Errorf("no innings 2 config")
	}
	cfg := ds.Innings2

	targetScore := cfg.TargetScore
	if targetScore == 0 && firstInningsScore > 0 {
		targetScore = firstInningsScore + 1
	}

	if _, err := svc.UpdateMatchScore(ctx, matchID, matches.UpdateScoreRequest{
		Innings:      2,
		CurrentScore: 0,
		WicketsLost:  0,
		BallsLeft:    120,
		TargetScore:  &targetScore,
		Status:       matches.StatusLive,
	}); err != nil {
		return err
	}

	_, err := svc.UpdateLiveContext(ctx, matchID, matches.UpdateLiveContextRequest{
		Striker:     matches.BatterStats{Name: cfg.StartStriker},
		NonStriker:  matches.BatterStats{Name: cfg.StartNonStriker},
		Bowler:      matches.BowlerStats{Name: cfg.StartBowler},
		Partnership: matches.PartnershipStats{},
	})
	return err
}

// ResumeOrStart continues a CSV replay from persisted state, or starts fresh
// when no balls have been recorded yet. Unlike Start(), it does NOT reset score
// or clear match_events.
func (s *Service) ResumeOrStart(ctx context.Context, matchID string, req StartRequest) (*SimStatus, error) {
	if !s.cfg.Enabled {
		return nil, fmt.Errorf("simulator is disabled (set SIMULATOR_ENABLED=true)")
	}

	scriptName := strings.TrimSpace(req.ScriptName)
	if scriptName == "" {
		scriptName = "csk_vs_mi"
	}

	ds, err := LoadDataset(s.cfg.DataDir, scriptName)
	if err != nil {
		return nil, fmt.Errorf("load dataset %q: %w", scriptName, err)
	}
	if len(ds.Events[1]) == 0 {
		return nil, fmt.Errorf("dataset %q has no innings 1 events", scriptName)
	}
	if ds.MatchID != "" && ds.MatchID != strings.TrimSpace(matchID) {
		return nil, fmt.Errorf("dataset %q is for match %s, not %s", scriptName, ds.MatchID, matchID)
	}

	match, err := s.svc.GetMatchByID(ctx, matchID)
	if err != nil || match == nil {
		return nil, fmt.Errorf("match not found: %s", matchID)
	}

	counts := map[int]int{
		1: s.eventCount(ctx, matchID, 1),
		2: s.eventCount(ctx, matchID, 2),
	}
	legalCounts := map[int]int{
		1: s.legalBallCount(ctx, matchID, 1),
		2: s.legalBallCount(ctx, matchID, 2),
	}
	plan := deriveResumePlan(match, ds, counts, legalCounts, s.cfg.AutoLoop)

	if plan.skip {
		log.Printf("simulator[%s]: not starting (%s)", matchID, plan.skipReason)
		return &SimStatus{
			Status:       StatusCompleted,
			MatchID:      matchID,
			Innings:      match.Innings,
			Cursor:       counts[match.Innings],
			TotalEvents:  len(ds.Events[match.Innings]),
			CurrentScore: fmt.Sprintf("%d/%d", match.CurrentScore, match.WicketsLost),
			OversText:    match.OversText,
			TargetScore:  match.TargetScore,
		}, nil
	}

	if plan.freshStart {
		return s.Start(ctx, matchID, req)
	}

	// Stop any in-process worker without touching persisted match state.
	if prev, ok := s.workers.Load(matchID); ok {
		prev.(*Worker).Stop()
		s.workers.Delete(matchID)
	}

	// Sync innings-2 document if we crashed between innings.
	if plan.innings == 2 && match.Innings == 1 && len(ds.Events[1]) > 0 && counts[1] >= len(ds.Events[1]) {
		if err := beginInnings2(ctx, s.svc, matchID, ds, match.CurrentScore); err != nil {
			log.Printf("simulator[%s]: begin innings 2 on resume: %v", matchID, err)
		} else if refreshed, rErr := s.svc.GetMatchByID(ctx, matchID); rErr == nil && refreshed != nil {
			match = refreshed
		}
	}

	// Ensure match stays live while replay continues.
	if matches.NormalizeStatus(match.Status) != matches.StatusLive {
		status := matches.StatusLive
		if _, uErr := s.svc.UpdateMatchScore(ctx, matchID, matches.UpdateScoreRequest{
			Innings:      match.Innings,
			CurrentScore: match.CurrentScore,
			WicketsLost:  match.WicketsLost,
			BallsLeft:    match.BallsLeft,
			TargetScore:  &match.TargetScore,
			Status:       status,
		}); uErr != nil {
			log.Printf("simulator[%s]: set live on resume: %v", matchID, uErr)
		}
	}

	intervalSec := req.IntervalSec
	if intervalSec <= 0 {
		intervalSec = ds.Innings1.ReplayIntervalSec
	}
	if intervalSec <= 0 {
		intervalSec = s.cfg.DefaultIntervalSec
	}

	w := newWorkerResumed(
		matchID, ds, s.svc, intervalSec,
		plan.innings, plan.cursor,
		match.CurrentScore, match.WicketsLost, match.OversText, match.TargetScore,
	)
	s.attachWorker(matchID, ds, w)

	log.Printf("simulator[%s]: resumed script=%s innings=%d cursor=%d/%d score=%d/%d",
		matchID, scriptName, plan.innings, plan.cursor, len(ds.Events[plan.innings]),
		match.CurrentScore, match.WicketsLost)

	return s.statusFrom(matchID, w), nil
}

func (s *Service) eventCount(ctx context.Context, matchID string, innings int) int {
	n, err := s.svc.BallEventCount(ctx, matchID, innings)
	if err != nil {
		log.Printf("simulator[%s]: BallEventCount innings=%d: %v", matchID, innings, err)
		return 0
	}
	return n
}

func (s *Service) legalBallCount(ctx context.Context, matchID string, innings int) int {
	n, err := s.svc.LegalBallCount(ctx, matchID, innings)
	if err != nil {
		log.Printf("simulator[%s]: LegalBallCount innings=%d: %v", matchID, innings, err)
		return 0
	}
	return n
}
