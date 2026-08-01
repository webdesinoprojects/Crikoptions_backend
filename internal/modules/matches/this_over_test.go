package matches

import (
	"context"
	"testing"
)

func overChips(balls []OverBall) []int {
	out := make([]int, len(balls))
	for i, ball := range balls {
		out[i] = ball.Runs
	}
	return out
}

// Warm-up (manual/simulator) matches previously served a null thisOver, because
// only the Sportmonks reducer populated it. The ball strip renders that array,
// so the panel came up empty for every demo game.
func TestRecordBallPopulatesThisOverForManualMatches(t *testing.T) {
	svc := liveSeedService(t)
	ctx := context.Background()

	feedBall(t, svc, "1", 0, false, "")
	feedBall(t, svc, "1", 1, false, "")
	feedBall(t, svc, "1", 4, false, "")

	match, err := svc.GetMatchByID(ctx, "1")
	if err != nil || match == nil {
		t.Fatalf("GetMatchByID: %v", err)
	}
	if got, want := overChips(match.ThisOver), []int{0, 1, 4}; !equalInts(got, want) {
		t.Fatalf("thisOver = %v, want %v", got, want)
	}
}

// The strip must restart at the top of each over rather than accumulating.
func TestThisOverResetsOnNewOver(t *testing.T) {
	svc := liveSeedService(t)
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		feedBall(t, svc, "1", 1, false, "")
	}
	match, err := svc.GetMatchByID(ctx, "1")
	if err != nil || match == nil {
		t.Fatalf("GetMatchByID: %v", err)
	}
	if len(match.ThisOver) != 6 {
		t.Fatalf("end of over: thisOver = %v, want 6 balls", overChips(match.ThisOver))
	}

	feedBall(t, svc, "1", 2, false, "")
	match, err = svc.GetMatchByID(ctx, "1")
	if err != nil || match == nil {
		t.Fatalf("GetMatchByID: %v", err)
	}
	if got, want := overChips(match.ThisOver), []int{2}; !equalInts(got, want) {
		t.Fatalf("new over: thisOver = %v, want %v", got, want)
	}
}

// Extras stay in the over and are flagged so the client can exclude them from
// the six legal-ball slots.
func TestThisOverKeepsExtrasAndWickets(t *testing.T) {
	svc := liveSeedService(t)
	ctx := context.Background()

	feedBall(t, svc, "1", 1, false, "")
	feedBall(t, svc, "1", 1, false, ExtraWide)
	feedBall(t, svc, "1", 0, true, "")

	match, err := svc.GetMatchByID(ctx, "1")
	if err != nil || match == nil {
		t.Fatalf("GetMatchByID: %v", err)
	}
	if len(match.ThisOver) != 3 {
		t.Fatalf("thisOver = %v, want 3 entries", match.ThisOver)
	}
	if match.ThisOver[1].Extra != ExtraWide || match.ThisOver[1].LegalBall {
		t.Fatalf("wide entry = %+v, want extra=wide legalBall=false", match.ThisOver[1])
	}
	if !match.ThisOver[2].IsWicket {
		t.Fatalf("wicket entry = %+v, want isWicket", match.ThisOver[2])
	}
}
