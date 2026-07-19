package worker

import (
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/reconcile"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/store"
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
		{matches.StatusCompleted, matches.FeedStateTerminal, 24 * time.Hour},
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
	if got, want := clampPreMatchPoll(projection, now.Add(2*time.Minute)), now.Add(210*time.Minute); !got.Equal(want) {
		t.Fatalf("next poll = %s want %s", got, want)
	}
	projection.Status = matches.StatusLive
	candidate := now.Add(10 * time.Second)
	if got := clampPreMatchPoll(projection, candidate); !got.Equal(candidate) {
		t.Fatalf("live poll was delayed to %s", got)
	}
}

func TestProviderStatusFailureIntervals(t *testing.T) {
	cfg := client.Config{
		PreMatchInterval: 2 * time.Minute, BreakInterval: time.Minute,
		FinalizingInterval: 15 * time.Second,
	}
	active := 10 * time.Second
	for status, want := range map[string]time.Duration{
		"NS":            2 * time.Minute,
		"1st Innings":   active,
		"Innings Break": time.Minute,
		"Int.":          time.Minute,
		"Finished":      15 * time.Second,
	} {
		if got := intervalForProviderStatus(status, active, cfg); got != want {
			t.Fatalf("status=%q interval=%s want=%s", status, got, want)
		}
	}
}
