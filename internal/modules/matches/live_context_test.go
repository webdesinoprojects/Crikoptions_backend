package matches

import "testing"

func TestApplyDeliveryToLiveContextTracksPlayersAndExtras(t *testing.T) {
	live := &LiveMatchContext{
		Striker:    BatterStats{Name: "A Batter"},
		NonStriker: BatterStats{Name: "B Batter"},
		Bowler:     BowlerStats{Name: "A Bowler"},
	}

	applyDeliveryToLiveContext(live, BallEventRequest{Runs: 1}, nil, true, 0)
	if live.Striker.Name != "B Batter" || live.NonStriker.Name != "A Batter" {
		t.Fatalf("odd run should rotate strike: striker=%q non-striker=%q", live.Striker.Name, live.NonStriker.Name)
	}
	if live.NonStriker.Runs != 1 || live.NonStriker.Balls != 1 {
		t.Fatalf("original striker = %d (%d), want 1 (1)", live.NonStriker.Runs, live.NonStriker.Balls)
	}

	applyDeliveryToLiveContext(live, BallEventRequest{Runs: 6}, nil, true, 1)
	noBall := ExtraNoBall
	applyDeliveryToLiveContext(live, BallEventRequest{Runs: 1, Extra: ExtraNoBall}, &noBall, false, 2)

	if live.Striker.Runs != 6 || live.Striker.Balls != 1 {
		t.Fatalf("striker = %d (%d), want 6 (1)", live.Striker.Runs, live.Striker.Balls)
	}
	if live.Bowler.Balls != 2 || live.Bowler.Runs != 8 {
		t.Fatalf("bowler = %d balls/%d runs, want 2/8", live.Bowler.Balls, live.Bowler.Runs)
	}
	if live.Partnership.Runs != 8 || live.Partnership.Balls != 2 {
		t.Fatalf("partnership = %d (%d), want 8 (2)", live.Partnership.Runs, live.Partnership.Balls)
	}

	applyDeliveryToLiveContext(live, BallEventRequest{IsWicket: true, NextBatterName: "C Batter"}, nil, true, 2)
	if live.Striker.Name != "C Batter" || live.Bowler.Wickets != 1 {
		t.Fatalf("wicket state: striker=%q wickets=%d", live.Striker.Name, live.Bowler.Wickets)
	}
	if live.Partnership.Runs != 0 || live.Partnership.Balls != 0 {
		t.Fatalf("partnership should reset on wicket: %+v", live.Partnership)
	}
}

func TestApplyDeliveryToLiveContextCompletesMaidenAndChangesEnds(t *testing.T) {
	live := &LiveMatchContext{
		Striker:    BatterStats{Name: "A Batter"},
		NonStriker: BatterStats{Name: "B Batter"},
		Bowler:     BowlerStats{Name: "A Bowler"},
	}

	for ball := 0; ball < 6; ball++ {
		applyDeliveryToLiveContext(live, BallEventRequest{}, nil, true, ball)
	}

	if live.Striker.Name != "B Batter" || live.NonStriker.Name != "A Batter" {
		t.Fatalf("end of over should change ends: striker=%q non-striker=%q", live.Striker.Name, live.NonStriker.Name)
	}
	if live.Bowler.Balls != 6 || live.Bowler.Maidens != 1 || live.Bowler.CurrentOverRuns != 0 {
		t.Fatalf("bowler figures after maiden: %+v", live.Bowler)
	}
}
