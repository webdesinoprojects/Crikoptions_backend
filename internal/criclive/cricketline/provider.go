package cricketline

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/reconcile"
)

// Schedule window, matching the worker's own lookback and horizon.
const (
	scheduleLookbackDays = 2
	scheduleDays         = 17
	// upcomingWindow lists fixtures about to start (or overdue) on discovery.
	// /api/live carries only matches in play, and the worker keeps discovery
	// on its fast cadence only while it sees one; without these the first
	// ball of a match could wait for the idle discovery interval.
	upcomingLead    = 30 * time.Minute
	upcomingOverdue = time.Hour
)

// ist is the zone CricketLineApi's schedule days are cut in.
var ist = time.FixedZone("IST", 5*3600+1800)

// abandonConfirmAfter is how long an abandonment must keep being reported
// before it is passed on. Abandonment voids every market and cannot be undone,
// so one read is not enough; until confirmed the match is held as stopped.
const abandonConfirmAfter = 45 * time.Second

// seriesLengthQuorum is how many completed matches must agree before a
// series' short innings length is assumed for its next match.
const seriesLengthQuorum = 2

// Provider adapts CricketLineApi to the feed pipeline: it satisfies the
// worker's Provider interface with the pipeline's own types, and reads a
// match in one request through MatchSnapshot.
type Provider struct {
	http   *httpClient
	now    func() time.Time
	strike *strikeTracker

	mu        sync.RWMutex
	fixtures  map[string]directoryFixture
	teams     map[int64]directoryTeam
	lengths   map[int64]seriesLength
	abandoned map[string]time.Time
}

// seriesLength is a series' observed short innings length and how many
// completed matches have shown it.
type seriesLength struct {
	overs int
	seen  map[int64]struct{}
}

type directoryTeam struct {
	id    int64
	name  string
	short string
	flag  string
}

type directoryFixture struct {
	id         int64
	teams      [2]directoryTeam
	seriesID   int64
	seriesName string
	format     string
	subtitle   string
	venue      string
	start      time.Time
}

// New builds the adapter from the pipeline config.
func New(cfg client.Config, base *http.Client) (*Provider, error) {
	transport, err := newHTTPClient(cfg.CricketLineBaseURL, cfg.CricketLineAPIKey, base)
	if err != nil {
		return nil, err
	}
	return &Provider{
		http: transport, now: time.Now, strike: newStrikeTracker(),
		fixtures: map[string]directoryFixture{}, teams: map[int64]directoryTeam{},
		lengths: map[int64]seriesLength{}, abandoned: map[string]time.Time{},
	}, nil
}

// SnapshotEndpoint is the quota label of a MatchSnapshot read.
func (p *Provider) SnapshotEndpoint() string { return EndpointMatchLive }

// EndpointLabels are the quota labels of the discovery and schedule reads.
func (p *Provider) EndpointLabels() (live, schedule string) { return EndpointLive, EndpointSchedule }

// Seed loads the fixture directory from fixtures the pipeline already stores,
// so the first discovery pass after a restart can name teams and see upcoming
// starts without spending a request. The worker's schedule sync is
// lease-guarded and may still belong to a process that just exited.
func (p *Provider) Seed(fixtures []client.Fixture) {
	learned := make([]directoryFixture, 0, len(fixtures))
	for _, fixture := range fixtures {
		if !IsFeedID(fixture.ID) || fixture.LocalTeamID <= 0 || fixture.VisitorTeamID <= 0 {
			continue
		}
		team := func(id int64, short, name, flag string) directoryTeam {
			return directoryTeam{id: id, short: firstText(short, name), name: firstText(name, short), flag: flag}
		}
		learned = append(learned, directoryFixture{
			id: fixture.ID, seriesID: fixture.SeriesID, seriesName: fixture.SeriesName,
			format: fixture.Format, subtitle: fixture.MatchDesc, venue: fixture.Venue, start: fixture.StartingAt.UTC(),
			teams: [2]directoryTeam{
				team(fixture.LocalTeamID, fixture.LocalTeamShort, fixture.LocalTeamName, fixture.LocalTeamImageURL),
				team(fixture.VisitorTeamID, fixture.VisitorTeamShort, fixture.VisitorTeamName, fixture.VisitorTeamImageURL),
			},
		})
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, fixture := range learned {
		key, _ := DecodeKey(fixture.id)
		if _, known := p.fixtures[key]; known {
			continue
		}
		p.fixtures[key] = fixture
		for _, team := range fixture.teams {
			if _, known := p.teams[team.id]; !known {
				p.teams[team.id] = team
			}
		}
	}
}

// Schedule reads the fixture window -2 to +14 days (IST calendar days) in one
// request and groups it the way the pipeline's schedule consumer expects.
func (p *Provider) Schedule(ctx context.Context) (client.ScheduleResponse, client.RateLimit, error) {
	start := p.now().In(ist).AddDate(0, 0, -scheduleLookbackDays).Format("2006-01-02")
	query := url.Values{"start": {start}, "days": {strconv.Itoa(scheduleDays)}}
	var payload scheduleRangeResponse
	_, rateLimit, err := p.http.get(ctx, EndpointSchedule, EndpointSchedule, query, &payload)
	if err != nil {
		return client.ScheduleResponse{}, rateLimit, err
	}

	dates := make([]string, 0, len(payload.ByDate))
	for date := range payload.ByDate {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	response := client.ScheduleResponse{Success: true}
	learned := make([]directoryFixture, 0, 128)
	for _, date := range dates {
		day := client.ScheduleDay{Date: date}
		groups := map[int64]int{}
		for _, match := range payload.ByDate[date].Matches {
			fixture, ok := fixtureFromSchedule(match)
			if !ok {
				continue
			}
			learned = append(learned, fixture)
			if strings.EqualFold(strings.TrimSpace(match.Status), "completed") {
				p.observeSeriesLength(fixture.seriesID, fixture.format, fixture.id,
					parseSideScore(match.Team1.Score, match.Team1.Overs), parseSideScore(match.Team2.Score, match.Team2.Overs))
			}
			index, seen := groups[fixture.seriesID]
			if !seen {
				index = len(day.Series)
				groups[fixture.seriesID] = index
				day.Series = append(day.Series, client.ScheduleSeries{
					SeriesID: fixture.seriesID, SeriesName: fixture.seriesName,
				})
			}
			day.Series[index].Matches = append(day.Series[index].Matches, client.ScheduleMatch{
				MatchID: fixture.id, MatchDesc: fixture.subtitle, MatchFormat: fixture.format,
				StartDate: strconv.FormatInt(fixture.start.UnixMilli(), 10),
				Team1:     fixture.teams[0].name, Team1Short: fixture.teams[0].short, Team1ID: fixture.teams[0].id,
				Team2: fixture.teams[1].name, Team2Short: fixture.teams[1].short, Team2ID: fixture.teams[1].id,
				Ground: fixture.venue, Team1ImageURL: fixture.teams[0].flag, Team2ImageURL: fixture.teams[1].flag,
			})
		}
		if len(day.Series) > 0 {
			response.Data = append(response.Data, day)
		}
	}
	p.learn(learned...)
	p.forgetBefore(p.now().UTC().AddDate(0, 0, -(scheduleLookbackDays + 1)))
	return response, rateLimit, nil
}

// forgetBefore drops directory fixtures that started before cutoff, so a
// long-running process does not keep every fixture it has ever seen.
func (p *Provider) forgetBefore(cutoff time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, fixture := range p.fixtures {
		if !fixture.start.IsZero() && fixture.start.Before(cutoff) {
			delete(p.fixtures, key)
		}
	}
}

func fixtureFromSchedule(match scheduleMatch) (directoryFixture, bool) {
	fixture := directoryFixture{
		id: encodeOrZero(match.MatchID), seriesID: encodeOrZero(match.Series.ID),
		seriesName: strings.TrimSpace(match.Series.Name), format: strings.TrimSpace(match.Format),
		subtitle: firstText(match.Subtitle, match.MatchNo), venue: strings.TrimSpace(match.Venue),
		teams: [2]directoryTeam{teamFromSchedule(match.Team1), teamFromSchedule(match.Team2)},
	}
	if millis := int64(match.StartTS.Float64()); millis > 0 {
		fixture.start = time.UnixMilli(millis).UTC()
	}
	valid := fixture.id > 0 && fixture.seriesID > 0 && !fixture.start.IsZero() &&
		fixture.teams[0].id > 0 && fixture.teams[1].id > 0 && fixture.teams[0].id != fixture.teams[1].id
	return fixture, valid
}

func teamFromSchedule(team teamRef) directoryTeam {
	return directoryTeam{
		id: encodeOrZero(team.TeamID), name: firstText(team.Name, team.ShortName),
		short: firstText(team.ShortName, team.Name), flag: strings.TrimSpace(team.Flag),
	}
}

// LiveScores lists the matches in play, plus scheduled fixtures within the
// warm-up window so the worker keeps discovery on its fast cadence around a
// start time.
func (p *Provider) LiveScores(ctx context.Context) (client.LiveResponse, client.RateLimit, error) {
	var payload liveListResponse
	raw, rateLimit, err := p.http.get(ctx, EndpointLive, EndpointLive, nil, &payload)
	if err != nil {
		return client.LiveResponse{}, rateLimit, err
	}
	now := p.now().UTC()
	response := client.LiveResponse{Success: true, Raw: raw}
	listed := map[int64]struct{}{}
	for _, match := range payload.Matches {
		item, ok := p.liveItem(match, now)
		if !ok {
			continue
		}
		listed[item.MatchID] = struct{}{}
		response.Data = append(response.Data, item)
	}
	for _, fixture := range p.upcoming(now) {
		if _, live := listed[fixture.id]; live {
			continue
		}
		response.Data = append(response.Data, matchItem(fixture, client.StatePreview))
	}
	response.Count = len(response.Data)
	return response, rateLimit, nil
}

func (p *Provider) liveItem(match liveMatch, now time.Time) (client.MatchItem, bool) {
	id := encodeOrZero(match.MatchID)
	if id == 0 {
		return client.MatchItem{}, false
	}
	fixture, known := p.fixture(id)
	if !known {
		// Not in the schedule window yet. /api/live pairs name with team_id
		// reliably (it is short_name and score that can belong to the other
		// side), so identity comes from those; the per-match read attributes
		// scores by label against it.
		fixture = directoryFixture{
			id: id, seriesID: encodeOrZero(match.Series.ID), seriesName: strings.TrimSpace(match.Series.Name),
			format: strings.TrimSpace(match.Format), subtitle: firstText(match.Subtitle, match.MatchNo),
			venue: strings.TrimSpace(match.Venue),
			teams: [2]directoryTeam{p.teamFromLive(match.Team1), p.teamFromLive(match.Team2)},
		}
		if fixture.teams[0].id == 0 || fixture.teams[1].id == 0 || fixture.teams[0].id == fixture.teams[1].id {
			return client.MatchItem{}, false
		}
		p.learn(fixture)
	}
	batters := make([]crease, 0, 2)
	striker := ""
	for _, batter := range match.CurrentBatters {
		name := strings.TrimSpace(batter.Name)
		if name == "" {
			continue
		}
		balls, _ := strconv.Atoi(strings.Trim(strings.TrimSpace(batter.Balls), "()"))
		batters = append(batters, crease{name: name, runs: batter.Runs.Int(), balls: balls})
		if batter.OnStrike && striker == "" {
			striker = name
		}
	}
	if len(batters) == 2 && striker != "" {
		p.strike.hint(match.MatchID, batters, striker, now)
	}
	item := matchItem(fixture, liveListState(match.Status))
	item.Status = strings.TrimSpace(match.Status)
	return item, true
}

// teamFromLive takes identity from an /api/live side: name and team_id, with
// the short label from the directory when the team is known.
func (p *Provider) teamFromLive(team teamRef) directoryTeam {
	id := encodeOrZero(team.TeamID)
	p.mu.RLock()
	known, ok := p.teams[id]
	p.mu.RUnlock()
	if ok {
		return known
	}
	name := strings.TrimSpace(team.Name)
	return directoryTeam{id: id, name: name, short: name, flag: strings.TrimSpace(team.Flag)}
}

func matchItem(fixture directoryFixture, state string) client.MatchItem {
	team := func(t directoryTeam) client.TeamItem {
		return client.TeamItem{ID: t.id, Name: t.short, FullName: t.name, ImageURL: t.flag}
	}
	return client.MatchItem{
		MatchID: fixture.id, SeriesID: fixture.seriesID, SeriesName: fixture.seriesName,
		MatchDesc: fixture.subtitle, Format: fixture.format, Venue: fixture.venue,
		State: state, FirstTeam: team(fixture.teams[0]), SecondTeam: team(fixture.teams[1]),
		StartTime: fixture.start,
	}
}

func (p *Provider) upcoming(now time.Time) []directoryFixture {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]directoryFixture, 0, 4)
	for _, fixture := range p.fixtures {
		if fixture.start.IsZero() {
			continue
		}
		if wait := fixture.start.Sub(now); wait <= upcomingLead && wait >= -upcomingOverdue {
			out = append(out, fixture)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start.Before(out[j].start) })
	return out
}

// MatchSnapshot reads one match with a single request.
func (p *Provider) MatchSnapshot(ctx context.Context, fixture client.Fixture) (client.Snapshot, client.RateLimit, error) {
	key, err := DecodeKey(fixture.ID)
	if err != nil {
		return client.Snapshot{}, client.RateLimit{}, fmt.Errorf("%w: %d", client.ErrForeignFixture, fixture.ID)
	}
	now := p.now().UTC()
	fixture = p.enrich(fixture)
	var tab matchLiveResponse
	raw, rateLimit, err := p.http.get(ctx, EndpointMatchLive, matchPath(key), nil, &tab)
	if err != nil {
		// The feed keeps no live record for a match nobody has read yet. Before
		// the scheduled start that is simply "not started"; failing the poll
		// would back the fixture off and delay its first live read.
		if !liveDataMissing(err) || fixture.StartingAt.IsZero() || !fixture.StartingAt.After(now) {
			return client.Snapshot{}, rateLimit, err
		}
		tab, raw = matchLiveResponse{}, nil
	}
	fixture.Format = firstText(fixture.Format, tab.Format)
	translated, err := translateLive(tab, fixture, p.fixtureTeams(fixture), p.seriesOvers(fixture.SeriesID), p.strike, now)
	if err != nil {
		return client.Snapshot{}, rateLimit, err
	}
	mini := &translated.commentary.MiniScore
	if mini.State == client.StateAbandon && !p.confirmAbandon(key, now) {
		// Held as stopped until a later read repeats it.
		held := client.StateRain
		if mini.InningsID == 0 {
			held = "Delay"
		}
		mini.State, translated.commentary.MatchHeader.State = held, held
	} else if mini.State != client.StateAbandon {
		p.clearAbandon(key)
	}
	if translated.firstInnings.present {
		p.observeSeriesLength(fixture.SeriesID, fixture.Format, fixture.ID, translated.firstInnings)
	}
	if reconcile.IsTerminalProviderStatus(mini.State) {
		p.strike.forget(key)
	}
	return client.Snapshot{
		MatchID: fixture.ID, Fixture: fixture,
		Commentary: translated.commentary, Overs: translated.overs, Raw: raw,
	}, rateLimit, nil
}

func liveDataMissing(err error) bool {
	var httpErr *client.HTTPError
	return errors.As(err, &httpErr) &&
		(httpErr.StatusCode == http.StatusNotFound || strings.Contains(strings.ToLower(httpErr.Message), "live data not found"))
}

// confirmAbandon reports whether an abandonment has now been reported for
// abandonConfirmAfter, recording the first sighting.
func (p *Provider) confirmAbandon(key string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	first, seen := p.abandoned[key]
	if !seen {
		p.abandoned[key] = now
		return false
	}
	return now.Sub(first) >= abandonConfirmAfter
}

func (p *Provider) clearAbandon(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.abandoned, key)
}

// observeSeriesLength records how long a completed innings of a series ran.
// Given several innings (a schedule entry lists both sides without saying
// which batted first) the longest is used: it is the one that could have run
// the full length. A full-length innings clears what was learned; a side
// bowled out early says nothing.
func (p *Provider) observeSeriesLength(series int64, format string, match int64, innings ...sideScore) {
	info, err := reconcile.ClassifyFormatInfo(format)
	if err != nil || series <= 0 {
		return
	}
	var longest sideScore
	for _, candidate := range innings {
		if candidate.present && candidate.overs > longest.overs {
			longest = candidate
		}
	}
	if !longest.present {
		return
	}
	overs := shortInningsOvers(longest, info.StandardOvers)
	p.mu.Lock()
	defer p.mu.Unlock()
	current := p.lengths[series]
	switch {
	case overs == 0 && reconcile.OversToBalls(longest.overs) >= info.ScheduledBalls:
		delete(p.lengths, series)
	case overs == 0:
	case current.overs != overs:
		p.lengths[series] = seriesLength{overs: overs, seen: map[int64]struct{}{match: {}}}
	default:
		current.seen[match] = struct{}{}
	}
}

// seriesOvers is the short innings length a series has shown in enough
// completed matches to assume for its next one, or 0.
func (p *Provider) seriesOvers(series int64) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if length, ok := p.lengths[series]; ok && len(length.seen) >= seriesLengthQuorum {
		return length.overs
	}
	return 0
}

// Commentary and Overs satisfy the worker's legacy two-endpoint interface;
// both read the same single match request.
func (p *Provider) Commentary(ctx context.Context, matchID int64) (client.CommentaryResponse, client.RateLimit, error) {
	snapshot, rateLimit, err := p.MatchSnapshot(ctx, p.fixtureByID(matchID))
	if err != nil {
		return client.CommentaryResponse{}, rateLimit, err
	}
	return client.CommentaryResponse{Success: true, Data: snapshot.Commentary, Raw: snapshot.Raw}, rateLimit, nil
}

func (p *Provider) Overs(ctx context.Context, matchID int64) (client.OversResponse, client.RateLimit, error) {
	snapshot, rateLimit, err := p.MatchSnapshot(ctx, p.fixtureByID(matchID))
	if err != nil {
		return client.OversResponse{}, rateLimit, err
	}
	return client.OversResponse{Success: true, Data: snapshot.Overs, Raw: snapshot.Raw}, rateLimit, nil
}

func (p *Provider) fixtureByID(id int64) client.Fixture {
	fixture := client.Fixture{ID: id}
	if known, ok := p.fixture(id); ok {
		fixture.SeriesID, fixture.Format, fixture.StartingAt = known.seriesID, known.format, known.start
		fixture.LocalTeamID, fixture.VisitorTeamID = known.teams[0].id, known.teams[1].id
		fixture.LocalTeamShort, fixture.VisitorTeamShort = known.teams[0].short, known.teams[1].short
		fixture.LocalTeamName, fixture.VisitorTeamName = known.teams[0].name, known.teams[1].name
	}
	return fixture
}

// enrich fills team identity the stored polling target lacks.
func (p *Provider) enrich(fixture client.Fixture) client.Fixture {
	known, ok := p.fixture(fixture.ID)
	if !ok {
		return fixture
	}
	if fixture.LocalTeamID <= 0 || fixture.VisitorTeamID <= 0 {
		fixture.LocalTeamID, fixture.VisitorTeamID = known.teams[0].id, known.teams[1].id
		fixture.LocalTeamShort, fixture.VisitorTeamShort = known.teams[0].short, known.teams[1].short
		fixture.LocalTeamName, fixture.VisitorTeamName = known.teams[0].name, known.teams[1].name
	}
	if fixture.SeriesID <= 0 {
		fixture.SeriesID = known.seriesID
	}
	if fixture.StartingAt.IsZero() {
		fixture.StartingAt = known.start
	}
	fixture.Format = firstText(fixture.Format, known.format)
	return fixture
}

// fixtureTeams gathers every label the feed may print for each side.
func (p *Provider) fixtureTeams(fixture client.Fixture) [2]fixtureTeam {
	p.mu.RLock()
	defer p.mu.RUnlock()
	side := func(id int64, labels ...string) fixtureTeam {
		team := fixtureTeam{id: id}
		if known, ok := p.teams[id]; ok {
			labels = append(labels, known.short, known.name)
		}
		for _, label := range labels {
			if strings.TrimSpace(label) != "" {
				team.labels = append(team.labels, strings.TrimSpace(label))
			}
		}
		return team
	}
	return [2]fixtureTeam{
		side(fixture.LocalTeamID, fixture.LocalTeamShort, fixture.LocalTeamName),
		side(fixture.VisitorTeamID, fixture.VisitorTeamShort, fixture.VisitorTeamName),
	}
}

func (p *Provider) fixture(id int64) (directoryFixture, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	key, err := DecodeKey(id)
	if err != nil {
		return directoryFixture{}, false
	}
	fixture, ok := p.fixtures[key]
	return fixture, ok
}

func (p *Provider) learn(fixtures ...directoryFixture) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, fixture := range fixtures {
		key, err := DecodeKey(fixture.id)
		if err != nil {
			continue
		}
		p.fixtures[key] = fixture
		for _, team := range fixture.teams {
			if team.id > 0 {
				p.teams[team.id] = team
			}
		}
	}
}
