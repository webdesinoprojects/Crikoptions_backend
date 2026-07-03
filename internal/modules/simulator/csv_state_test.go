package simulator

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

type fakeCSVMatchService struct {
	match       *matches.Match
	recordMatch *matches.Match

	eventCounts map[int]int
	legalCounts map[int]int

	recordCalls int
	recordReqs  []matches.BallEventRequest
	updateReqs  []matches.UpdateScoreRequest
}

func (f *fakeCSVMatchService) GetMatchByID(ctx context.Context, matchID string) (*matches.Match, error) {
	return cloneCSVTestMatch(f.match), nil
}

func (f *fakeCSVMatchService) BallEventCount(ctx context.Context, matchID string, innings int) (int, error) {
	return f.eventCounts[innings], nil
}

func (f *fakeCSVMatchService) LegalBallCount(ctx context.Context, matchID string, innings int) (int, error) {
	return f.legalCounts[innings], nil
}

func (f *fakeCSVMatchService) RecordBall(ctx context.Context, matchID string, req matches.BallEventRequest) (*matches.Match, error) {
	f.recordCalls++
	f.recordReqs = append(f.recordReqs, req)
	if f.recordMatch != nil {
		return cloneCSVTestMatch(f.recordMatch), nil
	}
	return cloneCSVTestMatch(f.match), nil
}

func (f *fakeCSVMatchService) UpdateMatchScore(ctx context.Context, id string, req matches.UpdateScoreRequest) (*matches.Match, error) {
	f.updateReqs = append(f.updateReqs, req)
	current := cloneCSVTestMatch(f.match)
	if current == nil {
		current = &matches.Match{}
	}
	status := current.Status
	if req.Status != "" {
		status = matches.NormalizeStatus(req.Status)
	}
	targetScore := current.TargetScore
	if req.TargetScore != nil {
		targetScore = *req.TargetScore
	}
	current.Innings = req.Innings
	current.CurrentScore = req.CurrentScore
	current.WicketsLost = req.WicketsLost
	current.BallsLeft = req.BallsLeft
	current.TargetScore = targetScore
	current.Status = status
	f.match = current
	return cloneCSVTestMatch(current), nil
}

func (f *fakeCSVMatchService) UpdateLiveContext(ctx context.Context, id string, req matches.UpdateLiveContextRequest) (*matches.Match, error) {
	current := cloneCSVTestMatch(f.match)
	if current == nil {
		current = &matches.Match{}
	}
	current.LiveContext = &matches.LiveMatchContext{
		Striker:     req.Striker,
		NonStriker:  req.NonStriker,
		Bowler:      req.Bowler,
		Partnership: req.Partnership,
	}
	f.match = current
	return cloneCSVTestMatch(current), nil
}

func (f *fakeCSVMatchService) CompleteMatch(ctx context.Context, id string) (*matches.Match, error) {
	current := cloneCSVTestMatch(f.match)
	if current == nil {
		current = &matches.Match{}
	}
	current.Status = matches.StatusCompleted
	f.match = current
	return cloneCSVTestMatch(current), nil
}

func (f *fakeCSVMatchService) ClearMatchEvents(ctx context.Context, matchID string) error {
	f.eventCounts = map[int]int{}
	f.legalCounts = map[int]int{}
	return nil
}

func cloneCSVTestMatch(match *matches.Match) *matches.Match {
	if match == nil {
		return nil
	}
	copy := *match
	if match.LiveContext != nil {
		live := *match.LiveContext
		copy.LiveContext = &live
	}
	return &copy
}

func TestWorkerRecordCSVBallAlignsAggregateState(t *testing.T) {
	svc := &fakeCSVMatchService{
		recordMatch: &matches.Match{
			Innings:      1,
			CurrentScore: 176,
			WicketsLost:  9,
			BallsLeft:    1,
			TargetScore:  178,
			Status:       matches.StatusLive,
		},
	}
	w := newWorkerResumed("match-1", &CSVDataset{}, svc, 1, 1, 0, 0, 0, "0.0", 178)
	row := BallRow{
		EventSeq:       120,
		Runs:           1,
		ScoreAfter:     177,
		WicketsAfter:   8,
		BallsLeftAfter: 0,
	}

	got, err := w.recordCSVBall(context.Background(), 1, row, matches.BallEventRequest{Runs: row.Runs})
	if err != nil {
		t.Fatalf("recordCSVBall: %v", err)
	}
	if svc.recordCalls != 1 {
		t.Fatalf("RecordBall calls = %d, want 1", svc.recordCalls)
	}
	if len(svc.updateReqs) != 1 {
		t.Fatalf("UpdateMatchScore calls = %d, want 1", len(svc.updateReqs))
	}
	req := svc.updateReqs[0]
	if req.CurrentScore != 177 || req.WicketsLost != 8 || req.BallsLeft != 0 {
		t.Fatalf("correction = %d/%d ballsLeft=%d, want 177/8 ballsLeft=0", req.CurrentScore, req.WicketsLost, req.BallsLeft)
	}
	if req.Innings != 1 || req.TargetScore == nil || *req.TargetScore != 178 || req.Status != matches.StatusLive {
		t.Fatalf("correction did not preserve match fields: %+v", req)
	}
	if got.CurrentScore != 177 || got.WicketsLost != 8 || got.BallsLeft != 0 {
		t.Fatalf("returned match = %d/%d ballsLeft=%d, want 177/8 ballsLeft=0", got.CurrentScore, got.WicketsLost, got.BallsLeft)
	}
}

func TestResumeOrStartAlignsPersistedAggregateToLastCSVRow(t *testing.T) {
	dataDir := t.TempDir()
	scriptName := "clean"
	matchID := "match-1"
	writeCleanDataset(t, dataDir, scriptName, matchID)

	svc := &fakeCSVMatchService{
		match: &matches.Match{
			Innings:      1,
			CurrentScore: 176,
			WicketsLost:  9,
			BallsLeft:    90,
			Status:       matches.StatusLive,
		},
		eventCounts: map[int]int{1: 1},
		legalCounts: map[int]int{1: 1},
	}
	sim := NewService(Config{
		DataDir:            dataDir,
		DefaultIntervalSec: 3600,
		Enabled:            true,
	}, svc)

	status, err := sim.ResumeOrStart(context.Background(), matchID, StartRequest{ScriptName: scriptName})
	if err != nil {
		t.Fatalf("ResumeOrStart: %v", err)
	}
	if worker, ok := sim.workers.Load(matchID); ok {
		worker.(*Worker).Stop()
	}
	if status.CurrentScore != "177/8" {
		t.Fatalf("status score = %q, want 177/8", status.CurrentScore)
	}
	if len(svc.updateReqs) != 1 {
		t.Fatalf("UpdateMatchScore calls = %d, want 1", len(svc.updateReqs))
	}
	req := svc.updateReqs[0]
	if req.CurrentScore != 177 || req.WicketsLost != 8 || req.BallsLeft != 119 {
		t.Fatalf("resume correction = %+v, want 177/8 with 119 balls left", req)
	}
	if len(svc.recordReqs) != 0 {
		t.Fatalf("RecordBall calls before resume returned = %d, want 0", len(svc.recordReqs))
	}
}

func writeCleanDataset(t *testing.T, dataDir, scriptName, matchID string) {
	t.Helper()
	dir := filepath.Join(dataDir, scriptName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	config := "match_id,innings,replay_interval_sec,start_striker,start_non_striker,start_bowler,target_score\n" +
		matchID + ",1,3600,A Batter,B Batter,A Bowler,0\n"
	if err := os.WriteFile(filepath.Join(dir, "matches_config.csv"), []byte(config), 0o644); err != nil {
		t.Fatalf("write matches_config.csv: %v", err)
	}
	events := "event_seq,innings,runs,is_wicket,extra,next_batter_name,wicket_type,delay_sec,score_after,wickets_after,commentary,end_innings,end_match,change_bowler\n" +
		"1,1,1,false,,,,15,177,8,align state,false,false,\n" +
		"2,1,0,false,,,,15,177,8,next,false,false,\n"
	if err := os.WriteFile(filepath.Join(dir, "ball_events_full_match.csv"), []byte(events), 0o644); err != nil {
		t.Fatalf("write ball_events_full_match.csv: %v", err)
	}
}
