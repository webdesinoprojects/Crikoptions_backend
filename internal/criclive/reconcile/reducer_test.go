package reconcile

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

// Token semantics verified against live /cricket/overs payloads: a wide's digit
// is the total charged ("Wd2" is two runs in all), while a no-ball's digit is
// runs off the bat on top of the one-run penalty ("N4" is five).
func TestParseBallToken(t *testing.T) {
	cases := []struct {
		token   string
		total   int
		batter  int
		legal   bool
		wicket  bool
		wides   int
		noBalls int
		byes    int
		legByes int
	}{
		{token: "0", total: 0, batter: 0, legal: true},
		{token: "1", total: 1, batter: 1, legal: true},
		{token: "4", total: 4, batter: 4, legal: true},
		{token: "6", total: 6, batter: 6, legal: true},
		{token: "W", total: 0, batter: 0, legal: true, wicket: true},
		{token: "1W", total: 1, batter: 1, legal: true, wicket: true},
		{token: "Wd", total: 1, legal: false, wides: 1},
		{token: "Wd2", total: 2, legal: false, wides: 2},
		{token: "N", total: 1, batter: 0, legal: false, noBalls: 1},
		{token: "N4", total: 5, batter: 4, legal: false, noBalls: 1},
		{token: "L1", total: 1, legal: true, legByes: 1},
		{token: "B4", total: 4, legal: true, byes: 4},
	}
	for _, tc := range cases {
		outcome, err := ParseBallToken(tc.token)
		if err != nil {
			t.Fatalf("token %q: %v", tc.token, err)
		}
		if outcome.TotalRuns != tc.total || outcome.BatterRuns != tc.batter ||
			outcome.LegalBall != tc.legal || outcome.IsWicket != tc.wicket {
			t.Fatalf("token %q: total=%d batter=%d legal=%v wicket=%v",
				tc.token, outcome.TotalRuns, outcome.BatterRuns, outcome.LegalBall, outcome.IsWicket)
		}
		if outcome.Extras.Wides != tc.wides || outcome.Extras.NoBalls != tc.noBalls ||
			outcome.Extras.Byes != tc.byes || outcome.Extras.LegByes != tc.legByes {
			t.Fatalf("token %q extras = %+v", tc.token, outcome.Extras)
		}
	}
	if _, err := ParseBallToken("???"); !errors.Is(err, ErrUnknownBallToken) {
		t.Fatalf("unknown token error = %v", err)
	}
}

// Each over's tokens must add up to the over total CricLive reports.
func TestParseBallTokenMatchesReportedOverTotals(t *testing.T) {
	overs := []struct {
		balls []string
		runs  int
	}{
		{balls: []string{"0", "0", "0", "0", "0", "N", "0"}, runs: 1},
		{balls: []string{"2", "0", "N4", "4", "0", "0", "1"}, runs: 12},
		{balls: []string{"L1", "2", "0", "1", "0", "1"}, runs: 5},
		{balls: []string{"1", "1", "0", "0", "B4", "1"}, runs: 7},
		{balls: []string{"4", "0", "0", "W", "1", "0"}, runs: 5},
		{balls: []string{"1", "4", "W", "W", "1", "Wd", "1"}, runs: 8},
		{balls: []string{"6", "Wd", "0", "1", "Wd", "1", "4", "1"}, runs: 15},
		{balls: []string{"0", "Wd2", "L1", "1", "1", "W", "0"}, runs: 5},
	}
	for _, over := range overs {
		total, legal := 0, 0
		for _, token := range over.balls {
			outcome, err := ParseBallToken(token)
			if err != nil {
				t.Fatalf("token %q: %v", token, err)
			}
			total += outcome.TotalRuns
			if outcome.LegalBall {
				legal++
			}
		}
		if total != over.runs {
			t.Fatalf("over %v: runs=%d want %d", over.balls, total, over.runs)
		}
		if legal != 6 {
			t.Fatalf("over %v: legal balls=%d want 6", over.balls, legal)
		}
	}
}

// CricLive counts a completed over as ".6", so 38.6 is 39 whole overs.
func TestOversToBalls(t *testing.T) {
	cases := map[float64]int{
		0: 0, 0.1: 1, 1: 6, 1.3: 9, 33.5: 203, 38.6: 234, 49.6: 300, 20: 120,
	}
	for overs, want := range cases {
		if got := OversToBalls(overs); got != want {
			t.Fatalf("OversToBalls(%v) = %d, want %d", overs, got, want)
		}
	}
}

func TestClassifyFormatRejectsUnpriceableFormats(t *testing.T) {
	for _, raw := range []string{"T20", "t20i", "ODI"} {
		if _, _, err := ClassifyFormat(raw); err != nil {
			t.Fatalf("format %q rejected: %v", raw, err)
		}
	}
	// A Test innings has no fixed ball count, so it cannot be priced.
	for _, raw := range []string{"TEST", "Test", "first class", "The Hundred", "T10", ""} {
		if _, _, err := ClassifyFormat(raw); !errors.Is(err, ErrUnsupportedFormat) {
			t.Fatalf("format %q was accepted (err=%v)", raw, err)
		}
	}
}

// liveSnapshot mirrors a real second-innings T20 read: miniscore plus the
// recent overs window.
func liveSnapshot(t *testing.T) client.Snapshot {
	t.Helper()
	const commentaryJSON = `{
	  "success": true,
	  "data": {
	  "match_id": 169350,
	  "miniscore": {
	    "innings_id": 2, "bat_team_id": 11, "bat_team_score": 96, "bat_team_wickets": 3,
	    "status": "DBG need 45 runs in 42 balls", "overs": 12.6, "target": 141,
	    "crr": 7.38, "rrr": 6.42, "ovs_rem": "$undefined",
	    "recent_overs": "0 6 1 1 1 L1  | 1 4 1 W 0 0",
	    "last_wicket": "Emilio Gay b Razaullah 60(114) - 143/4 in 33.4 ov.",
	    "last_10_overs": {"runs": 74, "wickets": 2, "overs": ""},
	    "partnership": {"runs": 13, "balls": "32"},
	    "striker": {"id": 10385, "name": "Dan Lawrence", "runs": 5, "balls": 10, "fours": 0, "sixes": 0, "strike_rate": "50.00"},
	    "non_striker": {"id": 12201, "name": "Harry Brook", "runs": "52", "balls": 76, "fours": 4, "sixes": 0, "strike_rate": "68.42"},
	    "bowler_striker": {"id": 1470217, "name": "Razaullah", "overs": 2.6, "maidens": 0, "runs": 42, "wickets": 2, "economy": 4.67},
	    "bowler_non_striker": {"id": 11298, "name": "Mohammad Abbas", "overs": 2, "maidens": 1, "runs": 37, "wickets": 1, "economy": 2.64},
	    "innings_scores": [
	      {"innings_id": 1, "bat_team": "PAK", "score": 140, "wickets": 7, "overs": 19.6, "is_declared": false, "is_follow_on": false},
	      {"innings_id": 2, "bat_team": "RSA", "score": 96, "wickets": 3, "overs": 12.6, "is_declared": false, "is_follow_on": false}
	    ],
	    "match_format": "T20", "custom_status": "DBG need 45 runs in 42 balls", "state": "In Progress"
	  },
	  "match_header": {
	    "match_id": 169350, "format": "T20", "state": "In Progress",
	    "team1": {"id": 3, "name": "Pakistan", "short": "PAK"},
	    "team2": {"id": 11, "name": "South Africa", "short": "RSA"}
	  },
	  "commentary": [
	    {"timestamp": 1788976320944, "text": "good length ball", "ball_metric": 1, "innings_id": 2}
	  ]
	  }
	}`
	const oversJSON = `{
	  "success": true,
	  "data": {
	  "matchId": "169350", "innings": 2,
	  "overs": [
	    {"inningsId": 2, "overNumber": 13, "runs": 6, "score": 96, "wickets": 3,
	     "ovrSummary": "1 4 1 W 0 0", "balls": ["1","4","1","W","0","0"],
	     "batTeamName": "RSA", "batStrikerNames": ["Dan Lawrence"], "batNonStrikerNames": ["Harry Brook"],
	     "bowlNames": ["Razaullah"]},
	    {"inningsId": 2, "overNumber": 12, "runs": 10, "score": 90, "wickets": 2,
	     "ovrSummary": "0 6 1 1 1 L1", "balls": ["0","6","1","1","1","L1"],
	     "batTeamName": "RSA", "batStrikerNames": ["Harry Brook"], "batNonStrikerNames": ["Dan Lawrence"],
	     "bowlNames": ["Mohammad Abbas"]}
	  ]
	  }
	}`
	var commentary client.CommentaryResponse
	if err := json.Unmarshal([]byte(commentaryJSON), &commentary); err != nil {
		t.Fatalf("commentary fixture: %v", err)
	}
	var overs client.OversResponse
	if err := json.Unmarshal([]byte(oversJSON), &overs); err != nil {
		t.Fatalf("overs fixture: %v", err)
	}
	return client.Snapshot{
		MatchID: 169350,
		Fixture: client.Fixture{
			ID: 169350, SeriesID: 12870, Format: "T20",
			StartingAt:      time.Date(2026, 9, 9, 13, 15, 0, 0, time.UTC),
			LocalTeamID:     3,
			VisitorTeamID:   11,
			LocalTeamName:   "Pakistan",
			VisitorTeamName: "South Africa",
			LocalTeamShort:  "PAK", VisitorTeamShort: "RSA",
			State: "In Progress",
		},
		Commentary: commentary.Data,
		Overs:      overs.Data,
	}
}

func TestReduceSnapshotUsesMiniScoreAsAuthority(t *testing.T) {
	projection, err := ReduceSnapshot(liveSnapshot(t))
	if err != nil {
		t.Fatalf("ReduceSnapshot: %v", err)
	}
	if projection.FixtureID != 169350 || projection.LeagueID != 12870 {
		t.Fatalf("identity: fixture=%d series=%d", projection.FixtureID, projection.LeagueID)
	}
	if projection.Format != "T20" || projection.ScheduledBalls != 120 {
		t.Fatalf("format=%q scheduledBalls=%d", projection.Format, projection.ScheduledBalls)
	}
	if projection.Status != matches.StatusLive {
		t.Fatalf("status=%q", projection.Status)
	}
	if projection.CurrentInnings != 2 || projection.BattingTeamID != 11 {
		t.Fatalf("innings=%d battingTeam=%d", projection.CurrentInnings, projection.BattingTeamID)
	}
	// 12.6 overs is 13 completed overs = 78 legal balls.
	if projection.CurrentScore != 96 || projection.Wickets != 3 || projection.LegalBalls != 78 {
		t.Fatalf("score=%d/%d balls=%d", projection.CurrentScore, projection.Wickets, projection.LegalBalls)
	}
	if projection.Target != 141 {
		t.Fatalf("target=%d", projection.Target)
	}
	if len(projection.Innings) != 2 {
		t.Fatalf("innings count=%d", len(projection.Innings))
	}
	if projection.Innings[0].Runs != 140 || !projection.Innings[0].Complete {
		t.Fatalf("first innings = %+v", projection.Innings[0])
	}
	if projection.SnapshotHash == "" {
		t.Fatal("snapshot hash is empty")
	}
}

// The player names are the whole point of polling the commentary endpoint.
func TestReduceSnapshotCarriesOnFieldPlayers(t *testing.T) {
	projection, err := ReduceSnapshot(liveSnapshot(t))
	if err != nil {
		t.Fatalf("ReduceSnapshot: %v", err)
	}
	live := projection.LiveContext
	if live == nil {
		t.Fatal("live context is nil")
	}
	if live.Striker.Name != "Dan Lawrence" || live.Striker.Runs != 5 || live.Striker.Balls != 10 {
		t.Fatalf("striker = %+v", live.Striker)
	}
	// runs/balls arrive as a quoted string on this field; FlexInt must absorb it.
	if live.NonStriker.Name != "Harry Brook" || live.NonStriker.Runs != 52 || live.NonStriker.Balls != 76 {
		t.Fatalf("non-striker = %+v", live.NonStriker)
	}
	if live.Bowler.Name != "Razaullah" || live.Bowler.Wickets != 2 || live.Bowler.Balls != 18 {
		t.Fatalf("bowler = %+v", live.Bowler)
	}
	if live.Partnership.Runs != 13 || live.Partnership.Balls != 32 {
		t.Fatalf("partnership = %+v", live.Partnership)
	}
	if projection.MatchPulse == nil || projection.MatchPulse.LastWicket == "No wicket this over" {
		t.Fatalf("match pulse = %+v", projection.MatchPulse)
	}
}

func TestReduceSnapshotBuildsDeliveriesAndWindow(t *testing.T) {
	projection, err := ReduceSnapshot(liveSnapshot(t))
	if err != nil {
		t.Fatalf("ReduceSnapshot: %v", err)
	}
	if len(projection.Deliveries) != 12 {
		t.Fatalf("deliveries=%d want 12", len(projection.Deliveries))
	}
	// The window floor must be the oldest over supplied, not the newest.
	if projection.DeliveryWindowInnings != 2 || projection.DeliveryWindowSequence != 1201 {
		t.Fatalf("window = innings %d sequence %d", projection.DeliveryWindowInnings, projection.DeliveryWindowSequence)
	}
	ids := make(map[string]struct{}, len(projection.Deliveries))
	for _, delivery := range projection.Deliveries {
		if _, duplicate := ids[delivery.ProviderEventID]; duplicate {
			t.Fatalf("duplicate delivery id %s", delivery.ProviderEventID)
		}
		ids[delivery.ProviderEventID] = struct{}{}
		if delivery.BowlerName == "" || delivery.BatterName == "" {
			t.Fatalf("delivery %s lost player names: %+v", delivery.ProviderEventID, delivery)
		}
	}
	// Deliveries must be ordered oldest first so the replay steps forward.
	if projection.Deliveries[0].Sequence >= projection.Deliveries[len(projection.Deliveries)-1].Sequence {
		t.Fatal("deliveries are not in ascending sequence order")
	}
	// The strip shows the over in progress (over 13, index 12).
	if len(projection.ThisOver) != 6 {
		t.Fatalf("thisOver=%d want 6", len(projection.ThisOver))
	}
	if !projection.ThisOver[3].IsWicket {
		t.Fatalf("fourth ball of the current over should be a wicket: %+v", projection.ThisOver)
	}
}

func TestReduceSnapshotRejectsUnpriceableSnapshots(t *testing.T) {
	snapshot := liveSnapshot(t)
	snapshot.Fixture.Format = "TEST"
	snapshot.Commentary.MiniScore.MatchFormat = "TEST"
	if _, err := ReduceSnapshot(snapshot); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("TEST format error = %v", err)
	}

	snapshot = liveSnapshot(t)
	snapshot.Commentary.MiniScore.InningsID = 3
	if _, err := ReduceSnapshot(snapshot); !errors.Is(err, ErrSuperOver) {
		t.Fatalf("third innings error = %v", err)
	}

	snapshot = liveSnapshot(t)
	snapshot.Commentary.MiniScore.BatTeamID = 999
	if _, err := ReduceSnapshot(snapshot); !errors.Is(err, ErrIncompleteSnapshot) {
		t.Fatalf("foreign batting team error = %v", err)
	}
}

// A snapshot that has not changed must hash identically, or every poll would
// look like a correction.
func TestReduceSnapshotHashIsStable(t *testing.T) {
	first, err := ReduceSnapshot(liveSnapshot(t))
	if err != nil {
		t.Fatalf("ReduceSnapshot: %v", err)
	}
	second, err := ReduceSnapshot(liveSnapshot(t))
	if err != nil {
		t.Fatalf("ReduceSnapshot: %v", err)
	}
	if first.SnapshotHash != second.SnapshotHash {
		t.Fatalf("hash drift: %s vs %s", first.SnapshotHash, second.SnapshotHash)
	}
	changed := liveSnapshot(t)
	changed.Commentary.MiniScore.BatTeamScore = 97
	third, err := ReduceSnapshot(changed)
	if err != nil {
		t.Fatalf("ReduceSnapshot: %v", err)
	}
	if third.SnapshotHash == first.SnapshotHash {
		t.Fatal("score change did not alter the snapshot hash")
	}
}

// The store persists the series id as providerSeasonId and rejects any later
// projection whose SeasonID differs. Leaving it zero made every re-poll of an
// existing match fail as "identity changed" — silently, for days.
func TestReduceSnapshotCarriesSeriesAsSeasonForIdentityCheck(t *testing.T) {
	projection, err := ReduceSnapshot(liveSnapshot(t))
	if err != nil {
		t.Fatalf("ReduceSnapshot: %v", err)
	}
	if projection.SeasonID != 12870 || projection.LeagueID != 12870 {
		t.Fatalf("season/league = %d/%d, want both 12870", projection.SeasonID, projection.LeagueID)
	}
}

// During play only /cricket/overs is polled. The reducer must produce the same
// projection from the overs feed plus discovery's innings summary that it would
// from a commentary read — score from the latest over, players at the crease,
// the chase target, and the ball strip — through the one shared path.
func TestReduceSnapshotFromOversOnlyDuringPlay(t *testing.T) {
	base := liveSnapshot(t)
	snapshot := client.Snapshot{
		MatchID: 169350,
		Fixture: client.Fixture{
			ID: 169350, SeriesID: 12870, Format: "T20",
			StartingAt:  time.Date(2026, 9, 9, 13, 15, 0, 0, time.UTC),
			LocalTeamID: 3, VisitorTeamID: 11,
			LocalTeamName: "Pakistan", VisitorTeamName: "South Africa",
			LocalTeamShort: "PAK", VisitorTeamShort: "RSA",
			State: client.StateInProgress,
			LiveInnings: []client.FixtureInnings{
				{Number: 1, TeamID: 3, Runs: 140, Wickets: 7, Overs: 19.6},
				// Discovery is a minute stale: the ball feed is ahead of it.
				{Number: 2, TeamID: 11, Runs: 90, Wickets: 2, Overs: 12, Target: 141},
			},
		},
		Overs: base.Overs, // no Commentary at all
	}
	projection, err := ReduceSnapshot(snapshot)
	if err != nil {
		t.Fatalf("ReduceSnapshot(overs only): %v", err)
	}
	if projection.Status != matches.StatusLive || projection.ProviderState != client.StateInProgress {
		t.Fatalf("status=%q state=%q", projection.Status, projection.ProviderState)
	}
	// The latest over (13: score 96/3, six balls) is fresher than discovery's 90/2.
	if projection.CurrentInnings != 2 || projection.BattingTeamID != 11 {
		t.Fatalf("innings=%d batting=%d", projection.CurrentInnings, projection.BattingTeamID)
	}
	if projection.CurrentScore != 96 || projection.Wickets != 3 || projection.LegalBalls != 78 {
		t.Fatalf("score=%d/%d balls=%d, want 96/3 and 78", projection.CurrentScore, projection.Wickets, projection.LegalBalls)
	}
	if projection.Target != 141 {
		t.Fatalf("target=%d want 141 (from discovery summary)", projection.Target)
	}
	if len(projection.Innings) != 2 || projection.Innings[0].Runs != 140 || projection.Innings[0].BattingTeamID != 3 || !projection.Innings[0].Complete {
		t.Fatalf("first innings = %+v", projection.Innings[0])
	}
	live := projection.LiveContext
	if live == nil || live.Striker.Name != "Dan Lawrence" || live.NonStriker.Name != "Harry Brook" || live.Bowler.Name != "Razaullah" {
		t.Fatalf("players from latest over = %+v", live)
	}
	if len(projection.ThisOver) != 6 || !projection.ThisOver[3].IsWicket {
		t.Fatalf("ball strip = %+v", projection.ThisOver)
	}
	if projection.MatchPulse == nil || projection.MatchPulse.LastWicket != "Wicket this over" {
		t.Fatalf("pulse = %+v", projection.MatchPulse)
	}
	// Partnership since the wicket in over 13: balls 5-6 were 0, 0.
	if live.Partnership.Runs != 0 || live.Partnership.Balls != 2 {
		t.Fatalf("partnership = %+v, want 0 off 2", live.Partnership)
	}
	if projection.SeasonID != 12870 {
		t.Fatalf("season=%d", projection.SeasonID)
	}
}

func TestOversNotationUsesDottedSix(t *testing.T) {
	cases := []struct {
		over, legal int
		want        float64
	}{{13, 3, 12.3}, {13, 6, 12.6}, {1, 0, 0}, {1, 1, 0.1}, {0, 4, 0}}
	for _, tc := range cases {
		if got := oversNotation(tc.over, tc.legal); got != tc.want {
			t.Fatalf("oversNotation(%d,%d)=%v want %v", tc.over, tc.legal, got, tc.want)
		}
	}
	if OversToBalls(oversNotation(13, 6)) != 78 {
		t.Fatal("dotted-six notation must round-trip to 78 legal balls")
	}
}

// The polling target carries team ids but no names. Innings discovery reported
// must keep their batting side by id rather than lose it in a label round trip
// with nothing to match against: the UI picks the batting side from it.
func TestOversOnlyKeepsBattingSideWithoutTeamNames(t *testing.T) {
	base := liveSnapshot(t)
	snapshot := client.Snapshot{
		MatchID: 169350,
		Fixture: client.Fixture{
			ID: 169350, SeriesID: 12870, Format: "T20", LocalTeamID: 3, VisitorTeamID: 11,
			State: client.StateInProgress,
			LiveInnings: []client.FixtureInnings{
				{Number: 1, TeamID: 3, Runs: 140, Wickets: 7, Overs: 19.6},
				{Number: 2, TeamID: 11, Runs: 90, Wickets: 2, Overs: 12, Target: 141},
			},
		},
		Overs: base.Overs,
	}
	projection, err := ReduceSnapshot(snapshot)
	if err != nil {
		t.Fatalf("ReduceSnapshot: %v", err)
	}
	if len(projection.Innings) != 2 || projection.Innings[0].BattingTeamID != 3 || projection.Innings[1].BattingTeamID != 11 {
		t.Fatalf("innings batting sides = %+v", projection.Innings)
	}
	for _, delivery := range projection.Deliveries {
		if delivery.TeamID != 11 {
			t.Fatalf("delivery %s stamped with team %d", delivery.ProviderEventID, delivery.TeamID)
		}
	}
}

// The first ball of the match can land before discovery has listed an innings
// at all. The over names the batting side; that must be enough to price it.
func TestOversOnlyResolvesFirstInningsBattingSideFromTheOver(t *testing.T) {
	snapshot := client.Snapshot{
		MatchID: 169350,
		Fixture: client.Fixture{
			ID: 169350, SeriesID: 12870, Format: "T20", LocalTeamID: 3, VisitorTeamID: 11,
			LocalTeamShort: "PAK", VisitorTeamShort: "RSA", State: client.StateInProgress,
		},
		Overs: client.OversData{Innings: 1, Overs: []client.OverItem{{
			InningsID: 1, OverNumber: 1, Runs: 4, Score: 4, Balls: []string{"0", "4"}, BatTeamName: "PAK",
			BatStrikerNames: []string{"Saim Ayub"}, BatStrikerRuns: 4, BatStrikerBalls: 2,
			BatNonStrikerNames: []string{"Fakhar Zaman"}, BowlNames: []string{"Kagiso Rabada"}, BowlOvers: 0.2, BowlRuns: 4,
		}}},
	}
	projection, err := ReduceSnapshot(snapshot)
	if err != nil {
		t.Fatalf("ReduceSnapshot(first over, no summary): %v", err)
	}
	if projection.CurrentInnings != 1 || projection.BattingTeamID != 3 {
		t.Fatalf("innings=%d batting=%d, want innings 1 batted by PAK (3)", projection.CurrentInnings, projection.BattingTeamID)
	}
	if projection.CurrentScore != 4 || projection.LegalBalls != 2 || len(projection.Deliveries) != 2 {
		t.Fatalf("score=%d balls=%d deliveries=%d", projection.CurrentScore, projection.LegalBalls, len(projection.Deliveries))
	}
	if projection.LiveContext == nil || projection.LiveContext.Striker.Name != "Saim Ayub" {
		t.Fatalf("live context = %+v", projection.LiveContext)
	}
}

// Right after an innings change discovery already reports the new innings
// while the overs feed still shows the last over of the previous one. Those
// batters, that bowler and that score do not belong to the new innings.
func TestOversOnlyIgnoresPreviousInningsOversAfterInningsChange(t *testing.T) {
	snapshot := client.Snapshot{
		MatchID: 169350,
		Fixture: client.Fixture{
			ID: 169350, SeriesID: 12870, Format: "T20", LocalTeamID: 3, VisitorTeamID: 11,
			LocalTeamShort: "PAK", VisitorTeamShort: "RSA", State: client.StateInProgress,
			LiveInnings: []client.FixtureInnings{
				{Number: 1, TeamID: 3, Runs: 140, Wickets: 7, Overs: 19.6},
				{Number: 2, TeamID: 11, Target: 141},
			},
		},
		Overs: client.OversData{Innings: 1, Overs: []client.OverItem{{
			InningsID: 1, OverNumber: 20, Runs: 8, Score: 140, Wickets: 7, Balls: []string{"1", "4", "W", "1", "2", "0"},
			BatTeamName: "PAK", BatStrikerNames: []string{"Shaheen Afridi"}, BowlNames: []string{"Kagiso Rabada"},
		}}},
	}
	projection, err := ReduceSnapshot(snapshot)
	if err != nil {
		t.Fatalf("ReduceSnapshot(lagging overs): %v", err)
	}
	if projection.CurrentInnings != 2 || projection.BattingTeamID != 11 || projection.Target != 141 {
		t.Fatalf("innings=%d batting=%d target=%d", projection.CurrentInnings, projection.BattingTeamID, projection.Target)
	}
	if projection.CurrentScore != 0 || projection.Wickets != 0 || projection.LegalBalls != 0 {
		t.Fatalf("new innings scored from the old one: %d/%d off %d balls", projection.CurrentScore, projection.Wickets, projection.LegalBalls)
	}
	if projection.LiveContext != nil {
		t.Fatalf("previous innings players carried into the new innings: %+v", projection.LiveContext)
	}
	if len(projection.Innings) != 2 || !projection.Innings[0].Complete || projection.Innings[0].BattingTeamID != 3 {
		t.Fatalf("first innings = %+v", projection.Innings[0])
	}
	for _, delivery := range projection.Deliveries {
		if delivery.Innings != 1 || delivery.TeamID != 3 {
			t.Fatalf("delivery %s innings=%d team=%d, want innings 1 by PAK", delivery.ProviderEventID, delivery.Innings, delivery.TeamID)
		}
	}
}

func lumpedOver(number int, names []string, sRuns, sBalls, nRuns, nBalls int) client.OverItem {
	return client.OverItem{
		InningsID: 2, OverNumber: client.FlexFloat(number), Balls: []string{"0"},
		BatStrikerNames: names, BatStrikerRuns: client.FlexInt(sRuns), BatStrikerBalls: client.FlexInt(sBalls),
		BatNonStrikerRuns: client.FlexInt(nRuns), BatNonStrikerBalls: client.FlexInt(nBalls),
	}
}

func splitOver(number int, striker string, sRuns, sBalls int, nonStriker string, nRuns, nBalls int) client.OverItem {
	over := lumpedOver(number, []string{striker}, sRuns, sBalls, nRuns, nBalls)
	over.BatNonStrikerNames = []string{nonStriker}
	return over
}

// /cricket/overs/129596 innings 2, overs 30-39 as captured on 9 Sep 2026.
// CricLive lumps both batters under batStrikerNames in most overs; the striker
// figures belong to the second name in over 32 and the first in over 33.
func TestBattersAtCreaseFollowsFiguresThroughLumpedOvers(t *testing.T) {
	overs := []client.OverItem{
		splitOver(30, "Emilio Gay", 51, 98, "Harry Brook", 40, 47),
		splitOver(31, "Emilio Gay", 52, 104, "Harry Brook", 40, 47),
		lumpedOver(32, []string{"Harry Brook", "Emilio Gay"}, 53, 107, 43, 50),
		lumpedOver(33, []string{"Harry Brook", "Emilio Gay"}, 45, 53, 56, 110),
		lumpedOver(34, []string{"Harry Brook", "Emilio Gay", "Dan Lawrence"}, 45, 54, 1, 1),
		lumpedOver(35, []string{"Harry Brook", "Dan Lawrence"}, 48, 59, 2, 2),
		lumpedOver(36, []string{"Harry Brook", "Dan Lawrence"}, 2, 5, 49, 62),
		splitOver(37, "Harry Brook", 49, 69, "Dan Lawrence", 2, 5),
		lumpedOver(38, []string{"Harry Brook", "Dan Lawrence"}, 5, 10, 50, 70),
		splitOver(39, "Harry Brook", 52, 76, "Dan Lawrence", 5, 10),
	}
	for _, tc := range []struct {
		upTo                int
		striker, nonStriker string
	}{
		{32, "Emilio Gay", "Harry Brook"},
		{33, "Harry Brook", "Emilio Gay"},
		{34, "Harry Brook", "Dan Lawrence"}, // Gay dismissed; the new name is the new batter
		{36, "Dan Lawrence", "Harry Brook"},
		{38, "Dan Lawrence", "Harry Brook"}, // agrees with the explicit over 39 that follows
		{39, "Harry Brook", "Dan Lawrence"},
	} {
		window := overs[:tc.upTo-29]
		// Newest first, as the provider sends them.
		reversed := make([]client.OverItem, 0, len(window))
		for i := len(window) - 1; i >= 0; i-- {
			reversed = append(reversed, window[i])
		}
		striker, nonStriker := battersAtCrease(oversOfInnings(client.OversData{Overs: reversed}, 2))
		if striker != tc.striker || nonStriker != tc.nonStriker {
			t.Fatalf("through over %d: striker=%q nonStriker=%q, want %q/%q", tc.upTo, striker, nonStriker, tc.striker, tc.nonStriker)
		}
	}
}

// /cricket/overs/171240 innings 2, overs 7-17 as captured on 12 Sep 2026: not
// one over in the window names the roles. The wicket in over 10 (a name new to
// the list, figures that continue nobody) is what binds the names; from there
// the figures say Rahul Kumar is on strike at the end, not the first-listed
// Abhay Choudhary.
func TestBattersAtCreaseBindsNamesAtAWicket(t *testing.T) {
	both := []string{"Abhay Choudhary", "Rahul Kumar"}
	openers := []string{"Naman Dhir", "Abhay Choudhary"}
	overs := []client.OverItem{
		lumpedOver(7, openers, 68, 27, 5, 7),
		lumpedOver(8, openers, 78, 31, 7, 9),
		lumpedOver(9, openers, 15, 14, 79, 32),
		lumpedOver(10, openers, 91, 37, 16, 15),
		lumpedOver(11, both, 22, 19, 1, 2),
		lumpedOver(12, both, 3, 5, 25, 22),
		lumpedOver(13, both, 32, 26, 4, 7),
		lumpedOver(14, both, 39, 30, 6, 9),
		lumpedOver(15, both, 20, 13, 41, 32),
		lumpedOver(16, both, 47, 36, 22, 15),
		lumpedOver(17, both, 26, 16, 48, 38),
	}
	striker, nonStriker := battersAtCrease(overs)
	if striker != "Rahul Kumar" || nonStriker != "Abhay Choudhary" {
		t.Fatalf("striker=%q nonStriker=%q, want Rahul Kumar on strike", striker, nonStriker)
	}
	// A window with neither an explicit over nor a wicket cannot be resolved;
	// both names are still reported, in list order.
	striker, nonStriker = battersAtCrease(overs[5:])
	if striker != "Abhay Choudhary" || nonStriker != "Rahul Kumar" {
		t.Fatalf("unresolved window: striker=%q nonStriker=%q, want list order", striker, nonStriker)
	}
}
