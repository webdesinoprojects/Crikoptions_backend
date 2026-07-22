package store

import (
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

// A live poll must clear a stuck cancellation_pending. providerBlockers keeps
// the marker (it is not an automated blocker) and the "any blocker re-blocks"
// rule at the end of projectMatch then re-closed the gate, so one feed hiccup
// left a healthy live match untradable until the gate job happened to drain.
func TestProjectMatchLivePollClearsStuckCancellationPending(t *testing.T) {
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	current := initialMatch(testProjection(matches.StatusLive, "one"), now)
	current.Status = matches.StatusLive
	current.FeedState = matches.FeedStateReconciling
	current.HealthySnapshotCount = 3
	current.TradingState = "blocked"
	current.TradingBlockers = []string{"cancellation_pending", "reconciling"}

	next := projectMatch(current, testProjection(matches.StatusLive, "two"), now.Add(4*time.Second),
		time.Minute, 2*time.Minute, 50*time.Second, 7)

	if containsValue(next.TradingBlockers, "cancellation_pending") {
		t.Fatalf("cancellation_pending survived a live poll: %v", next.TradingBlockers)
	}
	if next.TradingState != "open" {
		t.Fatalf("tradingState = %q, want open (blockers=%v)", next.TradingState, next.TradingBlockers)
	}
	if !matches.IsTradable(&next) {
		t.Fatalf("match must be tradable after a healthy live poll: %+v", next.TradingBlockers)
	}
}

// A manual (admin) suspension is not a sync artefact and must still survive.
func TestProjectMatchLivePollKeepsManualBlockerWhileClearingSyncMarker(t *testing.T) {
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	current := initialMatch(testProjection(matches.StatusLive, "one"), now)
	current.Status = matches.StatusLive
	current.HealthySnapshotCount = 3
	current.TradingState = "blocked"
	current.TradingBlockers = []string{"cancellation_pending", "manual"}

	next := projectMatch(current, testProjection(matches.StatusLive, "two"), now.Add(4*time.Second),
		time.Minute, 2*time.Minute, 50*time.Second, 7)

	if containsValue(next.TradingBlockers, "cancellation_pending") {
		t.Fatalf("cancellation_pending survived: %v", next.TradingBlockers)
	}
	if !containsValue(next.TradingBlockers, "manual") {
		t.Fatalf("manual suspension was dropped: %v", next.TradingBlockers)
	}
	if next.TradingState != "blocked" || matches.IsTradable(&next) {
		t.Fatalf("manual suspension must keep the match untradable: %+v", next)
	}
}

// A genuine hard stop still blocks — the fix must not open the gate at an
// innings break.
func TestProjectMatchInningsBreakStillBlocks(t *testing.T) {
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	current := initialMatch(testProjection(matches.StatusLive, "one"), now)
	current.Status = matches.StatusLive
	current.HealthySnapshotCount = 3
	current.TradingState = "open"

	next := projectMatch(current, testProjection(matches.StatusInningsBreak, "brk"), now.Add(4*time.Second),
		time.Minute, 2*time.Minute, 50*time.Second, 7)

	if next.TradingState != "blocked" || !containsValue(next.TradingBlockers, "innings_break") {
		t.Fatalf("innings break must block: state=%s blockers=%v", next.TradingState, next.TradingBlockers)
	}
	if matches.IsTradable(&next) {
		t.Fatal("innings break must not be tradable")
	}
}
