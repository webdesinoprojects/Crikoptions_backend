package store

import (
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/reconcile"
)

func TestLiveContextReady(t *testing.T) {
	if liveContextReady(nil) {
		t.Fatal("nil context should not be ready")
	}
	if liveContextReady(&matches.LiveMatchContext{}) {
		t.Fatal("empty context should not be ready")
	}
	ready := &matches.LiveMatchContext{
		Striker:    matches.BatterStats{Name: "A"},
		NonStriker: matches.BatterStats{Name: "B"},
		Bowler:     matches.BowlerStats{Name: "C"},
	}
	if !liveContextReady(ready) {
		t.Fatal("expected ready context")
	}
}

func TestApplyProjectionOverlayUpdatesFeedTimestamps(t *testing.T) {
	match := matches.Match{Innings: 1, FeedState: matches.FeedStateReconciling}
	projection := reconcile.Projection{
		ProviderStatus: "1st Innings",
		CurrentInnings: 1,
		CurrentScore:   50,
		Wickets:        2,
		LegalBalls:     30,
		ScheduledBalls: 300,
		LiveContext: &matches.LiveMatchContext{
			Striker:    matches.BatterStats{Name: "A", Runs: 20, Balls: 15},
			NonStriker: matches.BatterStats{Name: "B", Runs: 10, Balls: 8},
			Bowler:     matches.BowlerStats{Name: "C", Runs: 25, Balls: 30},
		},
		MatchPulse: &matches.MatchPulse{Momentum: "Even phase"},
		ThisOver:   []matches.OverBall{{Runs: 1, LegalBall: true}},
	}
	applyProjectionOverlay(&match, projection, mustTime("2026-07-17T10:00:00Z"), 50)
	applyFeedHealth(&match, projection)

	if match.FeedState != matches.FeedStateHealthy {
		t.Fatalf("feedState = %q, want healthy", match.FeedState)
	}
	if match.LiveContext == nil || match.LiveContext.Striker.Name != "A" {
		t.Fatalf("liveContext = %+v", match.LiveContext)
	}
	if match.LastSuccessfulPollAt == nil {
		t.Fatal("expected lastSuccessfulPollAt")
	}
	if containsValue(match.TradingBlockers, "reconciling") {
		t.Fatalf("blockers = %v", match.TradingBlockers)
	}
}

func mustTime(value string) (t time.Time) {
	t, _ = time.Parse(time.RFC3339, value)
	return t
}
