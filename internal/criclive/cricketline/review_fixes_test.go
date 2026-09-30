package cricketline

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

// A completed innings is settled on its aggregates, so its fingerprint must
// not move when its overs scroll into or out of the feed's recent-overs window.
func TestCompletedInningsHashIgnoresTheOversWindow(t *testing.T) {
	var tab matchLiveResponse
	loadJSON(t, "match_live_14F7.json", &tab)
	fixture := testFixture(t, "14F7", "2MT", "T20", knights, rocks)
	withoutFirst, _ := reduce(t, tab, fixture, newStrikeTracker(), time.Now())

	scrolled := tab
	scrolled.RecentOvers = append([]tabRecentOver{{Over: "Over 20", Balls: []string{"1", "4", "W", "0", "6", "1"}, Runs: 12}}, tab.RecentOvers...)
	withFirst, translated := reduce(t, scrolled, fixture, newStrikeTracker(), time.Now())
	if translated.overs.Overs[0].InningsID != 1 {
		t.Fatalf("over 20 filed under innings %d, want 1", translated.overs.Overs[0].InningsID)
	}
	if withFirst.Innings[0].SnapshotHash != withoutFirst.Innings[0].SnapshotHash {
		t.Fatal("innings 1 fingerprint changed with the overs window; a settled innings would be refused as corrected")
	}
}

func TestDeliveriesKeepTheirFingerprintAcrossAnOverChange(t *testing.T) {
	var sequence []matchLiveResponse
	loadJSON(t, "match_live_14F7_finish_sequence.json", &sequence)
	fixture := testFixture(t, "14F7", "2MT", "T20", knights, rocks)
	tracker := newStrikeTracker()
	// Reads 1 and 2 straddle the start of over 20 and a change of bowler.
	before, _ := reduce(t, sequence[1], fixture, tracker, time.Now())
	after, _ := reduce(t, sequence[2], fixture, tracker, time.Now())
	hashes := map[string]string{}
	for _, delivery := range before.Deliveries {
		hashes[delivery.ProviderEventID] = delivery.PayloadHash
	}
	for _, delivery := range after.Deliveries {
		if previous, ok := hashes[delivery.ProviderEventID]; ok && previous != delivery.PayloadHash {
			t.Fatalf("%s re-hashed between reads: the store would record a correction", delivery.ProviderEventID)
		}
	}
}

func TestMatchStateReadsOnlyWholePhrases(t *testing.T) {
	tests := []struct {
		live, status string
		want         string
	}{
		{"Review Cancelled", "Rocks need 13 runs in 7 balls", client.StateInProgress},
		{"Play called off", "", client.StateInProgress},
		{"No Result - Review", "Rocks need 13 runs in 7 balls", client.StateInProgress},
		{"Team Review", "", client.StateInProgress},
		{"Ball In Air", "", client.StateInProgress},
		{"Tea", "", "Tea"},
		{"Rain stopped play", "", client.StateRain},
		{"", "Match abandoned due to rain", client.StateAbandon},
		{"Match Abandoned", "", client.StateAbandon},
		{"", "No result", client.StateAbandon},
		{"4", "Rocks won by 5 wickets", client.StateComplete},
		{"Rocks won by 5 wickets 🏆", "", client.StateComplete},
		{"", "Rocks won the toss and chose to bowl", client.StateInProgress},
	}
	for _, test := range tests {
		got, _ := matchState(matchLiveResponse{LiveStatus: test.live, StatusText: test.status}, true)
		if got != test.want {
			t.Fatalf("livestatus %q / status %q -> %q, want %q", test.live, test.status, got, test.want)
		}
	}
}

func TestAbandonmentNeedsConfirmation(t *testing.T) {
	var tab matchLiveResponse
	loadJSON(t, "match_live_14F7.json", &tab)
	tab.LiveStatus, tab.StatusText = "", "Match abandoned due to rain"
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustJSON(t, tab))
	})
	clock := time.Date(2026, 9, 29, 19, 0, 0, 0, time.UTC)
	provider.now = func() time.Time { return clock }
	fixture := testFixture(t, "14F7", "2MT", "T20", knights, rocks)

	first, _, err := provider.MatchSnapshot(context.Background(), fixture)
	if err != nil {
		t.Fatal(err)
	}
	if state := first.Commentary.MiniScore.State; state != client.StateRain {
		t.Fatalf("first abandonment read = %q, want it held as %q", state, client.StateRain)
	}
	clock = clock.Add(time.Minute)
	second, _, err := provider.MatchSnapshot(context.Background(), fixture)
	if err != nil {
		t.Fatal(err)
	}
	if state := second.Commentary.MiniScore.State; state != client.StateAbandon {
		t.Fatalf("repeated abandonment = %q, want %q", state, client.StateAbandon)
	}
}

func TestRevisedChaseTargetHoldsTrading(t *testing.T) {
	var tab matchLiveResponse
	loadJSON(t, "match_live_14F7.json", &tab)
	fixture := testFixture(t, "14F7", "2MT", "T20", knights, rocks)

	// 148/4 after 18.5 overs chasing 160. "Need 30" puts the target at 178,
	// not 160 + 1: a rain rule has revised it.
	tab.StatusText = "Rocks need 30 runs in 7 balls"
	projection, _ := reduce(t, tab, fixture, newStrikeTracker(), time.Now())
	if !projection.ReducedOvers || projection.Target != 178 {
		t.Fatalf("revised target: reduced=%t target=%d, want held at 178", projection.ReducedOvers, projection.Target)
	}

	// Same target, but 113 balls bowled + 1 left is a 114-ball (19-over)
	// innings, not 120: the chase was shortened.
	tab.StatusText = "Rocks need 13 runs in 1 balls"
	projection, _ = reduce(t, tab, fixture, newStrikeTracker(), time.Now())
	if !projection.ReducedOvers || projection.ScheduledOvers != 19 {
		t.Fatalf("shortened chase: reduced=%t overs=%d, want held at 19 overs", projection.ReducedOvers, projection.ScheduledOvers)
	}

	// The recorded equation is consistent: nothing is held.
	tab.StatusText = "Rocks need 13 runs  in 7 balls"
	projection, _ = reduce(t, tab, fixture, newStrikeTracker(), time.Now())
	if projection.ReducedOvers || projection.Target != 161 {
		t.Fatalf("consistent chase: reduced=%t target=%d", projection.ReducedOvers, projection.Target)
	}
}

func TestBareScoreAtTheCreaseIsRefused(t *testing.T) {
	var tab matchLiveResponse
	loadJSON(t, "match_live_14F7.json", &tab)
	tab.BattingTeam.Score = "148"
	fixture := testFixture(t, "14F7", "2MT", "T20", knights, rocks)
	if _, err := translateLive(tab, fixture, teamsOf(fixture), 0, newStrikeTracker(), time.Now()); !errors.Is(err, ErrUnreadableScore) {
		t.Fatalf("err = %v, want ErrUnreadableScore", err)
	}
}

func TestSeriesLengthNeedsAQuorum(t *testing.T) {
	provider, _ := newTestProvider(t, func(http.ResponseWriter, *http.Request) {})
	series, _ := EncodeKey("2N8")
	provider.observeSeriesLength(series, "T20", 1, sideScore{121, 5, 10, true, false}, sideScore{112, 5, 10, true, false})
	if provider.seriesOvers(series) != 0 {
		t.Fatal("one short match is not enough to call the series short")
	}
	provider.observeSeriesLength(series, "T20", 2, sideScore{92, 6, 10, true, false})
	if provider.seriesOvers(series) != 10 {
		t.Fatalf("series overs = %d, want 10 after two matches", provider.seriesOvers(series))
	}
	provider.observeSeriesLength(series, "T20", 3, sideScore{175, 4, 20, true, false})
	if provider.seriesOvers(series) != 0 {
		t.Fatal("a full-length innings must clear the learned length")
	}

	// A series known to play ten overs holds a first innings with no grid.
	var tab matchLiveResponse
	loadJSON(t, "match_live_14D3_ten_overs.json", &tab)
	tab.ProjectedScore = tabProjectedGrid{}
	tab.BowlingTeam.Score, tab.BowlingTeam.Overs = "Yet To Bat", ""
	tab.BattingTeam.Score, tab.BattingTeam.Overs = "45-1", "5.0"
	tab.LiveStatus, tab.StatusText = "Ball", ""
	fixture := testFixture(t, "14D3", "2N8", "T20", side{"RH", "Haridwar Elmas", "HE"}, side{"RD", "USN Indians", "USN"})
	translated, err := translateLive(tab, fixture, teamsOf(fixture), 10, newStrikeTracker(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if translated.commentary.MiniScore.ScheduledOvers != 10 {
		t.Fatalf("scheduled overs = %d, want the series' 10", translated.commentary.MiniScore.ScheduledOvers)
	}
}

func TestMissingLiveDataBeforeStartIsNotAFailure(t *testing.T) {
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":{"status":"error","message":"Live data not found for match '14ND'"}}`))
	})
	fixture := testFixture(t, "14ND", "2N8", "T20", side{"1CR", "Rishikesh River Kings", "RRK"}, side{"RD", "USN Indians", "USN"})
	fixture.StartingAt = provider.now().Add(3 * time.Hour)
	snapshot, _, err := provider.MatchSnapshot(context.Background(), fixture)
	if err != nil || snapshot.Commentary.MiniScore.State != client.StatePreview {
		t.Fatalf("before start: state %q, err %v; want Preview", snapshot.Commentary.MiniScore.State, err)
	}
	fixture.StartingAt = provider.now().Add(-3 * time.Hour)
	if _, _, err := provider.MatchSnapshot(context.Background(), fixture); err == nil {
		t.Fatal("after the start a missing live record is still an error")
	}
}

func TestAssignInningsRefusesAmbiguousStrips(t *testing.T) {
	if got := assignInnings([]int{14, 15, 15, 16}, 2, 90, 20); got[0] != 0 || got[3] != 0 {
		t.Fatalf("repeated over label must drop the strip, got %v", got)
	}
	// The strip reached innings 2 before the scoreboard: the new over is
	// dropped, not filed under innings 1.
	got := assignInnings([]int{18, 19, 20, 1}, 1, 120, 0)
	if got[3] != 0 || got[2] != 1 || got[0] != 1 {
		t.Fatalf("got %v, want [1 1 1 0]", got)
	}
}

func TestReducedLengthIsNotTradable(t *testing.T) {
	var tab matchLiveResponse
	loadJSON(t, "match_live_14D3_ten_overs.json", &tab)
	fixture := testFixture(t, "14D3", "2N8", "T20", side{"RH", "Haridwar Elmas", "HE"}, side{"RD", "USN Indians", "USN"})
	projection, _ := reduce(t, tab, fixture, newStrikeTracker(), time.Now())
	if projection.Status != matches.StatusCompleted || !projection.ReducedOvers {
		t.Fatalf("status %s reduced %t", projection.Status, projection.ReducedOvers)
	}
}

// A wicket seen in one read and the new batter's first ball in the next: the
// newcomer was in before that ball, so it is credited to him.
func TestStrikeFollowsANewBatterWhoHasAlreadyFaced(t *testing.T) {
	tracker := newStrikeTracker()
	now := time.Now()
	ball := func(over, index int, token string, overEnd bool) trackedBall {
		return trackedBall{key: ballKey{over, index}, token: token, overEnd: overEnd}
	}
	balls := []trackedBall{ball(5, 1, "0", false)}
	tracker.resolve("M1", 2, balls, []crease{{"A", 10, 8}, {"B", 5, 4}}, now)
	balls = append(balls, ball(5, 2, "W", false))
	if got := tracker.resolve("M1", 2, balls, []crease{{"A", 10, 9}, {"B", 5, 4}}, now); got != "A" {
		t.Fatalf("after the wicket ball striker = %q, want A", got)
	}
	balls = append(balls, ball(5, 3, "1", false))
	if got := tracker.resolve("M1", 2, balls, []crease{{"C", 1, 1}, {"B", 5, 4}}, now); got != "B" {
		t.Fatalf("after the newcomer's single striker = %q, want B", got)
	}
}
