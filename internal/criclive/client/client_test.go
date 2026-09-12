package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testToken = "very-secret-token"

func newTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client, err := New(Config{APIToken: testToken, BaseURL: server.URL + "/api/v1"}, server.Client())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client
}

// CricLive authenticates with a bearer token. It must travel in the header and
// never in the URL, which is logged by proxies and stored in error strings.
func assertAuth(t *testing.T, request *http.Request) {
	t.Helper()
	if got := request.Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Errorf("Authorization = %q, want bearer test token", got)
	}
	if strings.Contains(request.URL.RawQuery, testToken) {
		t.Errorf("token leaked into query string: %s", request.URL.RawQuery)
	}
	if strings.Contains(request.Header.Get("User-Agent"), testToken) {
		t.Error("User-Agent leaked token")
	}
}

func TestLiveScoresDecodesMatchesAndRateMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request)
		if request.URL.Path != "/api/v1/cricket/live" {
			t.Errorf("path = %q", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("X-RateLimit-Limit", "2000")
		writer.Header().Set("X-RateLimit-Remaining", "1499")
		writer.Header().Set("X-RateLimit-Reset", "30")
		fmt.Fprint(writer, `{"success":true,"count":1,"data":[{
			"match_id":169350,"series_id":12870,"series_name":"European T20 Premier League 2026",
			"match_desc":"18th Match","format":"T20","match_type":"league",
			"date":"Sep 09, 13:15 GMT","venue":"The Village, Dublin","state":"In Progress",
			"status_detail":"DBG need 45 runs in 42 balls",
			"first_team":{"id":3,"name":"PAK","full_name":"Pakistan","image_id":776308,"score":"140/7 (20 ov)",
				"innings":[{"innings_id":1,"runs":"140","wickets":7,"overs":19.6}]},
			"second_team":{"id":11,"name":"RSA","full_name":"South Africa","image_id":776287,"score":"96/3 (13 ov)",
				"innings":[{"innings_id":2,"runs":96,"wickets":3,"overs":12.6}]},
			"status":"In Progress"}]}`)
	}))
	defer server.Close()

	response, rateLimit, err := newTestClient(t, server).LiveScores(context.Background())
	if err != nil {
		t.Fatalf("LiveScores() error = %v", err)
	}
	if len(response.Data) != 1 {
		t.Fatalf("matches = %d", len(response.Data))
	}
	match := response.Data[0]
	if match.MatchID != 169350 || match.SeriesID != 12870 || match.State != "In Progress" {
		t.Fatalf("match = %+v", match)
	}
	// runs arrives quoted on one side and numeric on the other.
	if match.FirstTeam.Innings[0].Runs.Int() != 140 || match.SecondTeam.Innings[0].Runs.Int() != 96 {
		t.Fatalf("innings runs = %v / %v", match.FirstTeam.Innings[0].Runs, match.SecondTeam.Innings[0].Runs)
	}
	if len(match.Raw) == 0 {
		t.Fatal("raw match payload was not retained")
	}
	if rateLimit.Limit == nil || *rateLimit.Limit != 2000 ||
		rateLimit.Remaining == nil || *rateLimit.Remaining != 1499 ||
		rateLimit.ResetAfter != 30*time.Second {
		t.Fatalf("rate limit = %+v", rateLimit)
	}
	if len(response.Raw) == 0 {
		t.Fatal("raw response was not retained")
	}
}

func TestCommentaryDecodesMiniScorePlayers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request)
		if request.URL.Path != "/api/v1/cricket/commentary/169350" {
			t.Errorf("path = %q", request.URL.Path)
		}
		fmt.Fprint(writer, `{"success":true,"data":{"match_id":169350,"miniscore":{
			"innings_id":2,"bat_team_id":11,"bat_team_score":96,"bat_team_wickets":3,
			"overs":12.6,"target":141,"crr":7.38,"rrr":"6.42","ovs_rem":"$undefined",
			"partnership":{"runs":13,"balls":"32"},
			"striker":{"id":10385,"name":"Dan Lawrence","runs":5,"balls":10,"strike_rate":"50.00"},
			"non_striker":{"id":12201,"name":"Harry Brook","runs":"52","balls":76},
			"bowler_striker":{"id":1470217,"name":"Razaullah","overs":2.6,"wickets":2},
			"state":"In Progress"},
			"match_header":{"team1":{"id":3,"name":"Pakistan","short":"PAK"},"team2":{"id":11,"name":"South Africa","short":"RSA"}},
			"commentary":[{"timestamp":1788976320944,"text":"driven for four","ball_metric":4,"innings_id":2},
			              {"timestamp":1788976320000,"text":"session summary","ball_metric":null,"innings_id":null}]}}`)
	}))
	defer server.Close()

	response, _, err := newTestClient(t, server).Commentary(context.Background(), 169350)
	if err != nil {
		t.Fatalf("Commentary() error = %v", err)
	}
	mini := response.Data.MiniScore
	if mini.Striker.Name != "Dan Lawrence" || mini.NonStriker.Name != "Harry Brook" || mini.BowlerStriker.Name != "Razaullah" {
		t.Fatalf("players = %+v / %+v / %+v", mini.Striker, mini.NonStriker, mini.BowlerStriker)
	}
	if mini.NonStriker.Runs.Int() != 52 || mini.Partnership.Balls.Int() != 32 {
		t.Fatalf("quoted numbers not absorbed: %+v %+v", mini.NonStriker, mini.Partnership)
	}
	if mini.RequiredRate.Float64() != 6.42 {
		t.Fatalf("rrr = %v", mini.RequiredRate)
	}
	// "$undefined" must decode as zero rather than failing the whole payload.
	if mini.Target == nil || *mini.Target != 141 {
		t.Fatalf("target = %v", mini.Target)
	}
	if len(response.Data.Commentary) != 2 {
		t.Fatalf("commentary items = %d", len(response.Data.Commentary))
	}
	if response.Data.Commentary[0].BallMetric == nil {
		t.Fatal("delivery entry lost its ball metric")
	}
	if response.Data.Commentary[1].BallMetric != nil {
		t.Fatal("prose entry should have no ball metric")
	}
}

func TestOversAndScorecardDecode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request)
		switch request.URL.Path {
		case "/api/v1/cricket/overs/169350":
			fmt.Fprint(writer, `{"success":true,"data":{"matchId":"169350","innings":2,
				"filtersList":[{"id":"2","value":"RSA (2nd Inn)"}],
				"overs":[{"inningsId":2,"overNumber":13,"runs":6,"balls":["1","4","1","W","0","0"],
					"batStrikerNames":["Dan Lawrence"],"bowlNames":["Razaullah"],"bowlOvers":2.6}]}}`)
		case "/api/v1/cricket/scorecard/169350":
			fmt.Fprint(writer, `{"success":true,"data":{"match_id":169350,"innings":[{
				"innings_id":1,"bat_team":"Pakistan","bat_team_short":"PAK","bowl_team":"South Africa",
				"score":"140/7 (20 ov)","run_rate":7,
				"batsmen":[{"name":"Babar Azam","runs":"45","balls":30,"fours":4,"sixes":1,"strike_rate":150,"out_desc":"c Miller b Rabada","wicket_code":"CAUGHT"}],
				"bowlers":[{"name":"Kagiso Rabada","overs":4,"maidens":0,"runs":28,"wickets":"2","economy":7}],
				"yet_to_bat":["Shaheen Afridi"],
				"extras_detail":{"total":16,"byes":5,"leg_byes":5,"wides":1,"no_balls":5,"penalty":0},
				"fall_of_wickets":[{"wkt_num":1,"player":"Babar Azam","runs":62,"over":8.4}],
				"partnerships":[{"bat1_name":"Babar Azam","bat1_runs":45,"bat2_name":"Rizwan","bat2_runs":17,"total_runs":62,"total_balls":40}]}]}}`)
		default:
			t.Errorf("unexpected path %q", request.URL.Path)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server)

	overs, _, err := client.Overs(context.Background(), 169350)
	if err != nil {
		t.Fatalf("Overs() error = %v", err)
	}
	if len(overs.Data.Overs) != 1 || len(overs.Data.Overs[0].Balls) != 6 {
		t.Fatalf("overs = %+v", overs.Data)
	}
	if overs.Data.Overs[0].BatStrikerNames[0] != "Dan Lawrence" || overs.Data.Overs[0].BowlNames[0] != "Razaullah" {
		t.Fatalf("over players = %+v", overs.Data.Overs[0])
	}

	card, _, err := client.Scorecard(context.Background(), 169350)
	if err != nil {
		t.Fatalf("Scorecard() error = %v", err)
	}
	if len(card.Data.Innings) != 1 {
		t.Fatalf("innings = %d", len(card.Data.Innings))
	}
	innings := card.Data.Innings[0]
	if len(innings.Batsmen) != 1 || innings.Batsmen[0].Name != "Babar Azam" || innings.Batsmen[0].Runs.Int() != 45 {
		t.Fatalf("batsmen = %+v", innings.Batsmen)
	}
	if len(innings.Bowlers) != 1 || innings.Bowlers[0].Name != "Kagiso Rabada" || innings.Bowlers[0].Wickets.Int() != 2 {
		t.Fatalf("bowlers = %+v", innings.Bowlers)
	}
	if innings.ExtrasDetail.Total.Int() != 16 || len(innings.YetToBat) != 1 {
		t.Fatalf("extras/yet-to-bat = %+v / %v", innings.ExtrasDetail, innings.YetToBat)
	}
	if len(innings.FallOfWickets) != 1 || innings.FallOfWickets[0].Player != "Babar Azam" {
		t.Fatalf("fall of wickets = %+v", innings.FallOfWickets)
	}
}

func TestRateLimitedResponseIsTyped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Retry-After", "45")
		writer.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(writer, `{"success":false,"error":"Too Many Attempts."}`)
	}))
	defer server.Close()

	_, _, err := newTestClient(t, server).LiveScores(context.Background())
	var limited *RateLimitError
	if !errors.As(err, &limited) {
		t.Fatalf("error = %v, want RateLimitError", err)
	}
	if limited.RetryAfter != 45*time.Second {
		t.Fatalf("retry after = %v", limited.RetryAfter)
	}
	if limited.Message != "Too Many Attempts." {
		t.Fatalf("message = %q", limited.Message)
	}
}

func TestProviderErrorsAreTypedAndRedacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(writer, `{"success":false,"error":"token %s is invalid"}`, testToken)
	}))
	defer server.Close()

	_, _, err := newTestClient(t, server).Commentary(context.Background(), 42)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %v, want HTTPError", err)
	}
	if httpErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", httpErr.StatusCode)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaked the API token: %v", err)
	}
}

func TestInvalidJSONIsARequestError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprint(writer, `{"success":true,"data":`)
	}))
	defer server.Close()

	_, _, err := newTestClient(t, server).Overs(context.Background(), 7)
	var reqErr *RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("error = %v, want RequestError", err)
	}
}

// Following a redirect would replay the Authorization header to another host.
func TestRedirectsAreRefused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "https://example.invalid/leak", http.StatusFound)
	}))
	defer server.Close()

	if _, _, err := newTestClient(t, server).LiveScores(context.Background()); err == nil {
		t.Fatal("redirect was followed")
	}
}

func TestMatchIDMustBePositive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("request should not have been made")
	}))
	defer server.Close()
	client := newTestClient(t, server)

	if _, _, err := client.Commentary(context.Background(), 0); err == nil {
		t.Fatal("zero match id was accepted")
	}
	if _, _, err := client.Scorecard(context.Background(), -1); err == nil {
		t.Fatal("negative match id was accepted")
	}
	if _, _, err := client.Overs(context.Background(), 0); err == nil {
		t.Fatal("zero match id was accepted")
	}
}

func TestNewRejectsUnsafeConfiguration(t *testing.T) {
	if _, err := New(Config{APIToken: "", BaseURL: DefaultBaseURL}, nil); err == nil {
		t.Fatal("missing token was accepted")
	}
	if _, err := New(Config{APIToken: testToken, BaseURL: "http://cricketliveapi.com/api/v1"}, nil); err == nil {
		t.Fatal("plaintext non-loopback base URL was accepted")
	}
	if _, err := New(Config{APIToken: testToken, BaseURL: "https://host/api/v1?api_token=x"}, nil); err == nil {
		t.Fatal("base URL with query parameters was accepted")
	}
	if _, err := New(Config{APIToken: testToken, BaseURL: "http://localhost:8080/api/v1"}, nil); err != nil {
		t.Fatalf("loopback development URL rejected: %v", err)
	}
}

func TestFixtureNormalizationFromLiveAndSchedule(t *testing.T) {
	now := time.Date(2026, 9, 9, 18, 0, 0, 0, time.UTC)
	live := FixtureFromMatchItem(MatchItem{
		MatchID: 169350, SeriesID: 12870, SeriesName: "European T20 Premier League 2026",
		Format: "T20", Date: "Sep 09, 13:15 GMT", Venue: "The Village, Dublin",
		State: "In Progress", StatusDetail: "DBG need 45 runs",
		FirstTeam:  TeamItem{ID: 3, Name: "PAK", FullName: "Pakistan", ImageID: 776308},
		SecondTeam: TeamItem{ID: 11, Name: "RSA", FullName: "South Africa"},
	}, now)
	if live.ID != 169350 || live.SeriesID != 12870 {
		t.Fatalf("live fixture identity = %+v", live)
	}
	if live.LocalTeamName != "Pakistan" || live.VisitorTeamShort != "RSA" {
		t.Fatalf("live fixture teams = %+v", live)
	}
	if !live.StartingAt.Equal(time.Date(2026, 9, 9, 13, 15, 0, 0, time.UTC)) {
		t.Fatalf("live start = %s", live.StartingAt)
	}
	if !live.Live {
		t.Fatal("in-progress fixture was not marked live")
	}

	scheduled := FixtureFromScheduleMatch(ScheduleMatch{
		MatchID: 129596, MatchFormat: "ODI", StartDate: "1788948000000",
		Team1: "England", Team1Short: "ENG", Team1ID: 9,
		Team2: "Pakistan", Team2Short: "PAK", Team2ID: 3,
		Ground: "Edgbaston", City: "Birmingham",
	}, ScheduleSeries{SeriesID: 10565, SeriesName: "Pakistan tour of England 2026"})
	if scheduled.ID != 129596 || scheduled.SeriesID != 10565 {
		t.Fatalf("scheduled fixture identity = %+v", scheduled)
	}
	if scheduled.StartingAt.IsZero() || scheduled.StartingAt.UnixMilli() != 1788948000000 {
		t.Fatalf("scheduled start = %s", scheduled.StartingAt)
	}
	if scheduled.Venue != "Edgbaston, Birmingham" {
		t.Fatalf("venue = %q", scheduled.Venue)
	}
	if scheduled.State != StatePreview {
		t.Fatalf("scheduled state = %q", scheduled.State)
	}
}

// /cricket/live omits the year, so a December fixture read in January (or the
// reverse) must not land twelve months away.
func TestParseLiveDateInfersYearAcrossBoundary(t *testing.T) {
	januaryNow := time.Date(2027, 1, 2, 6, 0, 0, 0, time.UTC)
	parsed := ParseLiveDate("Dec 31, 20:00 GMT", januaryNow)
	if parsed.Year() != 2026 || parsed.Month() != time.December || parsed.Day() != 31 {
		t.Fatalf("December fixture read in January = %s", parsed)
	}

	decemberNow := time.Date(2026, 12, 31, 22, 0, 0, 0, time.UTC)
	parsed = ParseLiveDate("Jan 01, 09:00 GMT", decemberNow)
	if parsed.Year() != 2027 || parsed.Month() != time.January {
		t.Fatalf("January fixture read in December = %s", parsed)
	}

	if got := ParseLiveDate("not a date", januaryNow); !got.IsZero() {
		t.Fatalf("unparseable date = %s, want zero", got)
	}
}

func TestStateClassification(t *testing.T) {
	for _, state := range []string{"In Progress", "innings break", "Stumps", "Rain", "Drinks", "Lunch", "Tea"} {
		if !IsLiveState(state) {
			t.Fatalf("state %q should be live", state)
		}
	}
	// Before the first ball there is nothing on the overs feed: the toss and a
	// pre-match delay must not put a fixture on the per-request poll cycle.
	for _, state := range []string{"Complete", "Abandon", "No Result", "", "Preview", "Scheduled", "Toss", "Delay"} {
		if IsLiveState(state) {
			t.Fatalf("state %q should not be live", state)
		}
	}
}
