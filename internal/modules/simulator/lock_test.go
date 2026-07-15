package simulator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

type fakeLockStore struct {
	mu           sync.Mutex
	acquireOK    bool
	acquireCalls int
	renewCalls   int
	releaseCalls int
}

func (f *fakeLockStore) EnsureIndexes(ctx context.Context) error { return nil }

func (f *fakeLockStore) Acquire(ctx context.Context, matchID, ownerID, token string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquireCalls++
	return f.acquireOK, nil
}

func (f *fakeLockStore) Renew(ctx context.Context, matchID, ownerID, token string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewCalls++
	return true, nil
}

func (f *fakeLockStore) Release(ctx context.Context, matchID, ownerID, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls++
	return nil
}

func (f *fakeLockStore) releases() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.releaseCalls
}

func TestStartDoesNotResetWhenLockHeld(t *testing.T) {
	dataDir := t.TempDir()
	matchID := "match-1"
	writeCleanDataset(t, dataDir, "clean", matchID)

	svc := &fakeCSVMatchService{
		match:       &matches.Match{Status: matches.StatusLive, Innings: 1, CurrentScore: 42, WicketsLost: 2, BallsLeft: 90},
		eventCounts: map[int]int{1: 12},
		legalCounts: map[int]int{1: 12},
	}
	locks := &fakeLockStore{acquireOK: false}
	sim := NewService(Config{DataDir: dataDir, DefaultIntervalSec: 3600, Enabled: true}, svc)
	sim.SetLockStore(locks)

	_, err := sim.Start(context.Background(), matchID, StartRequest{ScriptName: "clean"})
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("Start error = %v, want ErrLockHeld", err)
	}
	if locks.acquireCalls != 1 {
		t.Fatalf("Acquire calls = %d, want 1", locks.acquireCalls)
	}
	if len(svc.updateReqs) != 0 {
		t.Fatalf("UpdateMatchScore calls = %d, want 0", len(svc.updateReqs))
	}
	if svc.eventCounts[1] != 12 {
		t.Fatalf("event count changed to %d, want 12", svc.eventCounts[1])
	}
}

func TestWorkerReleasesLockOnStop(t *testing.T) {
	dataDir := t.TempDir()
	matchID := "match-1"
	writeCleanDataset(t, dataDir, "clean", matchID)

	svc := &fakeCSVMatchService{
		match:       &matches.Match{Status: matches.StatusLive, Innings: 1, BallsLeft: 120},
		eventCounts: map[int]int{},
		legalCounts: map[int]int{},
	}
	locks := &fakeLockStore{acquireOK: true}
	sim := NewService(Config{DataDir: dataDir, DefaultIntervalSec: 3600, Enabled: true}, svc)
	sim.SetLockStore(locks)

	if _, err := sim.Start(context.Background(), matchID, StartRequest{ScriptName: "clean"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	value, ok := sim.workers.Load(matchID)
	if !ok {
		t.Fatalf("worker not stored")
	}
	value.(*Worker).Stop()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if locks.releases() == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Release calls = %d, want 1", locks.releases())
}
