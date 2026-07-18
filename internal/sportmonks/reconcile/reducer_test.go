package reconcile

import (
	"errors"
	"strings"
	"testing"
)

const scoreCatalogJSON = `{
  "data": [
    {"id":1,"name":"1 Run","runs":1,"ball":true,"is_wicket":false,"out":false},
    {"id":2,"name":"Wide","runs":1,"ball":false,"is_wicket":false,"out":false},
    {"id":3,"name":"Wicket Bowled","runs":0,"ball":true,"is_wicket":true,"out":true},
    {"id":4,"name":"Four","runs":4,"ball":true,"four":true,"is_wicket":false,"out":false},
    {"id":5,"name":"1 Bye","runs":0,"bye":1,"ball":true,"is_wicket":false,"out":false},
    {"id":6,"name":"2 Leg Bye","runs":0,"leg_bye":2,"ball":true,"is_wicket":false,"out":false},
    {"id":7,"name":"No Ball + 4 Bye","runs":1,"bye":4,"noball":1,"noball_runs":0,"ball":false,"is_wicket":false,"out":false},
    {"id":8,"name":"5 Penalty","runs":0,"ball":true,"is_wicket":false,"out":false},
    {"id":9,"name":"2 No Balls","runs":2,"noball":1,"noball_runs":2,"ball":false,"is_wicket":false,"out":false},
    {"id":10,"name":"1 Leg Bye + 4 Runs","runs":4,"leg_bye":5,"ball":true,"is_wicket":false,"out":false}
  ]
}`

func mustCatalog(t *testing.T) Catalog {
	t.Helper()
	catalog, err := CatalogFromJSON([]byte(scoreCatalogJSON))
	if err != nil {
		t.Fatalf("CatalogFromJSON: %v", err)
	}
	return catalog
}

func TestCatalogRejectsMissingSemanticsAndDuplicateIDs(t *testing.T) {
	for _, raw := range []string{
		`{"data":[{"id":1,"name":"Unknown","runs":0}]}`,
		`{"data":[{"id":1,"name":"Dot","runs":0,"ball":true,"is_wicket":false,"out":false},{"id":1,"name":"One","runs":1,"ball":true,"is_wicket":false,"out":false}]}`,
	} {
		if _, err := CatalogFromJSON([]byte(raw)); err == nil {
			t.Fatalf("invalid catalog accepted: %s", raw)
		}
	}
}

func fixtureJSON(score, wickets int, overs string, secondScoreID int) string {
	return `{
  "id":99,"league_id":7,"season_id":8,"localteam_id":10,"visitorteam_id":11,
  "type":"T20","status":"1st Innings","starting_at":"2026-07-16T10:00:00Z",
  "localteam":{"data":{"id":10,"name":"Alpha"}},
  "visitorteam":{"data":{"id":11,"name":"Beta"}},
  "balls":{"data":[
    {"id":1001,"fixture_id":99,"team_id":10,"ball":"0.1","batsman_id":20,"bowler_id":30,"scoreboard":"S1","score_id":2,"updated_at":"2026-07-16T10:01:00Z"},
    {"id":1002,"fixture_id":99,"team_id":10,"ball":"0.1","batsman_id":20,"bowler_id":30,"scoreboard":"S1","score_id":` + itoa(secondScoreID) + `,"updated_at":"2026-07-16T10:01:05Z"}
  ]},
  "runs":{"data":[{"inning":1,"team_id":10,"score":` + itoa(score) + `,"wickets":` + itoa(wickets) + `,"overs":"` + overs + `"}]},
  "scoreboards":{"data":[{"scoreboard":"S1","team_id":10,"type":"total","total":` + itoa(score) + `,"wickets":` + itoa(wickets) + `,"overs":"` + overs + `"}]},
  "batting":{"data":[]},"bowling":{"data":[]}
}`
}

func TestReduceUsesStableBallIDAndAllowsDuplicateNotation(t *testing.T) {
	projection, err := ReduceFixtureJSON([]byte(fixtureJSON(2, 0, "0.1", 1)), mustCatalog(t))
	if err != nil {
		t.Fatalf("ReduceFixtureJSON: %v", err)
	}
	if len(projection.Deliveries) != 2 {
		t.Fatalf("deliveries=%d want 2", len(projection.Deliveries))
	}
	first, second := projection.Deliveries[0], projection.Deliveries[1]
	if first.ProviderBall != "0.1" || second.ProviderBall != "0.1" {
		t.Fatalf("provider notation changed: %q %q", first.ProviderBall, second.ProviderBall)
	}
	if first.ProviderEventID == second.ProviderEventID {
		t.Fatal("stable provider event IDs must differ")
	}
	if first.LegalBall || !second.LegalBall {
		t.Fatalf("legal flags = %v,%v", first.LegalBall, second.LegalBall)
	}
	if projection.CurrentScore != 2 || projection.LegalBalls != 1 || projection.ScheduledBalls != 120 {
		t.Fatalf("projection=%+v", projection)
	}
	if projection.LocalTeamName != "Alpha" || projection.VisitorTeamName != "Beta" {
		t.Fatalf("team names=%q/%q", projection.LocalTeamName, projection.VisitorTeamName)
	}
}

func TestInningsBreakDerivesTargetFromCompletedFirstInnings(t *testing.T) {
	balls := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		over, ball := i/6, i%6+1
		balls = append(balls, `{"id":`+itoa(2000+i)+`,"fixture_id":99,"team_id":10,"ball":"`+itoa(over)+`.`+itoa(ball)+`","batsman_id":20,"bowler_id":30,"scoreboard":"S1","score_id":3}`)
	}
	raw := `{
  "id":99,"league_id":7,"season_id":8,"localteam_id":10,"visitorteam_id":11,
  "type":"T20","status":"Innings Break","starting_at":"2026-07-16T10:00:00Z",
  "balls":{"data":[` + strings.Join(balls, ",") + `]},
  "runs":{"data":[{"inning":1,"team_id":10,"score":0,"wickets":10,"overs":"1.4"}]},
  "scoreboards":{"data":[{"scoreboard":"S1","team_id":10,"type":"total","total":0,"wickets":10,"overs":"1.4"}]},
  "batting":{"data":[]},"bowling":{"data":[]}
}`
	projection, err := ReduceFixtureJSON([]byte(raw), mustCatalog(t))
	if err != nil {
		t.Fatalf("ReduceFixtureJSON: %v", err)
	}
	if projection.Status != "innings_break" || projection.Target != 1 {
		t.Fatalf("break projection status/target = %s/%d", projection.Status, projection.Target)
	}
}

func TestDeliverySequenceIsStableWhenRelationOrderChanges(t *testing.T) {
	raw := fixtureJSON(2, 0, "0.1", 1)
	first := `{"id":1001,"fixture_id":99,"team_id":10,"ball":"0.1","batsman_id":20,"bowler_id":30,"scoreboard":"S1","score_id":2,"updated_at":"2026-07-16T10:01:00Z"}`
	second := `{"id":1002,"fixture_id":99,"team_id":10,"ball":"0.1","batsman_id":20,"bowler_id":30,"scoreboard":"S1","score_id":1,"updated_at":"2026-07-16T10:01:05Z"}`
	reversed := strings.Replace(raw, first+",\n    "+second, second+",\n    "+first, 1)
	projection, err := ReduceFixtureJSON([]byte(reversed), mustCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	if projection.Deliveries[0].ProviderEventID != "1001" || projection.Deliveries[0].Sequence != 1 ||
		projection.Deliveries[1].ProviderEventID != "1002" || projection.Deliveries[1].Sequence != 2 {
		t.Fatalf("canonical deliveries = %+v", projection.Deliveries)
	}
}

func TestReduceRejectsUnknownScoreAndAggregateDrift(t *testing.T) {
	_, err := ReduceFixtureJSON([]byte(fixtureJSON(2, 0, "0.1", 999)), mustCatalog(t))
	if !errors.Is(err, ErrUnknownScore) {
		t.Fatalf("unknown score error=%v", err)
	}

	_, err = ReduceFixtureJSON([]byte(fixtureJSON(9, 0, "0.1", 1)), mustCatalog(t))
	if !errors.Is(err, ErrAggregateDrift) {
		t.Fatalf("drift error=%v", err)
	}
}

func TestReduceRejectsDuplicateDeliveryID(t *testing.T) {
	raw := strings.Replace(fixtureJSON(2, 0, "0.1", 1), `"id":1002`, `"id":1001`, 1)
	_, err := ReduceFixtureJSON([]byte(raw), mustCatalog(t))
	if !errors.Is(err, ErrIncompleteSnapshot) {
		t.Fatalf("duplicate delivery error = %v", err)
	}
}

func TestReduceRejectsIncompleteRelationAndUnsupportedFormat(t *testing.T) {
	raw := []byte(`{"id":1,"league_id":7,"season_id":8,"localteam_id":10,"visitorteam_id":11,"starting_at":"2026-07-16T10:00:00Z","status":"NS","type":"Test","balls":[],"runs":[],"scoreboards":[],"batting":[],"bowling":[]}`)
	_, err := ReduceFixtureJSON(raw, mustCatalog(t))
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("unsupported error=%v", err)
	}

	raw = []byte(`{
      "id":1,"league_id":7,"season_id":8,"localteam_id":10,"visitorteam_id":11,
      "type":"T20","status":"1st Innings","starting_at":"2026-07-16T10:00:00Z",
      "balls":{"data":[],"pagination":{"current_page":1,"total_pages":2}},
      "runs":{"data":[]},"scoreboards":{"data":[]},"batting":{"data":[]},"bowling":{"data":[]}
    }`)
	_, err = ReduceFixtureJSON(raw, mustCatalog(t))
	if !errors.Is(err, ErrIncompleteSnapshot) {
		t.Fatalf("incomplete error=%v", err)
	}
}

func TestReduceRejectsMalformedOrLinkedRelationPagination(t *testing.T) {
	malformed := strings.Replace(fixtureJSON(2, 0, "0.1", 1), `"balls":{"data":[`, `"balls":{"pagination":true,"data":[`, 1)
	if _, err := ReduceFixtureJSON([]byte(malformed), mustCatalog(t)); !errors.Is(err, ErrIncompleteSnapshot) {
		t.Fatalf("malformed pagination error = %v", err)
	}
	partial := strings.Replace(fixtureJSON(2, 0, "0.1", 1), `"balls":{"data":[`, `"balls":{"pagination":{"current_page":1,"total_pages":1,"links":{"next":"https://provider/page/2"}},"data":[`, 1)
	if _, err := ReduceFixtureJSON([]byte(partial), mustCatalog(t)); !errors.Is(err, ErrIncompleteSnapshot) {
		t.Fatalf("linked next page error = %v", err)
	}
	terminal := strings.Replace(fixtureJSON(2, 0, "0.1", 1), `"balls":{"data":[`, `"balls":{"pagination":{"current_page":1,"total_pages":1,"links":{"next":null}},"data":[`, 1)
	if _, err := ReduceFixtureJSON([]byte(terminal), mustCatalog(t)); err != nil {
		t.Fatalf("terminal pagination rejected: %v", err)
	}
}

func TestReduceRequiresAggregateLegalBallCounts(t *testing.T) {
	raw := strings.ReplaceAll(fixtureJSON(2, 0, "0.1", 1), `,"overs":"0.1"`, "")
	if _, err := ReduceFixtureJSON([]byte(raw), mustCatalog(t)); !errors.Is(err, ErrIncompleteSnapshot) {
		t.Fatalf("missing aggregate overs error = %v", err)
	}
}

func TestReduceRejectsStructuredDLSData(t *testing.T) {
	raw := strings.Replace(
		fixtureJSON(2, 0, "0.1", 1),
		`"type":"T20"`,
		`"type":"T20","localteam_dl_data":{"score":80,"overs":10,"wickets_out":2}`,
		1,
	)
	_, err := ReduceFixtureJSON([]byte(raw), mustCatalog(t))
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("DLS error=%v", err)
	}
}

func TestReduceRejectsThirdInnings(t *testing.T) {
	raw := strings.Replace(fixtureJSON(2, 0, "0.1", 1), `"status":"1st Innings"`, `"status":"3rd Innings"`, 1)
	_, err := ReduceFixtureJSON([]byte(raw), mustCatalog(t))
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("third innings error=%v", err)
	}
}

func TestSecondInningsReachingStructuredTargetIsComplete(t *testing.T) {
	raw := []byte(`{
      "id":101,"league_id":7,"season_id":8,"localteam_id":10,"visitorteam_id":11,
      "type":"T20","status":"2nd Innings","starting_at":"2026-07-16T10:00:00Z","target":2,
      "balls":{"data":[
        {"id":1,"team_id":10,"ball":"0.1","scoreboard":"S1","score_id":1},
        {"id":2,"team_id":11,"ball":"0.1","scoreboard":"S2","score_id":1},
        {"id":3,"team_id":11,"ball":"0.2","scoreboard":"S2","score_id":1}
      ]},
      "runs":{"data":[
        {"inning":1,"team_id":10,"score":1,"wickets":0,"overs":"0.1"},
        {"inning":2,"team_id":11,"score":2,"wickets":0,"overs":"0.2"}
      ]},
      "scoreboards":{"data":[
        {"scoreboard":"S1","team_id":10,"type":"total","total":1,"wickets":0,"overs":"0.1"},
        {"scoreboard":"S2","team_id":11,"type":"total","total":2,"wickets":0,"overs":"0.2"}
      ]},
      "batting":{"data":[]},"bowling":{"data":[]}
    }`)
	projection, err := ReduceFixtureJSON(raw, mustCatalog(t))
	if err != nil {
		t.Fatalf("ReduceFixtureJSON: %v", err)
	}
	if projection.CurrentInnings != 2 || len(projection.Innings) != 2 || !projection.Innings[1].Complete || projection.Innings[1].Target != 2 {
		t.Fatalf("target chase projection=%+v", projection)
	}
}

func TestCorrectionChangesDeliveryAndSnapshotHashes(t *testing.T) {
	one, err := ReduceFixtureJSON([]byte(fixtureJSON(2, 0, "0.1", 1)), mustCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	corrected, err := ReduceFixtureJSON([]byte(fixtureJSON(5, 0, "0.1", 4)), mustCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	if one.Deliveries[1].ProviderEventID != corrected.Deliveries[1].ProviderEventID {
		t.Fatal("correction changed stable provider ID")
	}
	if one.Deliveries[1].PayloadHash == corrected.Deliveries[1].PayloadHash || one.SnapshotHash == corrected.SnapshotHash {
		t.Fatal("correction must change delivery and snapshot hashes")
	}
}

func TestProviderTimestampOnlyChangeDoesNotResetMovementHash(t *testing.T) {
	original := fixtureJSON(2, 0, "0.1", 1)
	updated := strings.ReplaceAll(original, "2026-07-16T10:01:00Z", "2026-07-16T10:02:00Z")
	updated = strings.ReplaceAll(updated, "2026-07-16T10:01:05Z", "2026-07-16T10:02:05Z")

	one, err := ReduceFixtureJSON([]byte(original), mustCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	two, err := ReduceFixtureJSON([]byte(updated), mustCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	if one.SnapshotHash != two.SnapshotHash || one.Deliveries[0].PayloadHash != two.Deliveries[0].PayloadHash {
		t.Fatal("provider timestamps alone must not count as delivery or aggregate movement")
	}
}

func TestDeliveryHashIncludesCanonicalSequence(t *testing.T) {
	delivery := Delivery{ProviderEventID: "10", Innings: 1, Sequence: 1, TeamRuns: 1, LegalBall: true}
	one := deliveryHash(delivery)
	delivery.Sequence = 2
	if two := deliveryHash(delivery); two == one {
		t.Fatal("provider array reordering must create a delivery revision")
	}
}

func TestExtrasUseCatalogTeamRunSemantics(t *testing.T) {
	raw := []byte(`{
      "id":100,"league_id":7,"season_id":8,"localteam_id":10,"visitorteam_id":11,
      "type":"T20","status":"1st Innings","starting_at":"2026-07-16T10:00:00Z",
      "balls":{"data":[
        {"id":1,"team_id":10,"ball":"0.1","scoreboard":"S1","score_id":5},
        {"id":2,"team_id":10,"ball":"0.2","scoreboard":"S1","score_id":6},
        {"id":3,"team_id":10,"ball":"0.3","scoreboard":"S1","score_id":7},
        {"id":4,"team_id":10,"ball":"0.3","scoreboard":"S1","score_id":8},
        {"id":5,"team_id":10,"ball":"0.4","scoreboard":"S1","score_id":10}
      ]},
      "runs":{"data":[{"inning":1,"team_id":10,"score":18,"wickets":0,"overs":"0.4"}]},
      "scoreboards":{"data":[{"scoreboard":"S1","team_id":10,"type":"total","total":18,"wickets":0,"overs":"0.4"}]},
      "batting":{"data":[]},"bowling":{"data":[]}
    }`)
	projection, err := ReduceFixtureJSON(raw, mustCatalog(t))
	if err != nil {
		t.Fatalf("ReduceFixtureJSON: %v", err)
	}
	if projection.CurrentScore != 18 || projection.LegalBalls != 4 {
		t.Fatalf("score/balls=%d/%d", projection.CurrentScore, projection.LegalBalls)
	}
	wantTotals := []int{1, 2, 5, 5, 5}
	for i, delivery := range projection.Deliveries {
		if delivery.TeamRuns != wantTotals[i] || delivery.BatterRuns != 0 {
			t.Fatalf("delivery %d runs=%d batter=%d", i, delivery.TeamRuns, delivery.BatterRuns)
		}
	}
	if got := projection.Deliveries[2].Extras; got.NoBalls != 1 || got.Byes != 4 {
		t.Fatalf("no-ball+byes extras=%+v", got)
	}
	if got := projection.Deliveries[3].Extras.Penalties; got != 5 {
		t.Fatalf("penalty=%d", got)
	}
}

func TestNoBallRunsDriveExtrasAndBatterRuns(t *testing.T) {
	raw := []byte(`{
      "id":101,"league_id":7,"season_id":8,"localteam_id":10,"visitorteam_id":11,
      "type":"T20","status":"1st Innings","starting_at":"2026-07-16T10:00:00Z",
      "balls":{"data":[{"id":1,"team_id":10,"ball":"0.1","scoreboard":"S1","score_id":9}]},
      "runs":{"data":[{"inning":1,"team_id":10,"score":2,"wickets":0,"overs":"0.0"}]},
      "scoreboards":{"data":[{"scoreboard":"S1","team_id":10,"type":"total","total":2,"wickets":0,"overs":"0.0"}]},
      "batting":{"data":[]},"bowling":{"data":[]}
    }`)
	projection, err := ReduceFixtureJSON(raw, mustCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	event := projection.Deliveries[0]
	if event.TeamRuns != 2 || event.BatterRuns != 0 || event.Extras.NoBalls != 2 || event.LegalBall {
		t.Fatalf("no-ball delivery=%+v", event)
	}
}

func TestInterruptedIsBreakAndAbandonedInningsIsNotFinal(t *testing.T) {
	if got := normalizeStatus("Int."); got != "innings_break" {
		t.Fatalf("interrupted status=%q", got)
	}
	if inningsComplete("Aban.", 1, 1, 50, 120, 4) {
		t.Fatal("abandoned partial innings must not be settlement-complete")
	}
}

func TestFiftyOverFormatsRequireStructuredSchedule(t *testing.T) {
	t.Run("List A", func(t *testing.T) {
		withoutSchedule := strings.Replace(fixtureJSON(2, 0, "0.1", 1), `"type":"T20"`, `"type":"List A"`, 1)
		if _, err := ReduceFixtureJSON([]byte(withoutSchedule), mustCatalog(t)); !errors.Is(err, ErrUnsupportedFormat) {
			t.Fatalf("unstructured List A error = %v", err)
		}
		withSchedule := strings.Replace(withoutSchedule, `"type":"List A"`, `"type":"List A","total_overs_played":50`, 1)
		projection, err := ReduceFixtureJSON([]byte(withSchedule), mustCatalog(t))
		if err != nil {
			t.Fatalf("structured List A: %v", err)
		}
		if projection.Format != "ODI" || projection.ScheduledBalls != 300 {
			t.Fatalf("structured List A format = %s/%d", projection.Format, projection.ScheduledBalls)
		}
	})
	t.Run("ODI", func(t *testing.T) {
		withoutSchedule := strings.Replace(fixtureJSON(2, 0, "0.1", 1), `"type":"T20"`, `"type":"ODI"`, 1)
		projection, err := ReduceFixtureJSON([]byte(withoutSchedule), mustCatalog(t))
		if err != nil {
			t.Fatalf("standard ODI without structured overs: %v", err)
		}
		if projection.Format != "ODI" || projection.ScheduledBalls != 300 {
			t.Fatalf("ODI format = %s/%d", projection.Format, projection.ScheduledBalls)
		}
	})
}

func TestStructuredOversMustMatchFormat(t *testing.T) {
	reduced := strings.Replace(fixtureJSON(2, 0, "0.1", 1), `"type":"T20"`, `"type":"T20","scheduled_overs":10`, 1)
	if _, err := ReduceFixtureJSON([]byte(reduced), mustCatalog(t)); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("conflicting T20 schedule error = %v", err)
	}
	reducedODI := strings.Replace(fixtureJSON(2, 0, "0.1", 1), `"type":"T20"`, `"type":"ODI","total_overs_played":20`, 1)
	if _, err := ReduceFixtureJSON([]byte(reducedODI), mustCatalog(t)); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("reduced ODI schedule error = %v", err)
	}
	officialT20 := strings.Replace(fixtureJSON(2, 0, "0.1", 1), `"type":"T20"`, `"type":"T20","total_overs_played":20`, 1)
	if _, err := ReduceFixtureJSON([]byte(officialT20), mustCatalog(t)); err != nil {
		t.Fatalf("structured T20 schedule: %v", err)
	}
	nullT20 := strings.Replace(fixtureJSON(2, 0, "0.1", 1), `"type":"T20"`, `"type":"T20","total_overs_played":null`, 1)
	if _, err := ReduceFixtureJSON([]byte(nullT20), mustCatalog(t)); err != nil {
		t.Fatalf("nullable official T20 schedule: %v", err)
	}
	nullODI := strings.Replace(fixtureJSON(2, 0, "0.1", 1), `"type":"T20"`, `"type":"ODI","total_overs_played":null`, 1)
	if _, err := ReduceFixtureJSON([]byte(nullODI), mustCatalog(t)); err != nil {
		t.Fatalf("nullable official ODI schedule: %v", err)
	}
}

func TestFixtureIdentityAndBattingTeamAreRequired(t *testing.T) {
	missingLeague := strings.Replace(fixtureJSON(2, 0, "0.1", 1), `"league_id":7,`, "", 1)
	if _, err := ReduceFixtureJSON([]byte(missingLeague), mustCatalog(t)); !errors.Is(err, ErrIncompleteSnapshot) {
		t.Fatalf("missing identity error = %v", err)
	}
	wrongTeam := strings.ReplaceAll(fixtureJSON(2, 0, "0.1", 1), `"team_id":10`, `"team_id":12`)
	if _, err := ReduceFixtureJSON([]byte(wrongTeam), mustCatalog(t)); !errors.Is(err, ErrIncompleteSnapshot) {
		t.Fatalf("foreign batting team error = %v", err)
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 8)
	for value > 0 {
		digits = append(digits, byte('0'+value%10))
		value /= 10
	}
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return string(digits)
}
