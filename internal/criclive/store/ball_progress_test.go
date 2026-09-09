package store

import (
	"testing"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/reconcile"
)

func TestProgressiveMatchViewStepsScorePerBall(t *testing.T) {
	base := matches.Match{
		CurrentScore: 9, WicketsLost: 0, BallsLeft: 291, ScheduledBalls: 300,
		OversText: "1.3", Format: "ODI",
	}
	// CurrentInnings totals are the authoritative end state; the progressive
	// view rewinds from them because CricLive only republishes recent overs.
	projection := reconcile.Projection{
		ScheduledBalls: 300, CurrentInnings: 1,
		CurrentScore: 11, Wickets: 0, LegalBalls: 4,
		Deliveries: []reconcile.Delivery{
			{ProviderEventID: "a", Innings: 1, Sequence: 1, ProviderBall: "0.1", TeamRuns: 1, LegalBall: true},
			{ProviderEventID: "b", Innings: 1, Sequence: 2, ProviderBall: "0.2", TeamRuns: 4, LegalBall: true},
			{ProviderEventID: "c", Innings: 1, Sequence: 3, ProviderBall: "0.3", TeamRuns: 0, LegalBall: true},
			{ProviderEventID: "d", Innings: 1, Sequence: 4, ProviderBall: "0.4", TeamRuns: 6, LegalBall: true},
		},
	}

	mid := progressiveMatchView(base, projection, projection.Deliveries[1])
	if mid.CurrentScore != 5 || mid.OversText != "0.2" || mid.BallsLeft != 298 {
		t.Fatalf("after 2 balls: score=%d overs=%s ballsLeft=%d", mid.CurrentScore, mid.OversText, mid.BallsLeft)
	}
	if len(mid.ThisOver) != 2 {
		t.Fatalf("thisOver len=%d want 2", len(mid.ThisOver))
	}

	last := progressiveMatchView(base, projection, projection.Deliveries[3])
	if last.CurrentScore != 11 || last.OversText != "0.4" || last.BallsLeft != 296 {
		t.Fatalf("after 4 balls: score=%d overs=%s ballsLeft=%d", last.CurrentScore, last.OversText, last.BallsLeft)
	}
}

// A windowed delivery list must not restart the score from zero: the runs
// bowled before the window are already inside the projection totals.
func TestProgressiveMatchViewKeepsRunsBeforeDeliveryWindow(t *testing.T) {
	base := matches.Match{ScheduledBalls: 300, Format: "ODI"}
	projection := reconcile.Projection{
		ScheduledBalls: 300, CurrentInnings: 1,
		CurrentScore: 154, Wickets: 3, LegalBalls: 122,
		DeliveryWindowInnings: 1, DeliveryWindowSequence: 2101,
		Deliveries: []reconcile.Delivery{
			{ProviderEventID: "w1", Innings: 1, Sequence: 2101, ProviderBall: "20.1", TeamRuns: 2, LegalBall: true},
			{ProviderEventID: "w2", Innings: 1, Sequence: 2102, ProviderBall: "20.2", TeamRuns: 4, LegalBall: true},
		},
	}

	first := progressiveMatchView(base, projection, projection.Deliveries[0])
	if first.CurrentScore != 150 || first.WicketsLost != 3 || first.OversText != "20.1" {
		t.Fatalf("first windowed ball: score=%d wickets=%d overs=%s", first.CurrentScore, first.WicketsLost, first.OversText)
	}
	last := progressiveMatchView(base, projection, projection.Deliveries[1])
	if last.CurrentScore != 154 || last.OversText != "20.2" {
		t.Fatalf("last windowed ball: score=%d overs=%s", last.CurrentScore, last.OversText)
	}
}

func TestSortBallEventsOrdersBySequence(t *testing.T) {
	events := []matches.BallEvent{
		{ProviderEventID: "c", Innings: 1, Sequence: 3, Over: 0, Ball: 3},
		{ProviderEventID: "a", Innings: 1, Sequence: 1, Over: 0, Ball: 1},
		{ProviderEventID: "b", Innings: 1, Sequence: 2, Over: 0, Ball: 2},
	}
	sortBallEvents(events)
	if events[0].ProviderEventID != "a" || events[1].ProviderEventID != "b" || events[2].ProviderEventID != "c" {
		t.Fatalf("order=%v", []string{events[0].ProviderEventID, events[1].ProviderEventID, events[2].ProviderEventID})
	}
}
