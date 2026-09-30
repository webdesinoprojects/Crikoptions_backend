// Command cricketline_probe checks the CricketLineApi adapter against the real
// API. It never touches a database: it reads recently finished matches through
// the adapter, reduces each one exactly as the feed worker would, and compares
// the innings with the scores /api/schedule/finished reports for the match. It
// then reads a few fixtures starting within a day, which must reduce to an
// upcoming match with no innings, and inventories every ball token the feed
// printed alongside the CricLive notation the adapter rewrote it into.
//
//	CRICKETLINE_API_KEY=... go run ./cmd/cricketline_probe
//	go run ./cmd/cricketline_probe -days 6 -keys VSV,14D3,13PK -out ./payloads
//	go run ./cmd/cricketline_probe -finished ./payloads/schedule_finished.json
//
// The API is metered. Every request goes through one transport that counts it
// and refuses to exceed -max-requests, and -finished replays a saved
// /api/schedule/finished payload instead of fetching it again. The key is sent
// only in the X-API-Key header and is never printed or saved.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/cricketline"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/reconcile"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

// ------------------------------------------------------------------ transport

// recorder is the transport every request goes through: it enforces the
// request budget, spaces requests out, and saves each body under dir.
type recorder struct {
	base  http.RoundTripper
	limit int
	pause time.Duration
	dir   string

	mu    sync.Mutex
	count int
	paths map[string]int
}

// spent reports whether the budget is used up. The adapter reports a
// transport failure as text, so callers ask the recorder rather than match the
// error.
func (r *recorder) spent() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count >= r.limit
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	if r.count >= r.limit {
		r.mu.Unlock()
		return nil, fmt.Errorf("request budget of %d exhausted", r.limit)
	}
	if r.count > 0 && r.pause > 0 {
		time.Sleep(r.pause)
	}
	r.count++
	r.paths[routeLabel(req.URL.Path)]++
	r.mu.Unlock()

	response, err := r.base.RoundTrip(req)
	if err != nil || r.dir == "" {
		return response, err
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(body))
	if readErr != nil {
		return response, nil
	}
	name := strings.Trim(strings.ReplaceAll(req.URL.Path, "/", "_"), "_")
	if query := req.URL.Query(); len(query) > 0 {
		name += "_" + strings.NewReplacer("=", "-", "&", "_").Replace(query.Encode())
	}
	if writeErr := os.WriteFile(filepath.Join(r.dir, name+".json"), body, 0o644); writeErr != nil {
		log.Printf("save payload %s: %v", name, writeErr)
	}
	return response, nil
}

// routeLabel folds a match path into its route, so the tally reads per route.
func routeLabel(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "match" {
		parts[2] = "{id}"
	}
	return "/" + strings.Join(parts, "/")
}

// ------------------------------------------------------------ finished feed

// sideScore is one side of a finished schedule entry.
type sideScore struct {
	Name      string `json:"name"`
	ShortName string `json:"short_name"`
	TeamID    string `json:"team_id"`
	Flag      string `json:"flag"`
	Score     string `json:"score"`
	Overs     string `json:"overs"`
}

type finishedMatch struct {
	MatchID  string           `json:"match_id"`
	MatchNo  string           `json:"match_no"`
	Format   string           `json:"format"`
	Subtitle string           `json:"subtitle"`
	Result   string           `json:"result"`
	StartTS  client.FlexFloat `json:"start_ts"`
	Venue    string           `json:"venue"`
	Series   struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"series"`
	Team1         sideScore `json:"team1"`
	Team2         sideScore `json:"team2"`
	WinningTeamID string    `json:"winning_team_id"`
}

type finishedResponse struct {
	Status string `json:"status"`
	ByDate map[string]struct {
		Matches []finishedMatch `json:"matches"`
	} `json:"by_date"`
}

// liveTab is the part of /api/match/{id}/live the probe reads itself: the
// sides as printed, and the raw over strip.
type liveTab struct {
	LiveStatus  string    `json:"livestatus"`
	StatusText  string    `json:"status_text"`
	Format      string    `json:"format"`
	BattingTeam sideScore `json:"batting_team"`
	BowlingTeam sideScore `json:"bowling_team"`
	RecentOvers []struct {
		Over  string         `json:"over"`
		Balls []string       `json:"balls"`
		Runs  client.FlexInt `json:"runs"`
	} `json:"recent_overs"`
}

func loadFinished(ctx context.Context, httpClient *http.Client, baseURL, key, file string, days int) ([]finishedMatch, error) {
	var body []byte
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		body = data
	} else {
		u, err := url.Parse(strings.TrimRight(baseURL, "/") + "/api/schedule/finished")
		if err != nil {
			return nil, err
		}
		u.RawQuery = url.Values{"days": {strconv.Itoa(days)}}.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("X-API-Key", key)
		response, err := httpClient.Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if body, err = io.ReadAll(response.Body); err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("/api/schedule/finished returned HTTP %d", response.StatusCode)
		}
	}
	var payload finishedResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode finished schedule: %w", err)
	}
	dates := make([]string, 0, len(payload.ByDate))
	for date := range payload.ByDate {
		dates = append(dates, date)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	var out []finishedMatch
	for _, date := range dates {
		out = append(out, payload.ByDate[date].Matches...)
	}
	return out, nil
}

// pickMatches returns the requested keys in the order given, or else up to
// limit matches taken round-robin across formats so every format is covered.
func pickMatches(all []finishedMatch, keys []string, limit int) []finishedMatch {
	if len(keys) > 0 {
		byKey := map[string]finishedMatch{}
		for _, match := range all {
			byKey[match.MatchID] = match
		}
		var out []finishedMatch
		for _, key := range keys {
			if match, ok := byKey[key]; ok {
				out = append(out, match)
			} else {
				fmt.Printf("  key %s is not in the finished schedule; skipped\n", key)
			}
		}
		return out
	}
	groups := map[string][]finishedMatch{}
	var formats []string
	for _, match := range all {
		if _, seen := groups[match.Format]; !seen {
			formats = append(formats, match.Format)
		}
		groups[match.Format] = append(groups[match.Format], match)
	}
	sort.Strings(formats)
	var out []finishedMatch
	for round := 0; len(out) < limit; round++ {
		added := false
		for _, format := range formats {
			if round < len(groups[format]) && len(out) < limit {
				out = append(out, groups[format][round])
				added = true
			}
		}
		if !added {
			break
		}
	}
	return out
}

// fixtureFromFinished builds the polling target the schedule sync would have
// stored for the match, through the same normalizer the worker uses.
func fixtureFromFinished(match finishedMatch) (client.Fixture, error) {
	id, err := cricketline.EncodeKey(match.MatchID)
	if err != nil {
		return client.Fixture{}, err
	}
	series, _ := cricketline.EncodeKey(match.Series.ID)
	team1, err1 := cricketline.EncodeKey(match.Team1.TeamID)
	team2, err2 := cricketline.EncodeKey(match.Team2.TeamID)
	if err := errors.Join(err1, err2); err != nil {
		return client.Fixture{}, err
	}
	return client.FixtureFromScheduleMatch(client.ScheduleMatch{
		MatchID: id, MatchDesc: match.Subtitle, MatchFormat: match.Format,
		StartDate: strconv.FormatInt(int64(match.StartTS.Float64()), 10),
		Team1:     match.Team1.Name, Team1Short: match.Team1.ShortName, Team1ID: team1,
		Team2: match.Team2.Name, Team2Short: match.Team2.ShortName, Team2ID: team2,
		Ground: match.Venue, Team1ImageURL: match.Team1.Flag, Team2ImageURL: match.Team2.Flag,
	}, client.ScheduleSeries{SeriesID: series, SeriesName: match.Series.Name}), nil
}

// ------------------------------------------------------------- ground truth

type inningsLine struct {
	team    string
	teamID  int64
	runs    int
	wickets int
	balls   int
}

func (l inningsLine) String() string {
	if l.team == "" {
		return "-"
	}
	return fmt.Sprintf("%s %d/%d", l.team, l.runs, l.wickets)
}

// expectation is what the finished schedule says the reduction must show.
type expectation struct {
	status  string
	innings []inningsLine
	// reduced is checked only when two innings were played.
	checkReduced bool
	reduced      bool
	overs        int
	notes        []string
}

var (
	finishedScore = regexp.MustCompile(`^\s*(\d+)\s*(?:[/-]\s*(\d+))?`)
	marginPattern = regexp.MustCompile(`(?i)won by (\d+) (run|wicket)`)
)

func parseSide(side sideScore) (inningsLine, bool) {
	match := finishedScore.FindStringSubmatch(side.Score)
	if match == nil {
		return inningsLine{}, false
	}
	line := inningsLine{team: side.ShortName}
	line.runs, _ = strconv.Atoi(match[1])
	if match[2] != "" {
		line.wickets, _ = strconv.Atoi(match[2])
	} else {
		line.wickets = 10
	}
	if overs, err := strconv.ParseFloat(strings.Trim(strings.TrimSpace(side.Overs), "()"), 64); err == nil {
		line.balls = reconcile.OversToBalls(overs)
	}
	line.teamID, _ = cricketline.EncodeKey(side.TeamID)
	return line, true
}

// expect derives the batting order from the result: a margin in runs means the
// winner batted first, in wickets that it chased. A result with no margin
// (DLS) falls back to a winner bowled out having batted first, then to the
// side that faced more balls.
func expect(match finishedMatch) expectation {
	result := strings.ToLower(match.Result)
	exp := expectation{status: matches.StatusCompleted}
	if strings.Contains(result, "abandon") || strings.Contains(result, "no result") {
		exp.status = matches.StatusAbandoned
	}
	one, has1 := parseSide(match.Team1)
	two, has2 := parseSide(match.Team2)
	switch {
	case !has1 && !has2:
		return exp
	case has1 != has2:
		if has1 {
			exp.innings = []inningsLine{one}
		} else {
			exp.innings = []inningsLine{two}
		}
		return exp
	}

	winner := 0
	label := strings.TrimSpace(strings.SplitN(match.Result, " Won", 2)[0])
	for i, side := range []sideScore{match.Team1, match.Team2} {
		if label != "" && (strings.EqualFold(label, side.ShortName) || strings.EqualFold(label, side.Name)) {
			winner = i + 1
		}
	}
	idWinner := 0
	for i, side := range []sideScore{match.Team1, match.Team2} {
		if side.TeamID == match.WinningTeamID {
			idWinner = i + 1
		}
	}
	switch {
	case winner == 0:
		winner = idWinner
		exp.notes = append(exp.notes, "winner from winning_team_id")
	case idWinner != 0 && idWinner != winner:
		exp.notes = append(exp.notes, "finished entry: winning_team_id disagrees with result label")
	}

	firstIsOne := one.balls >= two.balls
	margin := marginPattern.FindStringSubmatch(match.Result)
	switch {
	case winner != 0 && margin != nil:
		winnerFirst := strings.EqualFold(margin[2], "run")
		firstIsOne = (winner == 1) == winnerFirst
	case winner == 1 && one.wickets == 10:
		firstIsOne = true
	case winner == 2 && two.wickets == 10:
		firstIsOne = false
	default:
		exp.notes = append(exp.notes, "batting order inferred from balls faced")
	}
	if firstIsOne {
		exp.innings = []inningsLine{one, two}
	} else {
		exp.innings = []inningsLine{two, one}
	}

	info, err := reconcile.ClassifyFormatInfo(match.Format)
	if err == nil {
		first := exp.innings[0]
		played := int(math.Ceil(float64(first.balls) / 6))
		exp.checkReduced = true
		exp.reduced = first.wickets < 10 && played > 0 && played < info.StandardOvers
		if exp.reduced {
			exp.overs = played
		}
	}
	return exp
}

// ------------------------------------------------------------------ tokens

type tokenStats struct {
	mapped     map[string]map[string]int // raw -> CricLive token -> count
	unreadable map[string]int
	mismatches []string
	dropped    []string
	overs      int
}

func newTokenStats() *tokenStats {
	return &tokenStats{mapped: map[string]map[string]int{}, unreadable: map[string]int{}}
}

var overDigits = regexp.MustCompile(`\d+`)

// record pairs each raw over with the over the adapter emitted for it: they
// arrive in the same order, and an over the adapter could not place is absent.
func (s *tokenStats) record(key string, tab liveTab, emitted []client.OverItem) {
	next := 0
	for _, over := range tab.RecentOvers {
		if len(over.Balls) == 0 {
			continue
		}
		s.overs++
		number, _ := strconv.Atoi(overDigits.FindString(over.Over))
		if next >= len(emitted) || int(emitted[next].OverNumber.Float64()) != number ||
			len(emitted[next].Balls) != len(over.Balls) {
			s.dropped = append(s.dropped, fmt.Sprintf("%s %q %v runs=%d", key, over.Over, over.Balls, over.Runs.Int()))
			continue
		}
		normalized := emitted[next].Balls
		next++
		total := 0
		for i, raw := range over.Balls {
			if s.mapped[raw] == nil {
				s.mapped[raw] = map[string]int{}
			}
			s.mapped[raw][normalized[i]]++
			outcome, err := reconcile.ParseBallToken(normalized[i])
			if err != nil {
				s.unreadable[raw]++
				continue
			}
			total += outcome.TotalRuns
		}
		if total != over.Runs.Int() {
			s.mismatches = append(s.mismatches, fmt.Sprintf("%s %q raw=%v -> %v = %d, provider runs=%d",
				key, over.Over, over.Balls, normalized, total, over.Runs.Int()))
		}
	}
}

// ------------------------------------------------------------------- probe

type swapStats struct {
	sides, swapped, unresolved int
	examples                   []string
}

// checkSwap compares a side's team_id with the fixture team its label names.
func (s *swapStats) checkSwap(key string, side sideScore, fixture client.Fixture) {
	if strings.TrimSpace(side.ShortName) == "" || side.TeamID == "" {
		return
	}
	byLabel := int64(0)
	for _, team := range []struct {
		id     int64
		labels []string
	}{
		{fixture.LocalTeamID, []string{fixture.LocalTeamShort, fixture.LocalTeamName}},
		{fixture.VisitorTeamID, []string{fixture.VisitorTeamShort, fixture.VisitorTeamName}},
	} {
		for _, label := range team.labels {
			if strings.EqualFold(strings.TrimSpace(label), strings.TrimSpace(side.ShortName)) {
				byLabel = team.id
			}
		}
	}
	s.sides++
	encoded, _ := cricketline.EncodeKey(side.TeamID)
	switch {
	case byLabel == 0:
		s.unresolved++
	case encoded != byLabel:
		s.swapped++
		if len(s.examples) < 6 {
			s.examples = append(s.examples, fmt.Sprintf("%s %s team_id=%s", key, side.ShortName, side.TeamID))
		}
	}
}

type outcome struct {
	key, format, expected1, got1, expected2, got2, status, reduced, verdict string
	problems                                                                []string
}

func teamShort(fixture client.Fixture, id int64) string {
	switch id {
	case fixture.LocalTeamID:
		return fixture.LocalTeamShort
	case fixture.VisitorTeamID:
		return fixture.VisitorTeamShort
	case 0:
		return "?"
	}
	return fmt.Sprintf("#%d", id)
}

func compareFinished(match finishedMatch, fixture client.Fixture, reduced reconcile.Projection) outcome {
	exp := expect(match)
	row := outcome{key: match.MatchID, format: match.Format, problems: append([]string(nil), exp.notes...)}
	got := map[int]reconcile.Innings{}
	for _, in := range reduced.Innings {
		got[in.Number] = in
	}
	cell := func(number int) string {
		in, ok := got[number]
		if !ok {
			return "-"
		}
		return fmt.Sprintf("%s %d/%d", teamShort(fixture, in.BattingTeamID), in.Runs, in.Wickets)
	}
	failed := false
	fail := func(format string, args ...any) {
		failed = true
		row.problems = append(row.problems, fmt.Sprintf(format, args...))
	}
	for number := 1; number <= 2; number++ {
		want := inningsLine{}
		if number <= len(exp.innings) {
			want = exp.innings[number-1]
		}
		have, ok := got[number]
		switch {
		case want.team == "" && ok:
			fail("innings %d reported but none was played", number)
		case want.team != "" && !ok:
			fail("innings %d missing", number)
		case want.team != "" && (have.BattingTeamID != want.teamID || have.Runs != want.runs || have.Wickets != want.wickets):
			fail("innings %d want %s got %s", number, want, cell(number))
		}
	}
	if number := len(reduced.Innings); number > 2 {
		fail("%d innings reported", number)
	}
	row.expected1, row.got1 = lineOf(exp.innings, 0), cell(1)
	row.expected2, row.got2 = lineOf(exp.innings, 1), cell(2)
	row.status = reduced.Status
	if reduced.Status != exp.status {
		fail("status want %s got %s (state %q, phrase %q)", exp.status, reduced.Status, reduced.ProviderState, reduced.ProviderStatus)
	}
	row.reduced = fmt.Sprintf("%t/%d", reduced.ReducedOvers, reduced.ScheduledOvers)
	if exp.checkReduced {
		wantOvers := exp.overs
		if !exp.reduced {
			wantOvers = reduced.ScheduledOvers
		}
		if reduced.ReducedOvers != exp.reduced || reduced.ScheduledOvers != wantOvers {
			fail("reduced want %t/%d got %t/%d", exp.reduced, exp.overs, reduced.ReducedOvers, reduced.ScheduledOvers)
		}
	}
	row.verdict = "PASS"
	if failed {
		row.verdict = "FAIL"
	}
	return row
}

func lineOf(lines []inningsLine, i int) string {
	if i >= len(lines) {
		return "-"
	}
	return lines[i].String()
}

func main() {
	days := flag.Int("days", 2, "finished-schedule lookback in days")
	keysRaw := flag.String("keys", "", "comma-separated CricketLine match keys to probe (default: a mix of formats)")
	limit := flag.Int("limit", 40, "finished matches to probe when -keys is empty")
	upcomingN := flag.Int("upcoming", 3, "fixtures starting within 24h to probe")
	finishedFile := flag.String("finished", "", "read /api/schedule/finished from this saved payload instead of the API")
	outDir := flag.String("out", "", "directory to save every raw response in")
	maxRequests := flag.Int("max-requests", 60, "hard cap on API requests for this run")
	pause := flag.Duration("pause", 250*time.Millisecond, "pause between requests")
	flag.Parse()

	key := strings.TrimSpace(os.Getenv("CRICKETLINE_API_KEY"))
	if key == "" {
		log.Fatal("CRICKETLINE_API_KEY is not set")
	}
	baseURL := strings.TrimSpace(os.Getenv("CRICKETLINE_BASE_URL"))
	if baseURL == "" {
		baseURL = client.DefaultCricketLineBaseURL
	}
	if *outDir != "" {
		if err := os.MkdirAll(*outDir, 0o755); err != nil {
			log.Fatalf("out dir: %v", err)
		}
	}
	var keys []string
	for _, part := range strings.Split(*keysRaw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			keys = append(keys, part)
		}
	}

	transport := &recorder{base: http.DefaultTransport, limit: *maxRequests, pause: *pause, dir: *outDir, paths: map[string]int{}}
	httpClient := &http.Client{Timeout: 20 * time.Second, Transport: transport}
	provider, err := cricketline.New(client.Config{CricketLineBaseURL: baseURL, CricketLineAPIKey: key}, httpClient)
	if err != nil {
		log.Fatalf("adapter: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// In service the schedule sync fills the adapter's fixture directory before
	// any match is polled; do the same so labels resolve as they would there.
	schedule, _, err := provider.Schedule(ctx)
	if err != nil {
		log.Fatalf("schedule: %v", err)
	}
	now := time.Now().UTC()
	type scheduled struct {
		fixture client.Fixture
		format  string
	}
	var soon []scheduled
	fixtures, unsupported := 0, map[string]int{}
	for _, day := range schedule.Data {
		for _, series := range day.Series {
			for _, match := range series.Matches {
				fixtures++
				fixture := client.FixtureFromScheduleMatch(match, series)
				if _, _, err := reconcile.ClassifyFormat(fixture.Format); err != nil {
					unsupported[fixture.Format]++
					continue
				}
				if wait := fixture.StartingAt.Sub(now); wait > 0 && wait <= 24*time.Hour {
					soon = append(soon, scheduled{fixture: fixture, format: fixture.Format})
				}
			}
		}
	}
	sort.Slice(soon, func(i, j int) bool { return soon[i].fixture.StartingAt.Before(soon[j].fixture.StartingAt) })
	fmt.Printf("SCHEDULE %d fixtures in %d days; unsupported formats %v; %d supported starting within 24h\n\n",
		fixtures, len(schedule.Data), unsupported, len(soon))

	all, err := loadFinished(ctx, httpClient, baseURL, key, *finishedFile, *days)
	if err != nil {
		log.Fatalf("finished schedule: %v", err)
	}
	picked := pickMatches(all, keys, *limit)
	fmt.Printf("FINISHED %d matches listed, probing %d\n\n", len(all), len(picked))

	tokens := newTokenStats()
	swaps := &swapStats{}
	var rows []outcome
	for _, match := range picked {
		fixture, err := fixtureFromFinished(match)
		if err != nil {
			rows = append(rows, outcome{key: match.MatchID, format: match.Format, verdict: "FAIL", problems: []string{err.Error()}})
			continue
		}
		snapshot, _, err := provider.MatchSnapshot(ctx, fixture)
		var limited *client.RateLimitError
		if err != nil && (transport.spent() || errors.As(err, &limited)) {
			fmt.Printf("stopping: %v\n", err)
			break
		}
		if err != nil {
			// The feed keeps a match's live tab only for matches it followed
			// live; a 404 says nothing about the adapter, so it is not a FAIL.
			verdict := "FAIL"
			var httpErr *client.HTTPError
			if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
				verdict = "NODATA"
			}
			rows = append(rows, outcome{key: match.MatchID, format: match.Format, verdict: verdict, problems: []string{"snapshot: " + err.Error()}})
			continue
		}
		var tab liveTab
		if err := json.Unmarshal(snapshot.Raw, &tab); err == nil {
			tokens.record(match.MatchID, tab, snapshot.Overs.Overs)
			swaps.checkSwap(match.MatchID, tab.BattingTeam, fixture)
			swaps.checkSwap(match.MatchID, tab.BowlingTeam, fixture)
		}
		reduced, err := reconcile.ReduceSnapshot(snapshot)
		if err != nil {
			rows = append(rows, outcome{key: match.MatchID, format: match.Format, verdict: "FAIL",
				problems: []string{fmt.Sprintf("reduce: %v (livestatus %q)", err, tab.LiveStatus)}})
			continue
		}
		rows = append(rows, compareFinished(match, fixture, reduced))
	}

	fmt.Printf("%-5s %-8s %-15s %-15s %-15s %-15s %-10s %-8s %s\n",
		"KEY", "FORMAT", "WANT INN1", "GOT INN1", "WANT INN2", "GOT INN2", "STATUS", "REDUCED", "VERDICT")
	verdicts := map[string]int{}
	for _, row := range rows {
		verdicts[row.verdict]++
		fmt.Printf("%-5s %-8s %-15s %-15s %-15s %-15s %-10s %-8s %s\n",
			row.key, row.format, row.expected1, row.got1, row.expected2, row.got2, row.status, row.reduced, row.verdict)
		for _, problem := range row.problems {
			fmt.Printf("      - %s\n", problem)
		}
	}
	fmt.Printf("\nFINISHED %d probed: %d PASS, %d FAIL, %d NODATA (no live tab kept)\n",
		len(rows), verdicts["PASS"], verdicts["FAIL"], verdicts["NODATA"])

	fmt.Println("\nUPCOMING")
	for i, item := range soon {
		if i >= *upcomingN {
			break
		}
		key, _ := cricketline.DecodeKey(item.fixture.ID)
		label := fmt.Sprintf("%-5s %-7s %s v %s @ %s", key, item.format, item.fixture.LocalTeamShort,
			item.fixture.VisitorTeamShort, item.fixture.StartingAt.Format(time.RFC3339))
		snapshot, _, err := provider.MatchSnapshot(ctx, item.fixture)
		if err != nil {
			fmt.Printf("  %s FAIL snapshot: %v\n", label, err)
			if transport.spent() {
				break
			}
			continue
		}
		reduced, err := reconcile.ReduceSnapshot(snapshot)
		switch {
		case err != nil:
			fmt.Printf("  %s FAIL reduce: %v\n", label, err)
		case reduced.Status != matches.StatusUpcoming || len(reduced.Innings) > 0:
			fmt.Printf("  %s FAIL status=%s innings=%d state=%q\n", label, reduced.Status, len(reduced.Innings), reduced.ProviderState)
		default:
			fmt.Printf("  %s PASS status=%s state=%q\n", label, reduced.Status, reduced.ProviderState)
		}
	}

	fmt.Printf("\nTOKENS (%d overs read)\n", tokens.overs)
	raws := make([]string, 0, len(tokens.mapped))
	for raw := range tokens.mapped {
		raws = append(raws, raw)
	}
	sort.Strings(raws)
	for _, raw := range raws {
		var parts []string
		for normalized, count := range tokens.mapped[raw] {
			parts = append(parts, fmt.Sprintf("%s x%d", normalized, count))
		}
		sort.Strings(parts)
		fmt.Printf("  %-6q -> %s\n", raw, strings.Join(parts, ", "))
	}
	fmt.Printf("unreadable after normalizing: %v\n", tokens.unreadable)
	fmt.Printf("overs whose normalized total differs from the provider's (%d):\n", len(tokens.mismatches))
	for _, line := range tokens.mismatches {
		fmt.Println("  " + line)
	}
	fmt.Printf("overs the adapter dropped (%d):\n", len(tokens.dropped))
	for _, line := range tokens.dropped {
		fmt.Println("  " + line)
	}

	fmt.Printf("\nTEAM_ID SWAP: %d of %d live-tab sides carry the other side's team_id (%d unresolvable by label)\n",
		swaps.swapped, swaps.sides, swaps.unresolved)
	for _, example := range swaps.examples {
		fmt.Println("  " + example)
	}

	fmt.Printf("\nREQUESTS %d of %d allowed: %v\n", transport.count, transport.limit, transport.paths)
}
