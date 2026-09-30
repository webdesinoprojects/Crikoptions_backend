package cricketline

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/reconcile"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

// The testdata payloads are live CricketLineApi responses recorded on
// 29 September 2026, including the ball-by-ball finish of match 14F7 and two
// finished matches whose team_id fields belong to the other side.

type side struct{ key, name, short string }

func loadJSON(t *testing.T, name string, target any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

func testFixture(t *testing.T, match, series, format string, a, b side) client.Fixture {
	t.Helper()
	id := func(key string) int64 {
		value, err := EncodeKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	return client.Fixture{
		ID: id(match), SeriesID: id(series), Format: format,
		StartingAt:  time.Date(2026, 9, 29, 16, 0, 0, 0, time.UTC),
		LocalTeamID: id(a.key), VisitorTeamID: id(b.key),
		LocalTeamName: a.name, VisitorTeamName: b.name,
		LocalTeamShort: a.short, VisitorTeamShort: b.short,
	}
}

func teamsOf(fixture client.Fixture) [2]fixtureTeam {
	return [2]fixtureTeam{
		{id: fixture.LocalTeamID, labels: []string{fixture.LocalTeamShort, fixture.LocalTeamName}},
		{id: fixture.VisitorTeamID, labels: []string{fixture.VisitorTeamShort, fixture.VisitorTeamName}},
	}
}

func reduce(t *testing.T, tab matchLiveResponse, fixture client.Fixture, strike *strikeTracker, now time.Time) (reconcile.Projection, translation) {
	t.Helper()
	translated, err := translateLive(tab, fixture, teamsOf(fixture), 0, strike, now)
	if err != nil {
		t.Fatalf("translateLive: %v", err)
	}
	projection, err := reconcile.ReduceSnapshot(client.Snapshot{
		MatchID: fixture.ID, Fixture: fixture,
		Commentary: translated.commentary, Overs: translated.overs, ReceivedAt: now,
	})
	if err != nil {
		t.Fatalf("ReduceSnapshot: %v", err)
	}
	return projection, translated
}

var (
	knights = side{"6E", "Knights", "KNG"}
	rocks   = side{"JG", "Rocks", "Rocks"}
)

func TestTranslateSecondInningsInPlay(t *testing.T) {
	var tab matchLiveResponse
	loadJSON(t, "match_live_14F7.json", &tab)
	fixture := testFixture(t, "14F7", "2MT", "T20", knights, rocks)

	projection, _ := reduce(t, tab, fixture, newStrikeTracker(), time.Now())

	if projection.Status != matches.StatusLive || projection.ProviderState != client.StateInProgress {
		t.Fatalf("status = %s/%s, want live/In Progress", projection.Status, projection.ProviderState)
	}
	if projection.CurrentInnings != 2 || projection.BattingTeamID != fixture.VisitorTeamID {
		t.Fatalf("innings %d batting %d, want 2 batting Rocks", projection.CurrentInnings, projection.BattingTeamID)
	}
	if projection.CurrentScore != 148 || projection.Wickets != 4 || projection.LegalBalls != 113 || projection.Target != 161 {
		t.Fatalf("score %d/%d in %d balls chasing %d, want 148/4 in 113 chasing 161",
			projection.CurrentScore, projection.Wickets, projection.LegalBalls, projection.Target)
	}
	first := projection.Innings[0]
	if first.Number != 1 || first.BattingTeamID != fixture.LocalTeamID || first.Runs != 160 || first.Wickets != 5 || !first.Complete {
		t.Fatalf("first innings = %+v, want Knights 160/5 complete", first)
	}
	if projection.ScheduledBalls != 120 || projection.ReducedOvers {
		t.Fatalf("scheduled %d reduced %t, want a full T20", projection.ScheduledBalls, projection.ReducedOvers)
	}
	// Over 19 is in progress: 4 0 4 0 1 with the ids the store deduplicates on.
	var over19 []string
	for _, delivery := range projection.Deliveries {
		if delivery.Innings == 2 && delivery.ProviderBall[:2] == "18" {
			over19 = append(over19, delivery.ProviderEventID)
		}
	}
	if len(over19) != 5 || over19[0] != "2-19-1" || over19[4] != "2-19-5" {
		t.Fatalf("over 19 deliveries = %v", over19)
	}
	if projection.LiveContext == nil || projection.LiveContext.Bowler.Name != "M Siboto" ||
		projection.LiveContext.Bowler.Wickets != 1 || projection.LiveContext.Bowler.Runs != 23 {
		t.Fatalf("bowler = %+v", projection.LiveContext)
	}
}

func TestTranslateAttributesScoresByLabelWhenTeamIDsAreSwapped(t *testing.T) {
	tests := []struct {
		file           string
		match, series  string
		a, b           side
		firstBat       side
		firstRuns      int
		secondBat      side
		secondRuns     int
		secondWickets  int
		scheduledBalls int
	}{
		{
			file: "match_live_14F6_final_swapped_ids.json", match: "14F6", series: "2MT",
			a: side{"6C", "Warriors", "WAR"}, b: side{"JD", "Limpopo Impalas", "LMP"},
			firstBat: side{key: "6C"}, firstRuns: 175, secondBat: side{key: "JD"}, secondRuns: 140, secondWickets: 8,
			scheduledBalls: 120,
		},
		{
			file: "match_live_13VI_final_swapped_ids.json", match: "13VI", series: "2M6",
			a: side{"1CX", "Sambalpur Warriors", "SW"}, b: side{"1CV", "Bhubaneswar Tigers", "BT"},
			firstBat: side{key: "1CX"}, firstRuns: 162, secondBat: side{key: "1CV"}, secondRuns: 153, secondWickets: 9,
			scheduledBalls: 120,
		},
	}
	for _, test := range tests {
		t.Run(test.match, func(t *testing.T) {
			var tab matchLiveResponse
			loadJSON(t, test.file, &tab)
			fixture := testFixture(t, test.match, test.series, "T20", test.a, test.b)
			projection, _ := reduce(t, tab, fixture, newStrikeTracker(), time.Now())

			if projection.Status != matches.StatusCompleted {
				t.Fatalf("status = %s (%q), want completed", projection.Status, projection.ProviderStatus)
			}
			firstID, _ := EncodeKey(test.firstBat.key)
			secondID, _ := EncodeKey(test.secondBat.key)
			if len(projection.Innings) != 2 {
				t.Fatalf("innings = %+v", projection.Innings)
			}
			first, second := projection.Innings[0], projection.Innings[1]
			if first.BattingTeamID != firstID || first.Runs != test.firstRuns || !first.Complete {
				t.Fatalf("first innings = %+v, want %s %d", first, test.firstBat.key, test.firstRuns)
			}
			if second.BattingTeamID != secondID || second.Runs != test.secondRuns || second.Wickets != test.secondWickets || !second.Complete {
				t.Fatalf("second innings = %+v, want %s %d/%d", second, test.secondBat.key, test.secondRuns, test.secondWickets)
			}
			if projection.ScheduledBalls != test.scheduledBalls {
				t.Fatalf("scheduled balls = %d", projection.ScheduledBalls)
			}
		})
	}
}

func TestTranslateTenOverLeagueIsReduced(t *testing.T) {
	var tab matchLiveResponse
	loadJSON(t, "match_live_14D3_ten_overs.json", &tab)
	// Uttarakhand Premier League: listed as T20, played at ten overs.
	fixture := testFixture(t, "14D3", "2N8", "T20", side{"RH", "Haridwar Elmas", "HE"}, side{"RD", "USN Indians", "USN"})
	projection, translated := reduce(t, tab, fixture, newStrikeTracker(), time.Now())

	if !projection.ReducedOvers || projection.ScheduledOvers != 10 || projection.ScheduledBalls != 60 {
		t.Fatalf("scheduled %d overs / %d balls reduced=%t, want 10/60 reduced",
			projection.ScheduledOvers, projection.ScheduledBalls, projection.ReducedOvers)
	}
	if projection.Status != matches.StatusCompleted {
		t.Fatalf("status = %s", projection.Status)
	}
	// "wd" and "nb" are one-run extras; the over totals 16 as the feed says.
	var over10 client.OverItem
	for _, over := range translated.overs.Overs {
		if over.InningsID == 2 && over.OverNumber.Float64() == 10 {
			over10 = over
		}
	}
	want := []string{"Wd1", "N0", "0", "6", "Wd1", "0", "Wd1", "0", "Wd1", "4", "1"}
	if len(over10.Balls) != len(want) {
		t.Fatalf("over 10 = %v", over10.Balls)
	}
	for i := range want {
		if over10.Balls[i] != want[i] {
			t.Fatalf("over 10 = %v, want %v", over10.Balls, want)
		}
	}
	if overTotal(over10.Balls) != 16 {
		t.Fatalf("over 10 totals %d, want 16", overTotal(over10.Balls))
	}
}

func TestTranslateUpcomingHasNoInnings(t *testing.T) {
	var tab matchLiveResponse
	loadJSON(t, "match_live_14F8_upcoming.json", &tab)
	fixture := testFixture(t, "14F8", "2MT", "T20", side{"6A", "Dolphins", "DOL"}, side{"J8", "Northern Cape Heat", "NCH"})
	projection, _ := reduce(t, tab, fixture, newStrikeTracker(), time.Now())
	if projection.Status != matches.StatusUpcoming || projection.CurrentInnings != 0 || len(projection.Innings) != 0 {
		t.Fatalf("projection = status %s innings %d %+v, want upcoming with no innings",
			projection.Status, projection.CurrentInnings, projection.Innings)
	}
}

func TestResolveSidesFailsClosedWithoutLabels(t *testing.T) {
	a, _ := EncodeKey("6E")
	b, _ := EncodeKey("JG")
	teams := [2]fixtureTeam{{id: a, labels: []string{"Knights"}}, {id: b, labels: []string{"Rocks XI"}}}
	// The ids agree with a straight assignment, but ids are the field the feed
	// swaps; with no label to confirm them the read must be refused.
	_, _, err := resolveSides(teamRef{ShortName: "KNG", TeamID: "6E"}, teamRef{ShortName: "RCK", TeamID: "JG"}, teams)
	if !errors.Is(err, ErrTeamsUnresolved) {
		t.Fatalf("err = %v, want ErrTeamsUnresolved", err)
	}
	batting, bowling, err := resolveSides(teamRef{ShortName: "Rocks XI", TeamID: "6E"}, teamRef{ShortName: "Knights", TeamID: "JG"}, teams)
	if err != nil || batting != b || bowling != a {
		t.Fatalf("resolveSides = %d, %d, %v; want labels to win over swapped ids", batting, bowling, err)
	}
}

func TestStrikeFollowsBallsFacedThroughTheFinish(t *testing.T) {
	var sequence []matchLiveResponse
	loadJSON(t, "match_live_14F7_finish_sequence.json", &sequence)
	fixture := testFixture(t, "14F7", "2MT", "T20", knights, rocks)
	tracker := newStrikeTracker()
	want := []string{
		"J van Briesies", "J van Briesies", // 19.0: not yet confirmed, list order
		"J van Briesies", "J van Briesies", // 19.1 four: he faced it and keeps strike
		"J van Briesies", "J van Briesies", // 19.2 caught: still listed
		"F Adams", "F Adams", "F Adams", // new batter takes strike
		"G Kaplan",           // 19.3 single
		"F Adams", "F Adams", // 19.4 single
		"G Kaplan", "G Kaplan", "G Kaplan", "G Kaplan", // 19.5 three
		"F Adams", "F Adams", // 20.0 four, then the over ends
	}
	if len(sequence) != len(want) {
		t.Fatalf("sequence has %d reads, expectations %d", len(sequence), len(want))
	}
	base := time.Date(2026, 9, 29, 19, 3, 0, 0, time.UTC)
	var last reconcile.Projection
	for i, tab := range sequence {
		projection, translated := reduce(t, tab, fixture, tracker, base.Add(time.Duration(i)*10*time.Second))
		if got := translated.commentary.MiniScore.Striker.Name; got != want[i] {
			t.Fatalf("read %d (%s %s): striker %q, want %q", i, tab.BattingTeam.Score, tab.BattingTeam.Overs, got, want[i])
		}
		last = projection
	}
	if last.Status != matches.StatusCompleted || last.CurrentScore != 161 || last.Wickets != 5 {
		t.Fatalf("final projection = %s %d/%d", last.Status, last.CurrentScore, last.Wickets)
	}
	var final []string
	for _, delivery := range last.Deliveries {
		if delivery.Innings == 2 && delivery.ProviderBall[:2] == "19" {
			final = append(final, delivery.ProviderEventID)
			if delivery.ProviderEventID == "2-20-2" && delivery.Dismissal == nil {
				t.Fatalf("2-20-2 should carry the wicket")
			}
		}
	}
	if len(final) != 6 || final[5] != "2-20-6" {
		t.Fatalf("last over deliveries = %v", final)
	}
}
