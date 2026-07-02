package simulator

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

// MatchService is the subset of *matches.Service that the simulator uses.
// *matches.Service satisfies this interface.
type MatchService interface {
	GetMatchByID(ctx context.Context, matchID string) (*matches.Match, error)
	BallEventCount(ctx context.Context, matchID string, innings int) (int, error)
	LegalBallCount(ctx context.Context, matchID string, innings int) (int, error)
	RecordBall(ctx context.Context, matchID string, req matches.BallEventRequest) (*matches.Match, error)
	UpdateMatchScore(ctx context.Context, id string, req matches.UpdateScoreRequest) (*matches.Match, error)
	UpdateLiveContext(ctx context.Context, id string, req matches.UpdateLiveContextRequest) (*matches.Match, error)
	CompleteMatch(ctx context.Context, id string) (*matches.Match, error)
	ClearMatchEvents(ctx context.Context, matchID string) error
}

// Config holds simulator configuration read from environment variables.
type Config struct {
	DataDir            string
	DefaultIntervalSec int
	Enabled            bool
	AutoStart          bool
	AutoLoop           bool // restart from 0/0 when a replay finishes
}

// AutoStartSpec pairs a match hex id with its CSV script folder.
type AutoStartSpec struct {
	MatchID    string
	ScriptName string
}

// DefaultAutoStartSpecs returns the built-in replay profiles started on boot.
func DefaultAutoStartSpecs() []AutoStartSpec {
	return []AutoStartSpec{
		{MatchID: "0000000000000000000000aa", ScriptName: "csk_vs_mi"},
		{MatchID: "0000000000000000000000bb", ScriptName: "rcb_vs_kkr"},
	}
}

// LoadConfig reads SIMULATOR_* environment variables.
func LoadConfig() Config {
	dir := strings.TrimSpace(os.Getenv("SIMULATOR_DATA_DIR"))
	if dir == "" {
		dir = "./data/simulator"
	}
	interval := 15
	if v := strings.TrimSpace(os.Getenv("SIMULATOR_DEFAULT_INTERVAL_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			interval = n
		}
	}
	enabled := true
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("SIMULATOR_ENABLED"))); v == "false" || v == "0" {
		enabled = false
	}
	autoStart := enabled
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("SIMULATOR_AUTO_START"))); v == "false" || v == "0" {
		autoStart = false
	}
	autoLoop := enabled
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("SIMULATOR_AUTO_LOOP"))); v == "false" || v == "0" {
		autoLoop = false
	}
	return Config{DataDir: dir, DefaultIntervalSec: interval, Enabled: enabled, AutoStart: autoStart, AutoLoop: autoLoop}
}

// SimStatus is returned by the status and control APIs.
type SimStatus struct {
	Status       WorkerStatus `json:"status"`
	MatchID      string       `json:"matchId"`
	Innings      int          `json:"innings"`
	Cursor       int          `json:"cursor"`
	TotalEvents  int          `json:"totalEvents"`
	CurrentScore string       `json:"currentScore"`
	OversText    string       `json:"oversText"`
	TargetScore  int          `json:"targetScore"`
}

// StartRequest is the optional request body for POST .../simulator/start.
type StartRequest struct {
	ScriptName  string `json:"scriptName"`  // subfolder under DataDir, e.g. "csk_vs_mi"
	Mode        string `json:"mode"`        // "csv" only
	IntervalSec int    `json:"intervalSec"` // 0 → use CSV delay_sec
}

// Service manages replay workers across all active matches.
type Service struct {
	cfg     Config
	svc     MatchService
	workers sync.Map // map[matchID string]*Worker
}

// NewService creates a new simulator service.
func NewService(cfg Config, svc MatchService) *Service {
	return &Service{cfg: cfg, svc: svc}
}

// Start loads the CSV dataset, resets the match to a clean state, and launches
// the replay goroutine. Any previous worker for this match is stopped first.
func (s *Service) Start(ctx context.Context, matchID string, req StartRequest) (*SimStatus, error) {
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

	// Stop any existing worker for this match.
	if prev, ok := s.workers.Load(matchID); ok {
		prev.(*Worker).Stop()
		s.workers.Delete(matchID)
	}

	// Clear historical ball events so the slate is clean.
	if err := s.resetMatchForReplay(context.Background(), matchID, ds); err != nil {
		return nil, err
	}

	// Determine tick interval: request > CSV config > env default.
	intervalSec := req.IntervalSec
	if intervalSec <= 0 {
		intervalSec = ds.Innings1.ReplayIntervalSec
	}
	if intervalSec <= 0 {
		intervalSec = s.cfg.DefaultIntervalSec
	}

	w := newWorker(matchID, ds, s.svc, intervalSec)
	s.attachWorker(matchID, ds, w)

	return s.statusFrom(matchID, w), nil
}

// resetMatchForReplay clears ball history and sets the match to innings-1 0/0 live.
func (s *Service) resetMatchForReplay(ctx context.Context, matchID string, ds *CSVDataset) error {
	if err := s.svc.ClearMatchEvents(ctx, matchID); err != nil {
		log.Printf("simulator[%s]: ClearMatchEvents: %v", matchID, err)
	}
	targetZero := 0
	if _, err := s.svc.UpdateMatchScore(ctx, matchID, matches.UpdateScoreRequest{
		Innings:      1,
		CurrentScore: 0,
		WicketsLost:  0,
		BallsLeft:    120,
		TargetScore:  &targetZero,
		Status:       matches.StatusLive,
	}); err != nil {
		return fmt.Errorf("reset match score: %w", err)
	}
	i1 := ds.Innings1
	if _, err := s.svc.UpdateLiveContext(ctx, matchID, matches.UpdateLiveContextRequest{
		Striker:     matches.BatterStats{Name: i1.StartStriker},
		NonStriker:  matches.BatterStats{Name: i1.StartNonStriker},
		Bowler:      matches.BowlerStats{Name: i1.StartBowler},
		Partnership: matches.PartnershipStats{},
	}); err != nil {
		log.Printf("simulator[%s]: UpdateLiveContext innings 1: %v", matchID, err)
	}
	return nil
}

func (s *Service) attachWorker(matchID string, ds *CSVDataset, w *Worker) {
	w.loopOnComplete = s.cfg.AutoLoop
	w.onRestart = func(ctx context.Context) error {
		return s.resetMatchForReplay(ctx, matchID, ds)
	}
	s.workers.Store(matchID, w)
	go w.Run()
}

// Pause suspends the running worker (cursor is preserved).
func (s *Service) Pause(matchID string) (*SimStatus, error) {
	w, err := s.mustWorker(matchID)
	if err != nil {
		return nil, err
	}
	w.Pause()
	return s.statusFrom(matchID, w), nil
}

// Resume unpauses a paused worker.
func (s *Service) Resume(matchID string) (*SimStatus, error) {
	w, err := s.mustWorker(matchID)
	if err != nil {
		return nil, err
	}
	w.Resume()
	return s.statusFrom(matchID, w), nil
}

// Reset stops the worker and clears match state back to 0/0.
func (s *Service) Reset(ctx context.Context, matchID string) (*SimStatus, error) {
	if prev, ok := s.workers.Load(matchID); ok {
		prev.(*Worker).Stop()
		s.workers.Delete(matchID)
	}

	if err := s.svc.ClearMatchEvents(ctx, matchID); err != nil {
		log.Printf("simulator[%s]: Reset ClearMatchEvents: %v", matchID, err)
	}
	targetZero := 0
	_, _ = s.svc.UpdateMatchScore(ctx, matchID, matches.UpdateScoreRequest{
		Innings:      1,
		CurrentScore: 0,
		WicketsLost:  0,
		BallsLeft:    120,
		TargetScore:  &targetZero,
		Status:       "live",
	})

	return &SimStatus{
		Status:  StatusStopped,
		MatchID: matchID,
		Innings: 1,
	}, nil
}

// Status returns the current state of the simulator for a match.
func (s *Service) Status(matchID string) *SimStatus {
	w, ok := s.workers.Load(matchID)
	if !ok {
		return &SimStatus{Status: StatusStopped, MatchID: matchID}
	}
	return s.statusFrom(matchID, w.(*Worker))
}

// Shutdown stops all active workers gracefully (call on server shutdown).
func (s *Service) Shutdown() {
	s.workers.Range(func(_, v any) bool {
		v.(*Worker).Stop()
		return true
	})
}

// AutoStartOnBoot launches the default CSV replays (CSK vs MI + RCB vs KKR) when
// SIMULATOR_ENABLED and SIMULATOR_AUTO_START are true.
func (s *Service) AutoStartOnBoot(ctx context.Context) {
	if !s.cfg.Enabled || !s.cfg.AutoStart {
		return
	}
	for _, spec := range DefaultAutoStartSpecs() {
		status, err := s.ResumeOrStart(ctx, spec.MatchID, StartRequest{ScriptName: spec.ScriptName})
		if err != nil {
			log.Printf("simulator auto-start %s (%s): %v", spec.MatchID, spec.ScriptName, err)
			continue
		}
		log.Printf("simulator auto-start %s script=%s status=%s cursor=%d/%d score=%s",
			spec.MatchID, spec.ScriptName, status.Status, status.Cursor, status.TotalEvents, status.CurrentScore)
	}
}

func (s *Service) mustWorker(matchID string) (*Worker, error) {
	w, ok := s.workers.Load(matchID)
	if !ok {
		return nil, fmt.Errorf("no simulator running for match %s", matchID)
	}
	return w.(*Worker), nil
}

func (s *Service) statusFrom(matchID string, w *Worker) *SimStatus {
	snap := w.Snapshot()
	return &SimStatus{
		Status:       snap.Status,
		MatchID:      matchID,
		Innings:      snap.Innings,
		Cursor:       snap.Cursor,
		TotalEvents:  snap.TotalEvents,
		CurrentScore: fmt.Sprintf("%d/%d", snap.Score, snap.Wickets),
		OversText:    snap.OversText,
		TargetScore:  snap.TargetScore,
	}
}
