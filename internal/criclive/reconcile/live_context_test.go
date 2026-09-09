package reconcile

import (
	"testing"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

func TestBuildLiveContextMapsMiniScoreDirectly(t *testing.T) {
	mini := client.MiniScore{
		Striker:       client.BattingLine{Name: "Dan Lawrence", Runs: 5, Balls: 10},
		NonStriker:    client.BattingLine{Name: "Harry Brook", Runs: 52, Balls: 76},
		BowlerStriker: client.BowlingLine{Name: "Razaullah", Overs: 8.6, Maidens: 1, Runs: 42, Wickets: 2},
		Partnership:   client.Partnership{Runs: 13, Balls: 32},
	}
	live := BuildLiveContext(mini)
	if live == nil {
		t.Fatal("live context is nil")
	}
	if live.Striker.Name != "Dan Lawrence" || live.NonStriker.Name != "Harry Brook" {
		t.Fatalf("batters = %+v / %+v", live.Striker, live.NonStriker)
	}
	// 8.6 overs is nine completed overs.
	if live.Bowler.Name != "Razaullah" || live.Bowler.Balls != 54 || live.Bowler.Maidens != 1 {
		t.Fatalf("bowler = %+v", live.Bowler)
	}
	if live.Partnership.Runs != 13 || live.Partnership.Balls != 32 {
		t.Fatalf("partnership = %+v", live.Partnership)
	}
}

// Before the toss there is nobody at the crease; the UI shows its own waiting
// state rather than three blank rows.
func TestBuildLiveContextIsNilWhenNobodyIsNamed(t *testing.T) {
	if live := BuildLiveContext(client.MiniScore{}); live != nil {
		t.Fatalf("expected nil live context, got %+v", live)
	}
}

func TestBuildMatchPulseReadsRecentOvers(t *testing.T) {
	input := LiveContextInput{
		CurrentInnings: 1, BattingTeamID: 2, LocalTeamID: 1, VisitorTeamID: 2,
		LocalTeamName: "Pakistan", VisitorTeamName: "South Africa",
		ScheduledBalls: 120,
	}
	attacking := BuildMatchPulse(client.MiniScore{
		RecentOvers: "0 0 1 1 0 0  | 4 6 1 4 0 1",
		LastWicket:  "Emilio Gay b Razaullah 60(114) - 143/4 in 33.4 ov.",
	}, input)
	if attacking.MomentumLevel != "attacking" || attacking.Momentum != "South Africa attacking" {
		t.Fatalf("momentum = %+v", attacking)
	}
	if attacking.VolatilityLevel != "high" {
		t.Fatalf("volatility = %q", attacking.VolatilityLevel)
	}
	// CricLive already phrases the wicket line as a scorecard entry.
	if attacking.LastWicket != "Emilio Gay b Razaullah 60(114) - 143/4 in 33.4 ov." {
		t.Fatalf("lastWicket = %q", attacking.LastWicket)
	}

	quiet := BuildMatchPulse(client.MiniScore{RecentOvers: "4 4 1 1 0 0  | 0 0 1 0 0 0"}, input)
	if quiet.MomentumLevel != "defensive" || quiet.Momentum != "Pakistan control" {
		t.Fatalf("quiet momentum = %+v", quiet)
	}
	if quiet.LastWicket != "No wicket this over" {
		t.Fatalf("quiet lastWicket = %q", quiet.LastWicket)
	}
}

func TestBuildMatchPulseChaseAndDefendPressure(t *testing.T) {
	input := LiveContextInput{
		CurrentInnings: 2, BattingTeamID: 2, LocalTeamID: 1, VisitorTeamID: 2,
		LocalTeamName: "Pakistan", VisitorTeamName: "South Africa",
		CurrentScore: 96, LegalBalls: 78, ScheduledBalls: 120, Target: 141,
	}
	chase := BuildMatchPulse(client.MiniScore{CurrentRunRate: 7.38, RequiredRate: 10.7}, input)
	if chase.PressureLevel != "chase" || chase.Pressure != "On South Africa" {
		t.Fatalf("chase pressure = %+v", chase)
	}

	defend := BuildMatchPulse(client.MiniScore{CurrentRunRate: 9.5, RequiredRate: 4.2}, input)
	if defend.PressureLevel != "defend" || defend.Pressure != "On Pakistan" {
		t.Fatalf("defend pressure = %+v", defend)
	}

	input.CurrentScore = 141
	reached := BuildMatchPulse(client.MiniScore{}, input)
	if reached.PressureLevel != "complete" {
		t.Fatalf("target reached pressure = %+v", reached)
	}
}

func TestBuildMatchPulseIsNilBeforeFirstInnings(t *testing.T) {
	if pulse := BuildMatchPulse(client.MiniScore{}, LiveContextInput{}); pulse != nil {
		t.Fatalf("expected nil pulse, got %+v", pulse)
	}
}

func TestBuildThisOverShowsCurrentOverOnly(t *testing.T) {
	deliveries := []Delivery{
		{Innings: 1, ProviderBall: "11.5", TeamRuns: 1, LegalBall: true},
		{Innings: 1, ProviderBall: "11.6", TeamRuns: 0, LegalBall: true},
		{Innings: 1, ProviderBall: "12.1", TeamRuns: 4, LegalBall: true},
		{Innings: 1, ProviderBall: "12.1", TeamRuns: 1, LegalBall: false,
			Extras: matches.DeliveryExtras{Wides: 1}},
		{Innings: 1, ProviderBall: "12.2", TeamRuns: 0, LegalBall: true,
			Dismissal: &matches.Dismissal{Kind: "wicket"}},
		{Innings: 2, ProviderBall: "12.3", TeamRuns: 6, LegalBall: true},
	}
	// 74 legal balls puts the match in over index 12.
	over := BuildThisOver(deliveries, 1, 74)
	if len(over) != 3 {
		t.Fatalf("over = %+v", over)
	}
	if over[0].Runs != 4 || !over[0].LegalBall {
		t.Fatalf("first ball = %+v", over[0])
	}
	if over[1].Extra != matches.ExtraWide || over[1].LegalBall {
		t.Fatalf("wide = %+v", over[1])
	}
	if !over[2].IsWicket {
		t.Fatalf("wicket ball = %+v", over[2])
	}
	if BuildThisOver(deliveries, 1, 0) != nil {
		t.Fatal("no balls bowled should yield no strip")
	}
}

func TestCurrentOverIndexHoldsCompletedOver(t *testing.T) {
	cases := map[int]int{0: 0, 1: 0, 6: 0, 7: 1, 12: 1, 13: 2, 78: 12}
	for legalBalls, want := range cases {
		if got := currentOverIndex(legalBalls); got != want {
			t.Fatalf("currentOverIndex(%d) = %d, want %d", legalBalls, got, want)
		}
	}
}
