package store

import (
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/reconcile"
)

// A rain-shortened match must go LIVE so the scoreboard is visible, while
// buy/sell stays shut because pricing assumes a full-length innings.
func TestReducedOversGoesLiveButHoldsTrading(t *testing.T) {
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	projection := testProjection(matches.StatusLive, "reduced")
	projection.Format = "ODI"
	projection.ScheduledOvers = 47
	projection.ScheduledBalls = 282
	projection.ReducedOvers = true
	projection.Innings = []reconcile.Innings{
		{Number: 1, BattingTeamID: 10, Runs: 10, Wickets: 1, LegalBalls: 6, ScheduledBalls: 282},
	}

	current := initialMatch(projection, now)
	next := projectMatch(current, projection, now, time.Minute, 2*time.Minute, 50*time.Second, 1)

	if next.Status != matches.StatusLive {
		t.Fatalf("status = %q, want live so the scoreboard stays visible", next.Status)
	}
	if next.FeedState != matches.FeedStateHealthy {
		t.Fatalf("feedState = %q, want healthy", next.FeedState)
	}
	if next.TradingState != "blocked" {
		t.Fatalf("tradingState = %q, want blocked", next.TradingState)
	}
	if !matches.HasHardTradingBlockers(next.TradingBlockers) {
		t.Fatalf("blockers = %v, want a hard blocker", next.TradingBlockers)
	}
	if next.ScheduledOvers != 47 || next.ScheduledBalls != 282 {
		t.Fatalf("schedule = %d overs / %d balls, want 47/282", next.ScheduledOvers, next.ScheduledBalls)
	}
	if next.BallsLeft != 276 {
		t.Fatalf("ballsLeft = %d, want 276 (282 - 6 bowled)", next.BallsLeft)
	}
	if matches.IsTradable(&next) {
		t.Fatal("a reduced-overs match must not be tradable")
	}
}
