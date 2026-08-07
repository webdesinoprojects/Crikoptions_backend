package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/store"
)

type renewRecorder struct {
	Storage // nil: only the lease methods are exercised here
	mu      sync.Mutex
	renewals []time.Time
	fail     error
}

func (r *renewRecorder) RenewTargetLease(_ context.Context, _ int64, _, _ string, until time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.renewals = append(r.renewals, until)
	return nil
}

func (r *renewRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.renewals)
}

type testLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *testLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, format)
}

func (l *testLogger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.lines)
}

func newHeartbeatWorker(storage Storage, logger Logger, ttl time.Duration) *Worker {
	return &Worker{
		cfg:    client.Config{LeaseTTL: ttl},
		store:  storage,
		owner:  "worker-1",
		logger: logger,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// A poll that outlives the lease TTL must keep its lease, otherwise the apply
// is discarded and the fixture retries forever. This is the failure that left a
// live match frozen at "upcoming" while its innings played out.
func TestLeaseHeartbeatKeepsALongPollAlive(t *testing.T) {
	recorder := &renewRecorder{}
	worker := newHeartbeatWorker(recorder, &testLogger{}, 150*time.Millisecond) // renews every 50ms

	stop := worker.leaseHeartbeat(context.Background(), 69609, "token-1")
	time.Sleep(260 * time.Millisecond) // ~2.5 TTLs of "apply work"
	stop()

	if got := recorder.count(); got < 2 {
		t.Fatalf("renewals=%d, want at least 2 across a poll longer than the TTL", got)
	}
}

func TestLeaseHeartbeatStopsCleanly(t *testing.T) {
	recorder := &renewRecorder{}
	worker := newHeartbeatWorker(recorder, &testLogger{}, 150*time.Millisecond)

	stop := worker.leaseHeartbeat(context.Background(), 69609, "token-1")
	time.Sleep(120 * time.Millisecond)
	stop() // must block until the goroutine has exited

	settled := recorder.count()
	time.Sleep(180 * time.Millisecond)
	if got := recorder.count(); got != settled {
		t.Fatalf("renewals continued after stop: %d -> %d", settled, got)
	}
}

// Losing the lease must stop renewal and say so, never fail silently.
func TestLeaseHeartbeatSurrendersAndLogsOnLeaseLoss(t *testing.T) {
	recorder := &renewRecorder{fail: store.ErrFixtureLeaseLost}
	logger := &testLogger{}
	worker := newHeartbeatWorker(recorder, logger, 150*time.Millisecond)

	stop := worker.leaseHeartbeat(context.Background(), 69609, "token-1")
	time.Sleep(260 * time.Millisecond)
	stop()

	if logger.count() == 0 {
		t.Fatal("lease loss was not logged")
	}
}

func TestLeaseHeartbeatStopsWhenContextIsCancelled(t *testing.T) {
	recorder := &renewRecorder{}
	worker := newHeartbeatWorker(recorder, &testLogger{}, 150*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	stop := worker.leaseHeartbeat(ctx, 69609, "token-1")
	cancel()
	stop() // must not hang on a cancelled context

	// A transient (non-lease-lost) renewal error must not stop the heartbeat.
	recorder2 := &renewRecorder{fail: errors.New("transient mongo blip")}
	worker2 := newHeartbeatWorker(recorder2, &testLogger{}, 150*time.Millisecond)
	stop2 := worker2.leaseHeartbeat(context.Background(), 69609, "token-2")
	time.Sleep(180 * time.Millisecond)
	stop2()
}
