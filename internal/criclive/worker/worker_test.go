package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/reconcile"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/store"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

func TestAdaptivePollingIsOptInAndBounded(t *testing.T) {
	cfg := client.Config{MinPollInterval: 5 * time.Second, MaxPollInterval: 15 * time.Second}
	if got := adaptivePollInterval(1, cfg); got != 15*time.Second {
		t.Fatalf("safe default interval=%s", got)
	}
	cfg.FastPollingEnabled = true
	tests := []struct {
		active int
		want   time.Duration
	}{{1, 5 * time.Second}, {2, 5 * time.Second}, {3, 5 * time.Second}, {4, 5 * time.Second}, {5, 15 * time.Second}, {8, 15 * time.Second}}
	for _, test := range tests {
		if got := adaptivePollInterval(test.active, cfg); got != test.want {
			t.Fatalf("active=%d interval=%s want=%s", test.active, got, test.want)
		}
	}
	cfg.FastPollingEnabled = false
	cfg.Mode = client.ModeLive
	if got := adaptivePollInterval(1, cfg); got != 5*time.Second {
		t.Fatalf("live mode must fast-poll even when flag is off, got %s", got)
	}
}

func TestNewFixtureBudgetRetainsOpenMatches(t *testing.T) {
	for open, want := range map[int]int{-1: 6, 0: 6, 4: 2, 6: 0, 8: 0} {
		if got := newFixtureBudget(open); got != want {
			t.Fatalf("open=%d budget=%d want=%d", open, got, want)
		}
	}
}

func TestShadowSuccessDoesNotBypassLiveAdmissionBudget(t *testing.T) {
	now := time.Now().UTC()
	target := store.FixtureTarget{LastSuccessAt: &now, LastSuccessMode: string(client.ModeShadow)}
	if targetOpenInMode(target, client.ModeLive) {
		t.Fatal("shadow success was treated as an already-open live fixture")
	}
	if !targetOpenInMode(target, client.ModeShadow) {
		t.Fatal("shadow success was not retained in shadow mode")
	}
}

func TestFixtureLeagueKeyIsOrderIndependent(t *testing.T) {
	worker := &Worker{}
	worker.setFixtureLeagues([]int64{9, 2, 5})
	if worker.fixtureLeaguesChanged([]int64{5, 9, 2}) {
		t.Fatal("league ordering must not trigger a catalog resync")
	}
	if !worker.fixtureLeaguesChanged([]int64{5, 9, 3}) {
		t.Fatal("allowlist changes must trigger an immediate catalog resync")
	}
}

func TestQuotaWindowKeepsReserve(t *testing.T) {
	window := newQuotaWindow(10, 20)
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		if !window.take("fixtures", now.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("request %d unexpectedly blocked", i)
		}
	}
	if window.take("fixtures", now.Add(9*time.Second)) {
		t.Fatal("ninth request should be held as quota reserve")
	}
	if !window.take("livescores", now.Add(9*time.Second)) {
		t.Fatal("one endpoint exhausted another endpoint's quota")
	}
	if !window.take("fixtures", now.Add(time.Hour+time.Second)) {
		t.Fatal("sliding window did not recover")
	}
}

func TestProjectionIntervals(t *testing.T) {
	cfg := client.Config{
		PreMatchInterval: 2 * time.Minute, BreakInterval: time.Minute,
		FinalizingInterval: 15 * time.Second,
	}
	active := 10 * time.Second
	tests := []struct {
		status string
		feed   string
		want   time.Duration
	}{
		{matches.StatusLive, matches.FeedStateHealthy, active},
		{matches.StatusInningsBreak, matches.FeedStateHealthy, time.Minute},
		{matches.StatusCompleted, matches.FeedStateFinalizing, 15 * time.Second},
		{matches.StatusCompleted, matches.FeedStateTerminal, store.TerminalFixtureRecheck},
		{matches.StatusUpcoming, matches.FeedStateWarming, 2 * time.Minute},
	}
	for _, test := range tests {
		got := intervalForProjection(reconcile.Projection{Status: test.status}, store.ApplyResult{FeedState: test.feed}, active, cfg)
		if got != test.want {
			t.Fatalf("status/feed=%s/%s interval=%s want=%s", test.status, test.feed, got, test.want)
		}
	}
	finalizingInnings := reconcile.Projection{
		Status: matches.StatusInningsBreak, CurrentInnings: 1,
		Innings: []reconcile.Innings{{Number: 1, Complete: true}},
	}
	if got := intervalForProjection(finalizingInnings, store.ApplyResult{FeedState: matches.FeedStateHealthy}, active, cfg); got != cfg.FinalizingInterval {
		t.Fatalf("completed innings interval=%s want=%s", got, cfg.FinalizingInterval)
	}
}

func TestClampPreMatchPollSleepsUntilCoverageWindow(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	projection := reconcile.Projection{Status: matches.StatusUpcoming, StartTime: now.Add(4 * time.Hour)}
	if got, want := clampPreMatchPoll(projection, now, now.Add(2*time.Minute)), now.Add(210*time.Minute); !got.Equal(want) {
		t.Fatalf("next poll = %s want %s", got, want)
	}
	projection.Status = matches.StatusLive
	candidate := now.Add(10 * time.Second)
	if got := clampPreMatchPoll(projection, now, candidate); !got.Equal(candidate) {
		t.Fatalf("live poll was delayed to %s", got)
	}
}

// A fixture polled successfully but still "upcoming" long after its start is
// stale, not imminent. It must park rather than resume the 15-minute cadence —
// forty such fixtures at 15m cost ~160 requests an hour for nothing.
func TestClampPreMatchPollParksOverdueUpcoming(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	overdue := reconcile.Projection{Status: matches.StatusUpcoming, StartTime: now.Add(-2 * time.Hour)}
	if got, want := clampPreMatchPoll(overdue, now, now.Add(15*time.Minute)), now.Add(overdueUpcomingRecheck); !got.Equal(want) {
		t.Fatalf("overdue upcoming next poll = %s want %s", got, want)
	}
	// A start that slipped by a few minutes is still about to begin.
	slipped := reconcile.Projection{Status: matches.StatusUpcoming, StartTime: now.Add(-5 * time.Minute)}
	candidate := now.Add(15 * time.Minute)
	if got := clampPreMatchPoll(slipped, now, candidate); !got.Equal(candidate) {
		t.Fatalf("slipped-start fixture was parked: %s", got)
	}
}

// A deterministic rejection cannot be fixed by retrying; none of them may sit
// on the transient clock spending a request per attempt.
func TestFailureBackoffParksDeterministicFailures(t *testing.T) {
	w := &Worker{logger: discardLogger{}, now: time.Now}
	target := store.FixtureTarget{ConsecutiveFailures: 3000}
	for _, cause := range []error{
		store.ErrFixtureIdentity, store.ErrMidMatchPromotion, store.ErrSettledCorrection, reconcile.ErrUnsupportedFormat,
		fmt.Errorf("%w: %w", reconcile.ErrUnsupportedFormat, reconcile.ErrSuperOver),
	} {
		if got := w.failureBackoff(target, cause); got != deterministicFailureBackoff {
			t.Fatalf("%v: backoff = %s, want %s", cause, got, deterministicFailureBackoff)
		}
	}
	// Ordinary failures wait at least two minutes and grow to a bounded cap.
	if got := w.failureBackoff(store.FixtureTarget{}, context.DeadlineExceeded); got != minFailureBackoff {
		t.Fatalf("first transient backoff = %s, want %s", got, minFailureBackoff)
	}
	if got := w.failureBackoff(store.FixtureTarget{ConsecutiveFailures: 1}, context.DeadlineExceeded); got != 2*minFailureBackoff {
		t.Fatalf("second transient backoff = %s, want %s", got, 2*minFailureBackoff)
	}
	if got := w.failureBackoff(target, context.DeadlineExceeded); got != maxFailureBackoff {
		t.Fatalf("repeated transient backoff = %s, want cap %s", got, maxFailureBackoff)
	}
}

func TestProviderStatusFailureIntervals(t *testing.T) {
	cfg := client.Config{
		PreMatchInterval: 2 * time.Minute, BreakInterval: time.Minute,
		FinalizingInterval: 15 * time.Second,
	}
	active := 10 * time.Second
	for status, want := range map[string]time.Duration{
		"Preview":       2 * time.Minute,
		"Toss":          2 * time.Minute,
		"In Progress":   active,
		"Innings Break": time.Minute,
		"Stumps":        time.Minute,
		"Rain":          time.Minute,
		// A finished match is parked: re-reading it cannot change the result
		// and every read is billed against a daily allowance.
		"Complete": store.TerminalFixtureRecheck,
		"Abandon":  store.TerminalFixtureRecheck,
		// An unknown phase must not be treated as live.
		"some-new-phase": 2 * time.Minute,
	} {
		if got := intervalForProviderStatus(status, active, cfg); got != want {
			t.Fatalf("status=%q interval=%s want=%s", status, got, want)
		}
	}
}

// The guard counts per clock hour, so a refused poll must wait for the bucket
// to roll over. Retrying sooner spins the scheduler and, once the hour turns,
// drains the whole new allowance instantly.
func TestQuotaExhaustedBackoffWaitsForTheHourToTurn(t *testing.T) {
	now := time.Date(2026, 9, 12, 14, 5, 0, 0, time.UTC)
	if got := quotaExhaustedBackoff(now); got != 55*time.Minute {
		t.Fatalf("backoff = %s, want 55m", got)
	}
	// Never return an effectively-zero wait right on the hour boundary.
	edge := time.Date(2026, 9, 12, 14, 59, 40, 0, time.UTC)
	if got := quotaExhaustedBackoff(edge); got < minFailureBackoff {
		t.Fatalf("backoff at hour edge = %s, want at least %s", got, minFailureBackoff)
	}
}
