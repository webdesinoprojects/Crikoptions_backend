package reconcile

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

func TestBuildLiveContextFromSportmonksBattingBowling(t *testing.T) {
	batting := []map[string]any{
		{
			"scoreboard": "S1", "active": true, "score": 42, "ball": 28,
			"batsman":     map[string]any{"data": map[string]any{"fullname": "Ruturaj Gaikwad"}},
			"wicket_type": "not out", "partnership_runs": 17, "partnership_balls": 10,
		},
		{
			"scoreboard": "S1", "active": false, "score": 9, "ball": 4,
			"batsman":     map[string]any{"data": map[string]any{"fullname": "Shivam Dube"}},
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

// battingRow builds a Sportmonks-shaped batting scoreboard entry. Numeric fields
// use json.Number because the reducer decodes provider payloads with UseNumber.
func battingRow(id int64, name string, runs, balls int, active bool, fow int) map[string]any {
	row := map[string]any{
		"scoreboard": "S2", "active": active, "score": runs, "ball": balls,
		"batsman_id": json.Number(strconv.FormatInt(id, 10)),
		"batsman":    map[string]any{"data": map[string]any{"fullname": name}},
	}
	if fow > 0 {
		row["fow_score"] = json.Number(strconv.Itoa(fow))
	}
	return row
}

// A dismissed top scorer must never appear on the on-field card. The old
// selection treated every row as at the crease and then picked the highest
// scorer as striker, which pinned a long-departed batter to the display.
func TestBattingPairExcludesDismissedTopScorer(t *testing.T) {
	batting := []map[string]any{
		battingRow(1, "Dismissed Century", 101, 90, false, 150),
		battingRow(2, "Current Striker", 12, 9, true, 0),
		battingRow(3, "Current Partner", 4, 6, true, 0),
	}
	deliveries := []Delivery{
		{Innings: 2, ProviderBall: "20.3", BatterID: 2, TeamRuns: 0, LegalBall: true},
	}
	striker, nonStriker, _ := battingPair(batting, "S2", deliveries, 2)

	if striker.Name != "Current Striker" {
		t.Fatalf("striker = %q, want Current Striker", striker.Name)
	}
	if nonStriker.Name != "Current Partner" {
		t.Fatalf("non-striker = %q, want Current Partner", nonStriker.Name)
	}
}

// Strike must follow the ball-by-ball feed, not the order rows arrive in.
func TestBattingPairRotatesStrikeOnOddRuns(t *testing.T) {
	batting := []map[string]any{
		battingRow(2, "Faced Last Ball", 12, 9, true, 0),
		battingRow(3, "Partner", 4, 6, true, 0),
	}
	// Single taken off the last ball: the batters crossed, so the partner is now
	// on strike even though the feed records batter 2 as facing it.
	deliveries := []Delivery{
		{Innings: 2, ProviderBall: "20.3", BatterID: 2, TeamRuns: 1, BatterRuns: 1, LegalBall: true},
	}
	striker, nonStriker, _ := battingPair(batting, "S2", deliveries, 2)

	if striker.Name != "Partner" {
		t.Fatalf("striker = %q, want Partner after a single", striker.Name)
	}
	if nonStriker.Name != "Faced Last Ball" {
		t.Fatalf("non-striker = %q", nonStriker.Name)
	}
}

func TestBattingPairKeepsStrikeOnEvenRuns(t *testing.T) {
	batting := []map[string]any{
		battingRow(2, "On Strike", 12, 9, true, 0),
		battingRow(3, "Partner", 4, 6, true, 0),
	}
	deliveries := []Delivery{
		{Innings: 2, ProviderBall: "20.3", BatterID: 2, TeamRuns: 4, BatterRuns: 4, LegalBall: true},
	}
	striker, _, _ := battingPair(batting, "S2", deliveries, 2)
	if striker.Name != "On Strike" {
		t.Fatalf("striker = %q, want On Strike after a boundary", striker.Name)
	}
}

// Six legal balls completes the over, which swaps the strike back.
func TestBattingPairSwapsStrikeAtEndOfOver(t *testing.T) {
	batting := []map[string]any{
		battingRow(2, "Faced Last Ball", 12, 9, true, 0),
		battingRow(3, "Partner", 4, 6, true, 0),
	}
	deliveries := make([]Delivery, 0, 6)
	for i := 1; i <= 6; i++ {
		deliveries = append(deliveries, Delivery{
			Innings: 2, ProviderBall: fmt.Sprintf("20.%d", i), BatterID: 2, LegalBall: true,
		})
	}
	// Six dots: no run rotation, but the over ended, so the partner faces next.
	striker, _, _ := battingPair(batting, "S2", deliveries, 2)
	if striker.Name != "Partner" {
		t.Fatalf("striker = %q, want Partner at end of over", striker.Name)
	}
}

// The bowler must come from the last delivery, not the first scoreboard row.
func TestActiveBowlerFollowsLastDelivery(t *testing.T) {
	bowling := []map[string]any{
		{
			"scoreboard": "S2", "overs": "6.0", "runs": 30, "wickets": 1, "bowler_id": json.Number("90"),
			"bowler": map[string]any{"data": map[string]any{"fullname": "Opening Bowler"}},
		},
		{
			"scoreboard": "S2", "overs": "3.0", "runs": 14, "wickets": 0, "bowler_id": json.Number("91"),
			"bowler": map[string]any{"data": map[string]any{"fullname": "Current Bowler"}},
		},
	}
	deliveries := []Delivery{
		{Innings: 2, ProviderBall: "20.3", BowlerID: 91, TeamRuns: 1, LegalBall: true},
	}
	bowler := activeBowler(bowling, "S2", deliveries, 2)
	if bowler.Name != "Current Bowler" {
		t.Fatalf("bowler = %q, want Current Bowler", bowler.Name)
	}
	if bowler.Runs != 14 {
		t.Fatalf("bowler runs = %d, want 14", bowler.Runs)
	}
}
