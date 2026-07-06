package simulator

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

// WorkerStatus is the lifecycle state of a single match simulator.
type WorkerStatus string

const (
	StatusRunning   WorkerStatus = "running"
	StatusPaused    WorkerStatus = "paused"
	StatusStopped   WorkerStatus = "stopped"
	StatusCompleted WorkerStatus = "completed"
)

// WorkerSnapshot is a point-in-time read of worker state (no locks held by caller).
type WorkerSnapshot struct {
	Status      WorkerStatus
	Innings     int
	Cursor      int
	TotalEvents int
	Score       int
	Wickets     int
	OversText   string
	TargetScore int
}

// Worker drives the ball-by-ball replay loop for one match in its own goroutine.
type Worker struct {
	matchID     string
	dataset     *CSVDataset
	svc         MatchService
	intervalSec int

	// state guarded by mu
	mu          sync.Mutex
	status      WorkerStatus
	innings     int
	cursor      int
	score       int
	wickets     int
	oversText   string
	targetScore int

	// lifecycle
	doneCh   chan struct{} // closed once by stopOnce to terminate the goroutine
	stopOnce sync.Once

	// pause/resume
	pauseMu    sync.Mutex
	pausedFlag bool
	unpauseCh  chan struct{} // closed on Resume; replaced on next Pause

	// auto-loop: restart from 0/0 when the replay finishes
	loopOnComplete bool
	onRestart      func(context.Context) error
	squareOff      SquareOffPort
	lease          *LockLease
}

func newWorker(matchID string, ds *CSVDataset, svc MatchService, intervalSec int) *Worker {
	return newWorkerResumed(matchID, ds, svc, intervalSec, 1, 0, 0, 0, "0.0", 0)
}

func newWorkerResumed(
	matchID string,
	ds *CSVDataset,
	svc MatchService,
	intervalSec int,
	innings, cursor, score, wickets int,
	oversText string,
	targetScore int,
) *Worker {
	return &Worker{
		matchID:     matchID,
		dataset:     ds,
		svc:         svc,
		intervalSec: intervalSec,
		status:      StatusRunning,
		innings:     innings,
		cursor:      cursor,
		score:       score,
		wickets:     wickets,
		oversText:   oversText,
		targetScore: targetScore,
		doneCh:      make(chan struct{}),
	}
}

func (w *Worker) Snapshot() WorkerSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return WorkerSnapshot{
		Status:      w.status,
		Innings:     w.innings,
		Cursor:      w.cursor,
		TotalEvents: len(w.dataset.Events[w.innings]),
		Score:       w.score,
		Wickets:     w.wickets,
		OversText:   w.oversText,
		TargetScore: w.targetScore,
	}
}

func (w *Worker) Pause() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pauseMu.Lock()
	defer w.pauseMu.Unlock()
	if w.status == StatusRunning && !w.pausedFlag {
		w.status = StatusPaused
		w.pausedFlag = true
		w.unpauseCh = make(chan struct{})
	}
}

func (w *Worker) Resume() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pauseMu.Lock()
	defer w.pauseMu.Unlock()
	if w.pausedFlag {
		w.status = StatusRunning
		w.pausedFlag = false
		close(w.unpauseCh) // unblocks the waiting goroutine
	}
}

// Stop terminates the goroutine and marks the worker stopped.
func (w *Worker) Stop() {
	w.mu.Lock()
	if w.status != StatusCompleted {
		w.status = StatusStopped
	}
	w.mu.Unlock()

	w.stopOnce.Do(func() { close(w.doneCh) })

	// Unblock any pause wait so the goroutine can exit promptly.
	w.pauseMu.Lock()
	if w.pausedFlag {
		w.pausedFlag = false
		close(w.unpauseCh)
	}
	w.pauseMu.Unlock()
}

// Run is the goroutine entry point. Call it with `go worker.Run()`.
func (w *Worker) Run() {
	log.Printf("simulator[%s]: replay started", w.matchID)
	defer func() {
		w.mu.Lock()
		if w.status == StatusRunning {
			w.status = StatusStopped
		}
		w.mu.Unlock()
		w.lease.Release(context.Background())
		log.Printf("simulator[%s]: replay goroutine exited", w.matchID)
	}()

	var pendingBowlerChange string
	var lastMatch *matches.Match

	for {
		// Snapshot mutable state under lock.
		w.mu.Lock()
		innings := w.innings
		cursor := w.cursor
		status := w.status
		w.mu.Unlock()

		if status == StatusStopped || status == StatusCompleted {
			return
		}

		events := w.dataset.Events[innings]
		if cursor >= len(events) {
			log.Printf("simulator[%s]: exhausted events for innings %d — marking completed", w.matchID, innings)
			w.mu.Lock()
			w.status = StatusCompleted
			w.mu.Unlock()
			return
		}
		row := events[cursor]

		delay := w.deliveryDelay(row.DelaySec)

		// Sleep for the row's delay, interruptible by stop.
		if !w.sleep(delay) {
			return // stopped during sleep
		}

		// Block while paused.
		if !w.waitWhilePaused() {
			return // stopped while paused
		}

		// Re-check stop after unblocking.
		w.mu.Lock()
		st := w.status
		w.mu.Unlock()
		if st == StatusStopped || st == StatusCompleted {
			return
		}

		ctx := context.Background()
		if !w.renewLease(ctx) {
			return
		}

		// Do not bowl further if chase target already reached (innings 2).
		if innings == 2 && w.targetScore > 0 && w.score >= w.targetScore {
			if !w.completeMatch(ctx) {
				return
			}
			lastMatch = nil
			pendingBowlerChange = ""
			continue
		}

		// Apply a pending bowler change from the end of the previous over.
		if pendingBowlerChange != "" && lastMatch != nil && lastMatch.LiveContext != nil {
			lc := *lastMatch.LiveContext
			lc.Bowler = matches.BowlerStats{Name: pendingBowlerChange}
			req := matches.UpdateLiveContextRequest{
				Striker:     lc.Striker,
				NonStriker:  lc.NonStriker,
				Bowler:      lc.Bowler,
				Partnership: lc.Partnership,
			}
			if m, err := w.svc.UpdateLiveContext(ctx, w.matchID, req); err != nil {
				log.Printf("simulator[%s]: change bowler %q: %v", w.matchID, pendingBowlerChange, err)
			} else {
				lastMatch = m
			}
			pendingBowlerChange = ""
		}

		// Build and submit the ball event.
		ballReq := matches.BallEventRequest{
			Runs:           row.Runs,
			IsWicket:       row.IsWicket,
			Extra:          row.Extra,
			WicketType:     row.WicketType,
			Description:    row.Commentary,
			NextBatterName: row.NextBatterName,
		}

		m, err := w.recordCSVBall(ctx, innings, row, ballReq)
		if err != nil {
			log.Printf("simulator[%s] innings=%d seq=%d: RecordBall: %v", w.matchID, innings, row.EventSeq, err)
			if matches.TerminalBallError(err) {
				if w.handleTerminalBall(ctx, innings, err) {
					continue
				}
				return
			}
			// Non-terminal error: skip this CSV row and continue.
			w.mu.Lock()
			w.cursor++
			w.mu.Unlock()
			continue
		}

		lastMatch = m
		w.mu.Lock()
		w.score = m.CurrentScore
		w.wickets = m.WicketsLost
		w.oversText = m.OversText
		w.targetScore = m.TargetScore
		w.mu.Unlock()

		// Queue the new bowler for the first ball of the next over.
		if row.ChangeBowler != "" {
			pendingBowlerChange = row.ChangeBowler
		}

		// Advance cursor after a successful delivery.
		w.mu.Lock()
		w.cursor++
		w.mu.Unlock()

		if row.EndMatch {
			log.Printf("simulator[%s]: CSV signalled end_match", w.matchID)
			if !w.completeMatch(ctx) {
				return
			}
			lastMatch = nil
			pendingBowlerChange = ""
			continue
		}

		// Chase won (target reached on this ball).
		if innings == 2 && m.TargetScore > 0 && m.CurrentScore >= m.TargetScore {
			log.Printf("simulator[%s]: chase target %d reached (%d/%d)", w.matchID, m.TargetScore, m.CurrentScore, m.WicketsLost)
			if !w.completeMatch(ctx) {
				return
			}
			lastMatch = nil
			pendingBowlerChange = ""
			continue
		}

		// Innings 1: 20 overs done — move to innings 2 (CSV flag or ballsLeft).
		if innings == 1 && (row.EndInnings || m.BallsLeft <= 0) {
			if w.dataset.HasInnings2 && len(w.dataset.Events[2]) > 0 {
				log.Printf("simulator[%s]: innings 1 complete — transitioning to innings 2", w.matchID)
				if err := w.transitionToInnings2(ctx); err != nil {
					log.Printf("simulator[%s]: innings transition: %v — stopping", w.matchID, err)
					w.mu.Lock()
					w.status = StatusStopped
					w.mu.Unlock()
					return
				}
				lastMatch = nil
				pendingBowlerChange = ""
				continue
			}
			log.Printf("simulator[%s]: innings 1 overs complete — no innings 2 data", w.matchID)
			if !w.completeMatch(ctx) {
				return
			}
			lastMatch = nil
			pendingBowlerChange = ""
			continue
		}

		// Innings 2: 20 overs done without reaching target.
		if innings == 2 && m.BallsLeft <= 0 {
			log.Printf("simulator[%s]: innings 2 overs complete", w.matchID)
			if !w.completeMatch(ctx) {
				return
			}
			lastMatch = nil
			pendingBowlerChange = ""
			continue
		}
	}
}

func (w *Worker) recordCSVBall(ctx context.Context, innings int, row BallRow, ballReq matches.BallEventRequest) (*matches.Match, error) {
	m, err := w.svc.RecordBall(ctx, w.matchID, ballReq)
	if err != nil {
		return nil, err
	}

	before := m
	corrected, changed, err := reconcileMatchToCSVRow(ctx, w.svc, w.matchID, row, m)
	if err != nil {
		log.Printf("simulator[%s] innings=%d seq=%d: align CSV state: %v", w.matchID, innings, row.EventSeq, err)
		return m, nil
	}
	if changed {
		logCSVStateCorrection(w.matchID, innings, row, before, corrected)
		m = corrected
	}
	return m, nil
}

func (w *Worker) completeMatch(ctx context.Context) bool {
	if !w.renewLease(ctx) {
		return false
	}
	if _, err := w.svc.CompleteMatch(ctx, w.matchID); err != nil {
		log.Printf("simulator[%s]: CompleteMatch: %v", w.matchID, err)
	}

	if !w.loopOnComplete || w.onRestart == nil {
		w.mu.Lock()
		w.status = StatusCompleted
		w.mu.Unlock()
		return false
	}

	log.Printf("simulator[%s]: match complete — auto-restarting from 0/0", w.matchID)
	if err := w.onRestart(ctx); err != nil {
		log.Printf("simulator[%s]: auto-restart failed: %v", w.matchID, err)
		w.mu.Lock()
		w.status = StatusCompleted
		w.mu.Unlock()
		return false
	}

	w.mu.Lock()
	w.innings = 1
	w.cursor = 0
	w.score = 0
	w.wickets = 0
	w.oversText = "0.0"
	w.targetScore = 0
	w.status = StatusRunning
	w.mu.Unlock()
	return true
}

// handleTerminalBall reacts to innings-over / match-not-live errors.
// Returns true if the replay loop should continue (innings 1 → 2 transition).
func (w *Worker) handleTerminalBall(ctx context.Context, innings int, err error) bool {
	if matches.IsInningsOver(err) && innings == 1 && w.dataset.HasInnings2 && len(w.dataset.Events[2]) > 0 {
		log.Printf("simulator[%s]: innings 1 overs complete — transitioning to innings 2", w.matchID)
		if tErr := w.transitionToInnings2(ctx); tErr != nil {
			log.Printf("simulator[%s]: innings transition: %v — stopping", w.matchID, tErr)
			w.mu.Lock()
			w.status = StatusStopped
			w.mu.Unlock()
			return false
		}
		return true
	}
	log.Printf("simulator[%s]: stopping replay (%v)", w.matchID, err)
	if !w.completeMatch(ctx) {
		return false
	}
	return true
}

// transitionToInnings2 resets the match document for the second innings and
// updates the worker's own cursor/innings state.
func (w *Worker) transitionToInnings2(ctx context.Context) error {
	if !w.dataset.HasInnings2 || len(w.dataset.Events[2]) == 0 {
		return fmt.Errorf("no innings 2 data in dataset")
	}
	if !w.renewLease(ctx) {
		return ErrLockHeld
	}

	w.mu.Lock()
	firstInningsScore := w.score
	w.mu.Unlock()

	if w.squareOff != nil {
		if err := w.squareOff.SquareOffInnings1(ctx, w.matchID); err != nil {
			log.Printf("simulator[%s]: square-off innings 1: %v", w.matchID, err)
		}
	}

	if err := beginInnings2(ctx, w.svc, w.matchID, w.dataset, firstInningsScore); err != nil {
		return fmt.Errorf("begin innings 2: %w", err)
	}

	cfg := w.dataset.Innings2
	targetScore := cfg.TargetScore
	if targetScore == 0 {
		targetScore = firstInningsScore + 1
	}

	w.mu.Lock()
	w.innings = 2
	w.cursor = 0
	w.score = 0
	w.wickets = 0
	w.oversText = "0.0"
	w.targetScore = targetScore
	w.mu.Unlock()

	return nil
}

func (w *Worker) renewLease(ctx context.Context) bool {
	if w.lease == nil {
		return true
	}
	if err := w.lease.Renew(ctx); err != nil {
		log.Printf("simulator[%s]: lost simulator lock: %v", w.matchID, err)
		w.mu.Lock()
		w.status = StatusStopped
		w.mu.Unlock()
		return false
	}
	return true
}

// deliveryDelay returns seconds to wait before the next ball. The worker's
// intervalSec (from .env / API) overrides per-row CSV delay_sec when set.
func (w *Worker) deliveryDelay(csvDelaySec int) time.Duration {
	sec := w.intervalSec
	if sec <= 0 && csvDelaySec > 0 {
		sec = csvDelaySec
	}
	if sec <= 0 {
		sec = 15
	}
	return time.Duration(sec) * time.Second
}

// sleep sleeps for d, returning false if the worker is stopped during the wait.
func (w *Worker) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-w.doneCh:
		return false
	case <-timer.C:
		return true
	}
}

// waitWhilePaused blocks until the worker is resumed or stopped.
// Returns false if the worker was stopped.
func (w *Worker) waitWhilePaused() bool {
	for {
		w.pauseMu.Lock()
		paused := w.pausedFlag
		ch := w.unpauseCh
		w.pauseMu.Unlock()

		if !paused {
			return true
		}
		select {
		case <-w.doneCh:
			return false
		case <-ch:
			// re-check pausedFlag in next iteration
		}
	}
}
