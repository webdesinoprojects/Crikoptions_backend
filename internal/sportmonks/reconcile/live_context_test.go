package reconcile

import (
	"testing"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

func TestBuildLiveContextFromSportmonksBattingBowling(t *testing.T) {
	batting := []map[string]any{
		{
			"scoreboard": "S1", "active": true, "score": 42, "ball": 28,
			"batsman": map[string]any{"data": map[string]any{"fullname": "Ruturaj Gaikwad"}},
			"wicket_type": "not out", "partnership_runs": 17, "partnership_balls": 10,
		},
		{
			"scoreboard": "S1", "active": false, "score": 9, "ball": 4,
			"batsman": map[string]any{"data": map[string]any{"fullname": "Shivam Dube"}},
			"wicket_type": "not out",
		},
	}
	bowling := []map[string]any{
		{
			"scoreboard": "S1", "active": true, "overs": "1.0", "medians": 0,
			"runs": 13, "wickets": 0,
			"bowler": map[string]any{"data": map[string]any{"fullname": "Jasprit Bumrah"}},
		},
	}
	deliveries := []Delivery{
		{Innings: 1, ProviderBall: "0.1", TeamRuns: 0, LegalBall: true},
		{Innings: 1, ProviderBall: "0.2", TeamRuns: 1, LegalBall: true},
		{Innings: 1, ProviderBall: "0.3", TeamRuns: 2, LegalBall: true},
		{Innings: 1, ProviderBall: "0.4", TeamRuns: 0, LegalBall: true},
		{Innings: 1, ProviderBall: "0.5", TeamRuns: 6, LegalBall: true, BatterRuns: 6},
		{Innings: 1, ProviderBall: "0.6", TeamRuns: 6, LegalBall: true, BatterRuns: 6},
	}
	input := LiveContextInput{
		CurrentInnings: 1, BattingTeamID: 10, LocalTeamID: 10, VisitorTeamID: 11,
		LocalTeamName: "CSK", VisitorTeamName: "MI",
		CurrentScore: 42, Wickets: 1, LegalBalls: 6, ScheduledBalls: 120, Deliveries: deliveries,
	}

	live := BuildLiveContext(batting, bowling, input)
	if live == nil {
		t.Fatal("expected live context")
	}
	if live.Striker.Name != "Ruturaj Gaikwad" || live.Striker.Runs != 42 || live.Striker.Balls != 28 {
		t.Fatalf("striker = %+v", live.Striker)
	}
	if live.NonStriker.Name != "Shivam Dube" {
		t.Fatalf("non-striker = %+v", live.NonStriker)
	}
	if live.Bowler.Name != "Jasprit Bumrah" || live.Bowler.Runs != 13 {
		t.Fatalf("bowler = %+v", live.Bowler)
	}
	if live.Partnership.Runs != 17 || live.Partnership.Balls != 10 {
		t.Fatalf("partnership = %+v", live.Partnership)
	}

	thisOver := BuildThisOver(deliveries, 1, 6)
	if len(thisOver) != 6 {
		t.Fatalf("thisOver len = %d, want 6", len(thisOver))
	}
	if thisOver[4].Runs != 6 || !thisOver[4].LegalBall {
		t.Fatalf("5th ball = %+v", thisOver[4])
	}

	pulse := BuildMatchPulse(input)
	if pulse == nil {
		t.Fatal("expected match pulse")
	}
	if pulse.LastWicket != "No wicket this over" {
		t.Fatalf("lastWicket = %q", pulse.LastWicket)
	}
	if pulse.MarketVolatility != "High" {
		t.Fatalf("volatility = %q, want High", pulse.MarketVolatility)
	}
}

func TestBuildLiveContextReturnsNilWithoutBatters(t *testing.T) {
	live := BuildLiveContext(nil, nil, LiveContextInput{CurrentInnings: 1})
	if live != nil {
		t.Fatalf("live = %+v, want nil", live)
	}
}

func TestBuildThisOverUsesCurrentOverOnly(t *testing.T) {
	deliveries := []Delivery{
		{Innings: 1, ProviderBall: "0.6", TeamRuns: 1, LegalBall: true},
		{Innings: 1, ProviderBall: "1.1", TeamRuns: 4, LegalBall: true, BatterRuns: 4},
	}
	over := BuildThisOver(deliveries, 1, 7)
	if len(over) != 1 || over[0].Runs != 4 {
		t.Fatalf("over = %+v", over)
	}
}

func TestBuildMatchPulseChasePressure(t *testing.T) {
	input := LiveContextInput{
		CurrentInnings: 2, BattingTeamID: 11, LocalTeamID: 10, VisitorTeamID: 11,
		LocalTeamName: "CSK", VisitorTeamName: "MI",
		CurrentScore: 100, Target: 180, LegalBalls: 100, ScheduledBalls: 120,
		Deliveries: []Delivery{{Innings: 2, ProviderBall: "16.4", TeamRuns: 1, LegalBall: true}},
	}
	pulse := BuildMatchPulse(input)
	if pulse == nil {
		t.Fatal("expected pulse")
	}
	if pulse.PressureLevel != "chase" {
		t.Fatalf("pressure level = %q, want chase", pulse.PressureLevel)
	}
	if pulse.Pressure != "On MI" {
		t.Fatalf("pressure = %q", pulse.Pressure)
	}
}

func TestOverBallJSONShape(t *testing.T) {
	_ = matches.OverBall{Runs: 6, LegalBall: true}
}
