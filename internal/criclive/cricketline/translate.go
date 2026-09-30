package cricketline

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/reconcile"
)

var (
	ErrTeamsUnresolved = errors.New("CricketLine sides could not be matched to the fixture's teams")
	ErrUnreadableScore = errors.New("CricketLine score could not be read")
)

// fixtureTeam is one side of a fixture as the pipeline knows it, with every
// label the provider may print for it.
type fixtureTeam struct {
	id     int64
	labels []string
}

func (t fixtureTeam) named(label string) bool {
	clean := strings.ToLower(strings.TrimSpace(label))
	if clean == "" {
		return false
	}
	for _, candidate := range t.labels {
		if strings.EqualFold(strings.TrimSpace(candidate), clean) {
			return true
		}
	}
	return false
}

// resolveSides decides which fixture team is batting. The feed's short_name
// travels with the score, but its team_id and flag can belong to the other
// side (seen on finished matches), so labels decide: each of the two possible
// assignments counts the labels that agree with it, minus those pointing the
// other way. team_id is not consulted at all — it is the field the feed swaps.
// When the labels are silent or contradict each other the read is rejected:
// holding the feed is recoverable, crediting runs to the wrong team is not.
func resolveSides(batting, bowling teamRef, teams [2]fixtureTeam) (battingID, bowlingID int64, err error) {
	score := func(side teamRef, assigned, other fixtureTeam) int {
		total := 0
		for _, label := range []string{side.ShortName, side.Name} {
			switch {
			case assigned.named(label) && !other.named(label):
				total++
			case other.named(label) && !assigned.named(label):
				total--
			}
		}
		return total
	}
	straight := score(batting, teams[0], teams[1]) + score(bowling, teams[1], teams[0])
	swapped := score(batting, teams[1], teams[0]) + score(bowling, teams[0], teams[1])
	switch {
	case teams[0].id <= 0 || teams[1].id <= 0 || teams[0].id == teams[1].id:
	case straight > swapped:
		return teams[0].id, teams[1].id, nil
	case swapped > straight:
		return teams[1].id, teams[0].id, nil
	}
	return 0, 0, fmt.Errorf("%w: batting %q, bowling %q", ErrTeamsUnresolved,
		firstText(batting.ShortName, batting.Name), firstText(bowling.ShortName, bowling.Name))
}

// sideScore is one side's innings as printed: "148-4", "160/5", "155"
// (all out), "(20.0)", or "Yet To Bat".
type sideScore struct {
	runs    int
	wickets int
	overs   float64
	present bool
	// bare marks a total printed without wickets ("155"), which is how the
	// feed shows a completed innings bowled out. It is only trusted for an
	// innings that is over; the side at the crease must print its wickets.
	bare bool
}

var scorePattern = regexp.MustCompile(`^(\d+)(?:\s*[-/]\s*(\d+))?`)

func parseSideScore(score, overs string) sideScore {
	text := strings.TrimSpace(score)
	match := scorePattern.FindStringSubmatch(text)
	if match == nil {
		return sideScore{}
	}
	result := sideScore{present: true}
	result.runs, _ = strconv.Atoi(match[1])
	if match[2] != "" {
		result.wickets, _ = strconv.Atoi(match[2])
	} else if result.runs > 0 {
		result.wickets, result.bare = 10, true
	}
	cleanOvers := strings.Trim(strings.TrimSpace(overs), "()")
	if value, err := strconv.ParseFloat(cleanOvers, 64); err == nil && value > 0 {
		result.overs = value
	}
	return result
}

func (s sideScore) started() bool {
	return s.present && (s.runs > 0 || s.wickets > 0 || s.overs > 0)
}

// Terminal and paused phrases. status_text carries the chase equation during
// play and the result after it; livestatus is a ball-by-ball ticker ("Ball In
// Air", "Caught Out", "Review Cancelled") that shows the result a moment later
// with a trophy. Abandonment ends a match irreversibly and voids its markets,
// so it is only read from whole, anchored phrases — never from a ticker word
// such as "cancelled" — and the adapter confirms it on a second read.
var (
	statusResultPattern    = regexp.MustCompile(`(?i)\bwon\b|\bbeat\b|\btied\b|\bdrawn\b`)
	tickerResultPattern    = regexp.MustCompile(`(?i)\bwon\b`)
	statusAbandonedPattern = regexp.MustCompile(`(?i)^(the\s+)?(match\s+)?(abandoned|cancelled|canceled|called\s+off|washed\s+out)\b|\bmatch\s+(abandoned|cancelled|canceled|called\s+off)\b|\bno\s+result\b`)
	tickerAbandonedPattern = regexp.MustCompile(`(?i)^(the\s+)?(match\s+)?(abandoned|called\s+off|washed\s+out)\b|\bmatch\s+(abandoned|called\s+off)\b`)
	inningsBreakPattern    = regexp.MustCompile(`(?i)\binnings\s+break\b`)
	stumpsPattern          = regexp.MustCompile(`(?i)\bstumps\b`)
	drinksPattern          = regexp.MustCompile(`(?i)\bdrinks\b`)
	lunchPattern           = regexp.MustCompile(`(?i)\blunch\b`)
	teaPattern             = regexp.MustCompile(`(?i)\btea\b`)
	stoppagePattern        = regexp.MustCompile(`(?i)\brain\b|\bwet\s+outfield\b|\bbad\s+light\b|\bdelay(ed)?\b`)
)

func isTossLine(text string) bool {
	return strings.Contains(strings.ToLower(text), "toss")
}

// matchState maps a live-tab read onto CricLive's state vocabulary, which is
// what the reducer, scheduler and store classify. It returns the state and
// the display phrase.
func matchState(tab matchLiveResponse, started bool) (state, detail string) {
	live := cleanText(tab.LiveStatus)
	status := cleanText(tab.StatusText)
	switch {
	case statusAbandonedPattern.MatchString(status):
		return client.StateAbandon, status
	case tickerAbandonedPattern.MatchString(live):
		return client.StateAbandon, live
	case statusResultPattern.MatchString(status) && !isTossLine(status):
		return client.StateComplete, status
	case tickerResultPattern.MatchString(live) && !isTossLine(live):
		return client.StateComplete, live
	}
	detail = firstText(status, live)
	switch {
	case inningsBreakPattern.MatchString(live):
		return client.StateInnings, firstText(live, status)
	case stumpsPattern.MatchString(live):
		return client.StateStumps, firstText(live, status)
	case drinksPattern.MatchString(live):
		return "Drinks", detail
	case lunchPattern.MatchString(live):
		return "Lunch", detail
	case teaPattern.MatchString(live):
		return "Tea", detail
	case stoppagePattern.MatchString(live):
		if started {
			return client.StateRain, firstText(live, status)
		}
		return "Delay", firstText(live, status)
	}
	if !started {
		if strings.Contains(strings.ToLower(status), "toss") {
			return client.StateToss, status
		}
		return client.StatePreview, detail
	}
	return client.StateInProgress, detail
}

// liveListState maps /api/live's coarse status.
func liveListState(status string) string {
	lower := strings.ToLower(strings.TrimSpace(status))
	switch {
	case lower == "" || lower == "live":
		return client.StateInProgress
	case strings.Contains(lower, "break"):
		return client.StateInnings
	case strings.Contains(lower, "stump"):
		return client.StateStumps
	case statusAbandonedPattern.MatchString(lower):
		return client.StateAbandon
	case strings.Contains(lower, "complete") || strings.Contains(lower, "result"):
		return client.StateComplete
	case strings.Contains(lower, "upcoming") || strings.Contains(lower, "scheduled"):
		return client.StatePreview
	}
	return client.StateInProgress
}

// translation is a match live-tab read in pipeline terms.
type translation struct {
	commentary client.CommentaryData
	overs      client.OversData
	unreadable int
	// firstInnings is the completed first innings, once the second has begun.
	firstInnings sideScore
}

// translateLive turns /api/match/{id}/live into the miniscore and overs the
// reducer consumes. fixture carries the team identity the pipeline already
// holds; the tab is trusted for scores and players only.
// seriesOvers is the innings length the series is known to play when it is
// shorter than the format standard (0 when unknown); see Provider.seriesLength.
func translateLive(tab matchLiveResponse, fixture client.Fixture, teams [2]fixtureTeam, seriesOvers int, strike *strikeTracker, now time.Time) (translation, error) {
	batting := parseSideScore(tab.BattingTeam.Score, tab.BattingTeam.Overs)
	bowling := parseSideScore(tab.BowlingTeam.Score, tab.BowlingTeam.Overs)
	started := batting.started() || bowling.started() || len(tab.RecentOvers) > 0
	state, detail := matchState(tab, started)
	if reconcile.IsNotStartedStatus(state) {
		started = false
	}

	result := translation{}
	header := client.MatchHeader{
		MatchID: fixture.ID, Format: firstText(fixture.Format, tab.Format),
		Status: detail, State: state,
		Team1: client.HeaderTeam{ID: teams[0].id, Name: firstLabel(teams[0]), Short: firstLabel(teams[0])},
		Team2: client.HeaderTeam{ID: teams[1].id, Name: firstLabel(teams[1]), Short: firstLabel(teams[1])},
	}
	mini := client.MiniScore{
		State: state, CustomStatus: detail, Status: detail,
		MatchFormat: firstText(fixture.Format, tab.Format),
	}
	result.commentary = client.CommentaryData{MatchID: fixture.ID, MatchHeader: header}
	result.overs = client.OversData{MatchID: strconv.FormatInt(fixture.ID, 10)}
	if !started {
		result.commentary.MiniScore = mini
		return result, nil
	}

	battingID, bowlingID, err := resolveSides(tab.BattingTeam, tab.BowlingTeam, teams)
	if err != nil {
		return translation{}, err
	}
	// The side at the crease always prints its wickets; a bare total there is
	// a malformed read, and taking it as "all out" would close the innings.
	if batting.bare && !reconcile.IsTerminalProviderStatus(state) && state != client.StateInnings {
		return translation{}, fmt.Errorf("%w: batting score %q", ErrUnreadableScore, tab.BattingTeam.Score)
	}

	current := 1
	if bowling.started() {
		current = 2
	}
	battingLegal := reconcile.OversToBalls(batting.overs)
	mini.InningsID = current
	mini.BatTeamID = battingID
	mini.BatTeamScore = client.FlexInt(batting.runs)
	mini.BatTeamWickets = client.FlexInt(batting.wickets)
	mini.Overs = client.FlexFloat(batting.overs)
	mini.CurrentRunRate = client.FlexFloat(parseRate(tab.CRR))
	mini.RequiredRate = client.FlexFloat(parseRate(tab.RRR))
	mini.Partnership = client.Partnership{Runs: tab.Partnership.Runs, Balls: tab.Partnership.Balls}
	if strings.TrimSpace(tab.LastWicket.Name) != "" {
		mini.LastWicket = cleanText(tab.LastWicket.Text)
	}
	previousFinalOver := 0
	if current == 2 {
		result.firstInnings = bowling
		previousFinalOver = int(math.Ceil(float64(reconcile.OversToBalls(bowling.overs)) / 6))
		mini.InningsScores = append(mini.InningsScores, client.InningsScore{
			InningsID: 1, BatTeam: tab.BowlingTeam.ShortName, BatTeamID: bowlingID,
			Score: client.FlexInt(bowling.runs), Wickets: client.FlexInt(bowling.wickets),
			Overs: client.FlexFloat(bowling.overs),
		})
	}
	mini.InningsScores = append(mini.InningsScores, client.InningsScore{
		InningsID: current, BatTeam: tab.BattingTeam.ShortName, BatTeamID: battingID,
		Score: client.FlexInt(batting.runs), Wickets: client.FlexInt(batting.wickets),
		Overs: client.FlexFloat(batting.overs),
	})
	mini.ScheduledOvers = scheduledOvers(tab, fixture.Format, current, bowling, seriesOvers)
	if current == 2 {
		applyChaseEquation(&mini, tab.StatusText, fixture.Format, batting, bowling, battingLegal)
	}

	// Overs window, oldest first, each filed under its innings.
	numbers := make([]int, len(tab.RecentOvers))
	for i, over := range tab.RecentOvers {
		numbers[i] = overNumber(over.Over)
	}
	inningsOf := assignInnings(numbers, current, battingLegal, previousFinalOver)
	var tracked []trackedBall
	var summary []string
	for i, over := range tab.RecentOvers {
		if numbers[i] <= 0 || inningsOf[i] <= 0 {
			continue
		}
		tokens, unreadable := normalizeOver(over.Balls, over.Runs.Int(), len(over.Balls) > 0)
		result.unreadable += unreadable
		item := client.OverItem{
			InningsID: inningsOf[i], OverNumber: client.FlexFloat(numbers[i]),
			Runs: over.Runs, Balls: tokens, BatTeamName: tab.BattingTeam.ShortName,
		}
		if inningsOf[i] != current {
			item.BatTeamName = tab.BowlingTeam.ShortName
		}
		result.overs.Overs = append(result.overs.Overs, item)
		if inningsOf[i] != current {
			continue
		}
		summary = append(summary, strings.Join(tokens, " "))
		legal := 0
		for index, token := range tokens {
			outcome, parseErr := reconcile.ParseBallToken(token)
			if parseErr != nil {
				continue
			}
			if outcome.LegalBall {
				legal++
			}
			tracked = append(tracked, trackedBall{
				key: ballKey{over: numbers[i], index: index + 1}, token: token,
				overEnd: outcome.LegalBall && legal == 6,
			})
		}
	}
	// No bowler or batter names on the overs: the feed names only the current
	// bowler, and stamping him on the newest over would rename its deliveries
	// the moment the next over began — a "correction" to every ball, every over.
	result.overs.Innings = current
	mini.RecentOvers = strings.Join(summary, " | ")

	// Batters, striker first.
	batters := make([]crease, 0, 2)
	lines := map[string]client.BattingLine{}
	for _, batter := range tab.Batsmen {
		name := strings.TrimSpace(batter.Name)
		if name == "" {
			continue
		}
		batters = append(batters, crease{name: name, runs: batter.Runs.Int(), balls: batter.Balls.Int()})
		lines[name] = client.BattingLine{
			Name: name, Runs: batter.Runs, Balls: batter.Balls,
			Fours: batter.Fours, Sixes: batter.Sixes, StrikeRate: batter.StrikeRate,
		}
	}
	matchKey, _ := DecodeKey(fixture.ID)
	if striker := strike.resolve(matchKey, current, tracked, batters, now); striker != "" {
		mini.Striker = lines[striker]
		mini.NonStriker = lines[otherBatter(batters, striker)]
	}

	if name := strings.TrimSpace(tab.Bowler.Name); name != "" {
		wickets, runs := parseFigures(tab.Bowler.Figures)
		mini.BowlerStriker = client.BowlingLine{
			Name: name, Overs: tab.Bowler.Overs, Runs: client.FlexInt(runs),
			Wickets: client.FlexInt(wickets), Economy: tab.Bowler.Economy,
		}
	}

	if tab.Probability.BattingTeam.ShortName != "" || tab.Probability.BowlingTeam.ShortName != "" {
		result.commentary.WinProbability = &client.WinProbability{
			Team1: client.HeaderTeamOdds{ID: battingID, Short: tab.Probability.BattingTeam.ShortName, Percent: tab.Probability.BattingTeam.Percent},
			Team2: client.HeaderTeamOdds{ID: bowlingID, Short: tab.Probability.BowlingTeam.ShortName, Percent: tab.Probability.BowlingTeam.Percent},
		}
	}
	result.commentary.MiniScore = mini
	return result, nil
}

// scheduledOvers is the innings length when the feed shows it differs from
// the format standard. The projection grid is drawn to it when present; failing
// that, the series' known length applies (a league that plays "T20" at ten
// overs); and a first innings that ended short with wickets in hand was
// shortened whatever either says.
func scheduledOvers(tab matchLiveResponse, format string, current int, firstInnings sideScore, seriesOvers int) int {
	info, err := reconcile.ClassifyFormatInfo(format)
	if err != nil {
		return 0
	}
	overs := tab.ProjectedScore.scheduledOvers()
	if overs == 0 {
		overs = seriesOvers
	}
	if current == 2 {
		if played := shortInningsOvers(firstInnings, info.StandardOvers); played > 0 && (overs == 0 || played < overs) {
			overs = played
		}
	}
	if overs <= 0 || overs >= info.StandardOvers {
		return 0
	}
	return overs
}

// shortInningsOvers is the length of a completed innings that ended before the
// standard overs with wickets in hand — the innings it was scheduled for — or 0.
func shortInningsOvers(innings sideScore, standardOvers int) int {
	if !innings.present || innings.wickets >= 10 {
		return 0
	}
	played := int(math.Ceil(float64(reconcile.OversToBalls(innings.overs)) / 6))
	if played <= 0 || played >= standardOvers {
		return 0
	}
	return played
}

var chasePattern = regexp.MustCompile(`(?i)\bneeds?\s+(\d+)\s+runs?\s+(?:from|in)\s+(\d+)\s+balls?\b|\b(\d+)\s+runs?\s+needed\s+(?:from|in)\s+(\d+)\s+balls?\b`)

// applyChaseEquation reads the chase equation ("Rocks need 13 runs in 7
// balls"), printed in the same payload as the score. It is the one place the
// feed states the target and the balls left, so it exposes a rain rule the
// scores alone cannot: a target other than first-innings runs + 1 is revised,
// and balls left that do not reach the standard length mean fewer overs.
// Either holds trading (the pricing assumes neither).
func applyChaseEquation(mini *client.MiniScore, status, format string, batting, firstInnings sideScore, legalBalls int) {
	match := chasePattern.FindStringSubmatch(cleanText(status))
	if match == nil {
		return
	}
	needText, ballsText := match[1], match[2]
	if needText == "" {
		needText, ballsText = match[3], match[4]
	}
	need, errNeed := strconv.Atoi(needText)
	ballsLeft, errBalls := strconv.Atoi(ballsText)
	if errNeed != nil || errBalls != nil || need <= 0 {
		return
	}
	if target := batting.runs + need; target != firstInnings.runs+1 {
		mini.Target = &target
		mini.RevisedTarget = true
	}
	info, err := reconcile.ClassifyFormatInfo(format)
	if err != nil {
		return
	}
	if scheduled := legalBalls + ballsLeft; scheduled > 0 && scheduled < info.ScheduledBalls {
		overs := int(math.Ceil(float64(scheduled) / 6))
		if mini.ScheduledOvers == 0 || overs < mini.ScheduledOvers {
			mini.ScheduledOvers = overs
		}
	}
}

// parseFigures reads a bowler's "1-23" as one wicket for 23 runs.
func parseFigures(figures string) (wickets, runs int) {
	parts := strings.SplitN(strings.TrimSpace(figures), "-", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	wickets, _ = strconv.Atoi(strings.TrimSpace(parts[0]))
	runs, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
	return wickets, runs
}

func parseRate(value string) float64 {
	rate, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return 0
	}
	return rate
}

// cleanText collapses the feed's doubled spaces and drops its trophy glyph.
func cleanText(value string) string {
	value = strings.ReplaceAll(value, "🏆", "")
	return strings.Join(strings.Fields(value), " ")
}

func firstText(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func firstLabel(team fixtureTeam) string {
	return firstText(team.labels...)
}
