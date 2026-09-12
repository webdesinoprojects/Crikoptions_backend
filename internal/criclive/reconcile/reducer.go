package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

var (
	ErrIncompleteSnapshot = errors.New("incomplete CricLive snapshot")
	ErrUnknownBallToken   = errors.New("unknown CricLive ball token")
	ErrUnsupportedFormat  = errors.New("unsupported cricket format")
	// ErrSuperOver and ErrRevisedTarget refine ErrUnsupportedFormat so the
	// reason a live fixture is held reaches the UI as something explainable
	// rather than a bare "unsupported".
	ErrSuperOver     = errors.New("super over")
	ErrRevisedTarget = errors.New("revised/DLS target")
)

// UnsupportedBlocker maps a rejected snapshot to the trading blocker that best
// describes why the fixture is being held.
func UnsupportedBlocker(err error) string {
	switch {
	case errors.Is(err, ErrSuperOver):
		return "super_over"
	case errors.Is(err, ErrRevisedTarget):
		return "revised_target"
	default:
		return "unsupported"
	}
}

// Delivery is the provider-neutral, fully classified delivery emitted by the
// reducer. Provider ball notation is display data only; ProviderEventID is the
// stable identity used for deduplication and corrections.
type Delivery struct {
	ProviderEventID   string
	ProviderScoreID   int64
	ProviderBall      string
	Innings           int
	Sequence          int64
	TeamID            int64
	BatterID          int64
	BowlerID          int64
	BatterName        string
	BowlerName        string
	TeamRuns          int
	BatterRuns        int
	LegalBall         bool
	Extras            matches.DeliveryExtras
	Dismissal         *matches.Dismissal
	ProviderUpdatedAt *time.Time
	PayloadHash       string
}

type Innings struct {
	Number         int
	BattingTeamID  int64
	Runs           int
	Wickets        int
	LegalBalls     int
	ScheduledBalls int
	Target         int
	Complete       bool
	SnapshotHash   string
}

type Projection struct {
	FixtureID       int64
	LeagueID        int64
	SeasonID        int64
	LocalTeamID     int64
	VisitorTeamID   int64
	LocalTeamName   string
	VisitorTeamName string
	StartTime       time.Time
	Format          string
	ScheduledBalls  int
	ScheduledOvers  int
	ReducedOvers    bool
	// ProviderStatus is the provider's display phrase ("RSA need 45 runs...");
	// ProviderState is its canonical state ("In Progress"), which is what the
	// scheduler and endpoint choice classify.
	ProviderStatus string
	ProviderState  string
	Status         string
	CurrentInnings int
	BattingTeamID  int64
	CurrentScore   int
	Wickets        int
	LegalBalls     int
	Target         int
	Innings        []Innings
	Deliveries     []Delivery
	// DeliveryWindow marks the oldest delivery CricLive still exposes.
	// /cricket/overs only returns the most recent overs, so deliveries older
	// than this were not observed in this poll and must not be mistaken for
	// deletions. Zero means "the whole innings was observed".
	DeliveryWindowInnings  int
	DeliveryWindowSequence int64
	LiveContext            *matches.LiveMatchContext
	MatchPulse             *matches.MatchPulse
	ThisOver               []matches.OverBall
	ProviderUpdatedAt      *time.Time
	SnapshotHash           string
}

// FormatInfo describes the playing conditions a fixture is actually being
// played under. ScheduledOvers is what the provider scheduled per innings,
// which drops below StandardOvers when a match is shortened (rain delay,
// reduced-overs restart). Such a match stays a real ODI/T20 and keeps a
// deterministic ball count, so it is admitted read-only rather than dropped —
// callers gate trading on Reduced.
type FormatInfo struct {
	Format         string
	ScheduledBalls int
	ScheduledOvers int
	StandardOvers  int
	Reduced        bool
}

// ReduceSnapshot normalizes one polled CricLive match into the projection the
// store applies. It accepts either source a poll may have used: the commentary
// miniscore (the one-shot state read outside play) or, during play, the
// ball-by-ball overs feed plus the innings summary discovery placed on the
// fixture. The overs case is folded into the miniscore shape up front so the
// reduction has a single path.
func ReduceSnapshot(snapshot client.Snapshot) (Projection, error) {
	fixture := snapshot.Fixture
	fixtureID := snapshot.MatchID
	if fixtureID <= 0 {
		fixtureID = fixture.ID
	}
	if fixtureID <= 0 {
		return Projection{}, fmt.Errorf("match id is missing")
	}
	mini := snapshot.Commentary.MiniScore
	header := snapshot.Commentary.MatchHeader
	if mini.State == "" && len(mini.InningsScores) == 0 &&
		(len(snapshot.Overs.Overs) > 0 || len(snapshot.Fixture.LiveInnings) > 0) {
		mini = miniScoreFromOvers(snapshot)
	}

	localID, visitorID := fixture.LocalTeamID, fixture.VisitorTeamID
	localName, visitorName := fixture.LocalTeamName, fixture.VisitorTeamName
	// The commentary header is the fallback identity source: a fixture
	// discovered mid-poll may not have been seen by discovery yet.
	if localID <= 0 || visitorID <= 0 {
		localID, visitorID = header.Team1.ID, header.Team2.ID
		localName, visitorName = header.Team1.Name, header.Team2.Name
	}
	if localID <= 0 || visitorID <= 0 || localID == visitorID {
		return Projection{}, fmt.Errorf("%w: fixture identity is incomplete", ErrIncompleteSnapshot)
	}

	rawFormat := firstNonEmpty(fixture.Format, mini.MatchFormat, header.Format)
	formatInfo, err := ClassifyFormatInfo(rawFormat)
	if err != nil {
		return Projection{}, err
	}
	scheduledBalls := formatInfo.ScheduledBalls

	state := firstNonEmpty(mini.State, fixture.State, header.State)
	providerStatus := firstNonEmpty(mini.CustomStatus, fixture.StatusDetail, mini.Status, state)
	localStatus := NormalizeProviderStatus(state)

	currentInnings := mini.InningsID
	if currentInnings <= 0 {
		currentInnings = len(mini.InningsScores)
	}
	// A super over is scored as a further innings pair beyond the scheduled
	// two; it is not deterministically priceable, so hold rather than guess.
	if currentInnings > 2 {
		return Projection{}, fmt.Errorf("%w: %w", ErrUnsupportedFormat, ErrSuperOver)
	}

	innings, err := inningsFromSnapshot(snapshot, mini, localID, visitorID, scheduledBalls, currentInnings, localStatus)
	if err != nil {
		return Projection{}, err
	}

	target := 0
	if mini.Target != nil {
		target = *mini.Target
	}
	if target <= 0 && (currentInnings == 2 || localStatus == matches.StatusInningsBreak) {
		for _, in := range innings {
			if in.Number == 1 && in.Complete {
				target = in.Runs + 1
			}
		}
	}
	if currentInnings == 2 && target > 0 {
		for i := range innings {
			if innings[i].Number != 2 {
				continue
			}
			innings[i].Target = target
			innings[i].Complete = innings[i].Complete || innings[i].Runs >= target
		}
	}

	battingTeamID := mini.BatTeamID
	if battingTeamID != 0 && battingTeamID != localID && battingTeamID != visitorID {
		return Projection{}, fmt.Errorf("%w: batting team %d is not a fixture team", ErrIncompleteSnapshot, battingTeamID)
	}

	current := Innings{Number: currentInnings, ScheduledBalls: scheduledBalls}
	for _, in := range innings {
		if in.Number == currentInnings {
			current = in
			break
		}
	}
	if battingTeamID == 0 {
		battingTeamID = current.BattingTeamID
	}
	if current.BattingTeamID == 0 {
		current.BattingTeamID = battingTeamID
	}
	// A live fixture with nobody batting is an unusable snapshot. Terminal
	// fixtures legitimately report no batting side, so they are let through.
	if currentInnings > 0 && battingTeamID == 0 && !IsTerminalProviderStatus(state) {
		return Projection{}, fmt.Errorf("%w: current innings batting team is missing", ErrIncompleteSnapshot)
	}

	deliveries, windowInnings, windowSequence := deliveriesFromOvers(snapshot.Overs, battingTeamID, innings)
	providerUpdated := latestCommentaryTime(snapshot.Commentary.Commentary)

	projection := Projection{
		// CricLive has no season concept. The store persists the series id as
		// providerSeasonId and the identity check compares against it, so the
		// projection must carry the same value — leaving it zero made every
		// re-poll of an existing match fail as "identity changed".
		FixtureID: fixtureID, LeagueID: fixture.SeriesID, SeasonID: fixture.SeriesID,
		LocalTeamID: localID, VisitorTeamID: visitorID,
		LocalTeamName: localName, VisitorTeamName: visitorName,
		StartTime: fixture.StartingAt, Format: formatInfo.Format, ScheduledBalls: scheduledBalls,
		ScheduledOvers: formatInfo.ScheduledOvers, ReducedOvers: formatInfo.Reduced,
		ProviderStatus: providerStatus, ProviderState: state, Status: localStatus,
		CurrentInnings: currentInnings, BattingTeamID: battingTeamID,
		CurrentScore: current.Runs, Wickets: current.Wickets,
		LegalBalls: current.LegalBalls, Target: target,
		Innings: innings, Deliveries: deliveries,
		DeliveryWindowInnings: windowInnings, DeliveryWindowSequence: windowSequence,
		ProviderUpdatedAt: providerUpdated,
		LiveContext:       BuildLiveContext(mini),
		MatchPulse: BuildMatchPulse(mini, LiveContextInput{
			CurrentInnings: currentInnings, BattingTeamID: battingTeamID,
			LocalTeamID: localID, VisitorTeamID: visitorID,
			LocalTeamName: localName, VisitorTeamName: visitorName,
			CurrentScore: current.Runs, Wickets: current.Wickets,
			LegalBalls: current.LegalBalls, ScheduledBalls: scheduledBalls,
			Target: target, Status: localStatus,
		}),
	}
	for i := range projection.Innings {
		projection.Innings[i].SnapshotHash = inningsProjectionHash(projection.Innings[i], deliveries)
	}
	projection.ThisOver = BuildThisOver(deliveries, currentInnings, current.LegalBalls)
	projection.SnapshotHash = projectionHash(projection)
	return projection, nil
}

// inningsFromSnapshot builds the per-innings aggregates from the miniscore,
// which is authoritative, and falls back to the scorecard when the miniscore
// has not yet been populated (typically right at the toss).
// inningsFromSnapshot takes the miniscore explicitly rather than re-reading it
// from the snapshot: during play it is synthesised from the overs feed, and a
// second read of the raw (empty) commentary would silently zero every innings.
func inningsFromSnapshot(snapshot client.Snapshot, mini client.MiniScore, localID, visitorID int64, scheduledBalls, currentInnings int, localStatus string) ([]Innings, error) {
	byNumber := make(map[int]Innings)
	order := make([]int, 0, 2)

	for _, score := range mini.InningsScores {
		number := score.InningsID
		if number <= 0 || number > 2 {
			continue
		}
		legalBalls := OversToBalls(score.Overs.Float64())
		battingTeamID := score.BatTeamID
		if battingTeamID == 0 {
			battingTeamID = teamIDForName(snapshot, score.BatTeam, localID, visitorID)
		}
		in := Innings{
			Number:         number,
			BattingTeamID:  battingTeamID,
			Runs:           score.Score.Int(),
			Wickets:        score.Wickets.Int(),
			LegalBalls:     legalBalls,
			ScheduledBalls: scheduledBalls,
		}
		in.Complete = inningsComplete(localStatus, number, currentInnings, in.LegalBalls, scheduledBalls, in.Wickets) ||
			score.IsDeclared
		if _, seen := byNumber[number]; !seen {
			order = append(order, number)
		}
		byNumber[number] = in
	}

	// The miniscore's own live figures win for the innings in progress: the
	// innings_scores array can lag by a delivery at the moment a ball lands.
	if currentInnings > 0 && currentInnings <= 2 {
		in, exists := byNumber[currentInnings]
		if !exists {
			in = Innings{Number: currentInnings, ScheduledBalls: scheduledBalls}
			order = append(order, currentInnings)
		}
		if mini.BatTeamID != 0 {
			in.BattingTeamID = mini.BatTeamID
		}
		if runs := mini.BatTeamScore.Int(); runs >= in.Runs {
			in.Runs = runs
		}
		if wickets := mini.BatTeamWickets.Int(); wickets >= in.Wickets {
			in.Wickets = wickets
		}
		if balls := OversToBalls(mini.Overs.Float64()); balls >= in.LegalBalls {
			in.LegalBalls = balls
		}
		in.Complete = inningsComplete(localStatus, currentInnings, currentInnings, in.LegalBalls, scheduledBalls, in.Wickets)
		byNumber[currentInnings] = in
	}

	if len(order) == 0 {
		if currentInnings <= 0 {
			// Nothing has been bowled yet; an empty innings list is valid for a
			// fixture still in Preview.
			return nil, nil
		}
		return nil, fmt.Errorf("%w: no innings aggregate for innings %d", ErrIncompleteSnapshot, currentInnings)
	}

	sort.Ints(order)
	result := make([]Innings, 0, len(order))
	for _, number := range order {
		in := byNumber[number]
		if in.Runs < 0 || in.Wickets < 0 || in.Wickets > 10 || in.LegalBalls < 0 {
			return nil, fmt.Errorf("%w: innings %d aggregate is out of range", ErrIncompleteSnapshot, number)
		}
		if in.BattingTeamID != 0 && in.BattingTeamID != localID && in.BattingTeamID != visitorID {
			return nil, fmt.Errorf("%w: innings %d batting team %d is not a fixture team", ErrIncompleteSnapshot, number, in.BattingTeamID)
		}
		result = append(result, in)
	}
	return result, nil
}

// teamIDForName resolves a CricLive team label ("ENG", "England") to a fixture
// team ID. CricLive orders teams differently per endpoint, so matching is done
// on the name rather than on position.
func teamIDForName(snapshot client.Snapshot, name string, localID, visitorID int64) int64 {
	clean := strings.ToLower(strings.TrimSpace(name))
	if clean == "" {
		return 0
	}
	candidates := []struct {
		id     int64
		labels []string
	}{
		{snapshot.Fixture.LocalTeamID, []string{snapshot.Fixture.LocalTeamName, snapshot.Fixture.LocalTeamShort}},
		{snapshot.Fixture.VisitorTeamID, []string{snapshot.Fixture.VisitorTeamName, snapshot.Fixture.VisitorTeamShort}},
		{snapshot.Commentary.MatchHeader.Team1.ID, []string{snapshot.Commentary.MatchHeader.Team1.Name, snapshot.Commentary.MatchHeader.Team1.Short}},
		{snapshot.Commentary.MatchHeader.Team2.ID, []string{snapshot.Commentary.MatchHeader.Team2.Name, snapshot.Commentary.MatchHeader.Team2.Short}},
	}
	for _, candidate := range candidates {
		if candidate.id != localID && candidate.id != visitorID {
			continue
		}
		for _, label := range candidate.labels {
			if strings.EqualFold(strings.TrimSpace(label), clean) {
				return candidate.id
			}
		}
	}
	return 0
}

// deliveriesFromOvers converts the recent-overs window into deliveries. It
// returns the window floor so the store can tell "not sent this poll" apart
// from "deleted by the provider".
func deliveriesFromOvers(data client.OversData, battingTeamID int64, summary []Innings) ([]Delivery, int, int64) {
	// A delivery belongs to the side that batted in ITS innings. The overs feed
	// can still show the previous innings for a moment after discovery has
	// moved the fixture on, and stamping those balls with the new batting side
	// would re-hash stored deliveries into "corrections".
	teamForInnings := func(number int) int64 {
		for _, in := range summary {
			if in.Number == number && in.BattingTeamID != 0 {
				return in.BattingTeamID
			}
		}
		return battingTeamID
	}
	overs := make([]client.OverItem, 0, len(data.Overs))
	for _, over := range data.Overs {
		if len(over.Balls) == 0 {
			continue
		}
		overs = append(overs, over)
	}
	if len(overs) == 0 {
		return nil, 0, 0
	}
	// CricLive returns the newest over first.
	sort.SliceStable(overs, func(i, j int) bool {
		if overs[i].InningsID != overs[j].InningsID {
			return overs[i].InningsID < overs[j].InningsID
		}
		return overs[i].OverNumber.Float64() < overs[j].OverNumber.Float64()
	})

	deliveries := make([]Delivery, 0, len(overs)*8)
	windowInnings, windowSequence := 0, int64(0)
	for _, over := range overs {
		overNumber := int(math.Round(over.OverNumber.Float64()))
		if overNumber <= 0 {
			continue
		}
		innings := over.InningsID
		if innings <= 0 {
			innings = data.Innings
		}
		strikerName := firstName(over.BatStrikerNames)
		bowlerName := firstName(over.BowlNames)
		legalIndex := 0
		for index, token := range over.Balls {
			outcome, err := ParseBallToken(token)
			if err != nil {
				// A token we cannot price is skipped rather than failing the
				// whole snapshot: the miniscore already carries the
				// authoritative score, and the ball strip is display data.
				continue
			}
			sequence := int64(overNumber)*100 + int64(index) + 1
			delivery := Delivery{
				ProviderEventID: fmt.Sprintf("%d-%d-%d", innings, overNumber, index+1),
				ProviderBall:    fmt.Sprintf("%d.%d", overNumber-1, legalIndex+boolToInt(outcome.LegalBall)),
				Innings:         innings,
				Sequence:        sequence,
				TeamID:          teamForInnings(innings),
				BatterName:      strikerName,
				BowlerName:      bowlerName,
				TeamRuns:        outcome.TotalRuns,
				BatterRuns:      outcome.BatterRuns,
				LegalBall:       outcome.LegalBall,
				Extras:          outcome.Extras,
			}
			if outcome.IsWicket {
				delivery.Dismissal = &matches.Dismissal{
					Kind:         outcome.DismissalKind,
					BowlerCredit: outcome.BowlerCredit,
				}
			}
			if outcome.LegalBall {
				legalIndex++
			}
			delivery.PayloadHash = deliveryHash(delivery)
			deliveries = append(deliveries, delivery)
			if windowInnings == 0 || innings < windowInnings ||
				(innings == windowInnings && sequence < windowSequence) {
				windowInnings, windowSequence = innings, sequence
			}
		}
	}
	return deliveries, windowInnings, windowSequence
}

// BallOutcome is one parsed CricLive ball token.
type BallOutcome struct {
	TotalRuns     int
	BatterRuns    int
	LegalBall     bool
	IsWicket      bool
	DismissalKind string
	BowlerCredit  bool
	Extras        matches.DeliveryExtras
}

// ParseBallToken decodes CricLive's over notation. Verified against live
// payloads: "0".."6" are runs off the bat, "W" a wicket, "Wd"/"Wd2" a wide
// carrying that many total runs, "N"/"N4" a no-ball worth one penalty run plus
// runs off the bat, "L1" leg byes and "B4" byes (both legal deliveries).
func ParseBallToken(raw string) (BallOutcome, error) {
	token := strings.TrimSpace(raw)
	if token == "" {
		return BallOutcome{}, fmt.Errorf("%w: empty", ErrUnknownBallToken)
	}
	upper := strings.ToUpper(token)

	// A wicket can be combined with runs ("1W" for a run out completed after a
	// single). Strip the marker and price the remainder.
	wicket := false
	if strings.HasSuffix(upper, "W") && !strings.HasPrefix(upper, "WD") {
		wicket = true
		upper = strings.TrimSuffix(upper, "W")
	} else if strings.HasPrefix(upper, "W") && !strings.HasPrefix(upper, "WD") {
		wicket = true
		upper = strings.TrimPrefix(upper, "W")
	}

	outcome := BallOutcome{LegalBall: true, IsWicket: wicket}
	if wicket {
		outcome.DismissalKind = "wicket"
		outcome.BowlerCredit = true
	}

	switch {
	case upper == "":
		return outcome, nil
	case strings.HasPrefix(upper, "WD"):
		// The digit is the total charged to the batting side, not an addition
		// to the one-run wide penalty: "Wd2" is two runs in all.
		runs := trailingInt(upper, "WD", 1)
		outcome.LegalBall = false
		outcome.TotalRuns = runs
		outcome.Extras.Wides = runs
		return outcome, nil
	case strings.HasPrefix(upper, "NB"), strings.HasPrefix(upper, "N"):
		prefix := "N"
		if strings.HasPrefix(upper, "NB") {
			prefix = "NB"
		}
		// The digit is runs off the bat; the no-ball itself adds one.
		batter := trailingInt(upper, prefix, 0)
		outcome.LegalBall = false
		outcome.BatterRuns = batter
		outcome.TotalRuns = batter + 1
		outcome.Extras.NoBalls = 1
		return outcome, nil
	case strings.HasPrefix(upper, "LB"), strings.HasPrefix(upper, "L"):
		prefix := "L"
		if strings.HasPrefix(upper, "LB") {
			prefix = "LB"
		}
		runs := trailingInt(upper, prefix, 1)
		outcome.TotalRuns = runs
		outcome.Extras.LegByes = runs
		return outcome, nil
	case strings.HasPrefix(upper, "B"):
		runs := trailingInt(upper, "B", 1)
		outcome.TotalRuns = runs
		outcome.Extras.Byes = runs
		return outcome, nil
	case strings.HasPrefix(upper, "P"):
		runs := trailingInt(upper, "P", 5)
		outcome.TotalRuns = runs
		outcome.Extras.Penalties = runs
		return outcome, nil
	}

	runs, err := strconv.Atoi(upper)
	if err != nil || runs < 0 {
		return BallOutcome{}, fmt.Errorf("%w: %q", ErrUnknownBallToken, raw)
	}
	outcome.TotalRuns = runs
	outcome.BatterRuns = runs
	return outcome, nil
}

func trailingInt(token, prefix string, fallback int) int {
	rest := strings.TrimSpace(strings.TrimPrefix(token, prefix))
	if rest == "" {
		return fallback
	}
	value, err := strconv.Atoi(rest)
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

// OversToBalls converts CricLive's over notation to a legal-ball count.
// CricLive counts a completed over as ".6" (38.6 is 39 completed overs), so the
// fractional part is a ball count from 0 to 6 rather than a decimal fraction.
func OversToBalls(overs float64) int {
	if overs <= 0 {
		return 0
	}
	whole := math.Floor(overs + 1e-9)
	balls := int(math.Round((overs - whole) * 10))
	if balls < 0 {
		balls = 0
	}
	if balls > 6 {
		balls = 6
	}
	return int(whole)*6 + balls
}

// BallsToOversText renders a legal-ball count back into CricLive's notation.
func BallsToOversText(balls int) string {
	if balls <= 0 {
		return "0.0"
	}
	return fmt.Sprintf("%d.%d", balls/6, balls%6)
}

func normalizeFormat(raw string) (string, int, error) {
	clean := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(raw), "_", ""))
	clean = strings.ReplaceAll(clean, " ", "")
	clean = strings.ReplaceAll(clean, "-", "")
	switch clean {
	case "t20", "t20i", "t10":
		if clean == "t10" {
			return "", 0, fmt.Errorf("%w: %s", ErrUnsupportedFormat, raw)
		}
		return "T20", 120, nil
	case "odi", "odm", "lista":
		return "ODI", 300, nil
	default:
		// TEST, first class, and the hundred are deliberately unsupported:
		// their innings have no fixed ball count to price against.
		return "", 0, fmt.Errorf("%w: %s", ErrUnsupportedFormat, raw)
	}
}

// ClassifyFormat exposes the reducer's allowlist to fixture discovery without
// requiring a full live snapshot.
func ClassifyFormat(raw string) (string, int, error) {
	return normalizeFormat(raw)
}

// ClassifyFormatInfo is ClassifyFormat with the scheduled-overs detail
// publication needs to gate trading without hiding the fixture.
func ClassifyFormatInfo(raw string) (FormatInfo, error) {
	format, scheduledBalls, err := normalizeFormat(raw)
	if err != nil {
		return FormatInfo{}, err
	}
	standardOvers := scheduledBalls / 6
	return FormatInfo{
		Format: format, ScheduledBalls: scheduledBalls,
		ScheduledOvers: standardOvers, StandardOvers: standardOvers,
	}, nil
}

func inningsComplete(localStatus string, number, current, legalBalls, scheduledBalls, wickets int) bool {
	if localStatus == matches.StatusCompleted || localStatus == matches.StatusAbandoned {
		return true
	}
	if current > number {
		return true
	}
	return legalBalls >= scheduledBalls || wickets >= 10
}

func latestCommentaryTime(items []client.CommentaryItem) *time.Time {
	var latest time.Time
	for _, item := range items {
		if item.Timestamp <= 0 {
			continue
		}
		when := time.UnixMilli(item.Timestamp).UTC()
		if when.After(latest) {
			latest = when
		}
	}
	if latest.IsZero() {
		return nil
	}
	return &latest
}

func deliveryHash(delivery Delivery) string {
	clone := delivery
	clone.PayloadHash = ""
	clone.ProviderUpdatedAt = nil
	encoded, _ := json.Marshal(clone)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func projectionHash(projection Projection) string {
	clone := projection
	clone.SnapshotHash = ""
	clone.ProviderUpdatedAt = nil
	clone.Deliveries = append([]Delivery(nil), projection.Deliveries...)
	for i := range clone.Deliveries {
		clone.Deliveries[i].ProviderUpdatedAt = nil
	}
	encoded, _ := json.Marshal(clone)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func inningsProjectionHash(innings Innings, deliveries []Delivery) string {
	type hashInput struct {
		Innings    Innings
		Deliveries []Delivery
	}
	clone := innings
	clone.SnapshotHash = ""
	relevant := make([]Delivery, 0)
	for _, delivery := range deliveries {
		if delivery.Innings == innings.Number {
			delivery.ProviderUpdatedAt = nil
			relevant = append(relevant, delivery)
		}
	}
	encoded, _ := json.Marshal(hashInput{Innings: clone, Deliveries: relevant})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func firstName(values []string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// displayBall splits provider ball notation ("38.4") into over and ball.
func displayBall(value string) (int, int) {
	parts := strings.SplitN(strings.TrimSpace(value), ".", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	over, err1 := strconv.Atoi(parts[0])
	ball, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	return over, ball
}

// miniScoreFromOvers folds a play-time poll -- the overs feed plus the innings
// summary discovery wrote on the fixture -- into the miniscore shape so the
// rest of the reduction is unchanged. The latest over is the fresher source
// for the running score and the players at the crease; the summary supplies
// the innings that are already complete and the chase target.
func miniScoreFromOvers(snapshot client.Snapshot) client.MiniScore {
	fixture := snapshot.Fixture
	mini := client.MiniScore{
		State: fixture.State, CustomStatus: fixture.StatusDetail, MatchFormat: fixture.Format,
	}
	shortName := func(teamID int64) string {
		switch teamID {
		case fixture.LocalTeamID:
			return fixture.LocalTeamShort
		case fixture.VisitorTeamID:
			return fixture.VisitorTeamShort
		}
		return ""
	}
	for _, in := range fixture.LiveInnings {
		mini.InningsScores = append(mini.InningsScores, client.InningsScore{
			InningsID: in.Number, BatTeam: shortName(in.TeamID), BatTeamID: in.TeamID,
			Score: client.FlexInt(in.Runs), Wickets: client.FlexInt(in.Wickets),
			Overs: client.FlexFloat(in.Overs), IsDeclared: in.Declared,
		})
		if in.Number >= mini.InningsID {
			mini.InningsID = in.Number
			mini.BatTeamID = in.TeamID
			mini.BatTeamScore = client.FlexInt(in.Runs)
			mini.BatTeamWickets = client.FlexInt(in.Wickets)
			mini.Overs = client.FlexFloat(in.Overs)
			if in.Target > 0 {
				target := in.Target
				mini.Target = &target
			}
		}
	}

	latest := latestOverItem(snapshot.Overs)
	if latest == nil {
		return mini
	}
	if latest.InningsID > mini.InningsID {
		// The ball feed is ahead of discovery: a new innings has begun. In a
		// two-innings match the side now batting is whichever did not bat first.
		mini.InningsID = latest.InningsID
		mini.BatTeamID = 0
		for _, in := range fixture.LiveInnings {
			if in.Number < latest.InningsID && in.TeamID != 0 {
				if in.TeamID == fixture.LocalTeamID {
					mini.BatTeamID = fixture.VisitorTeamID
				} else {
					mini.BatTeamID = fixture.LocalTeamID
				}
			}
		}
		if mini.BatTeamID == 0 {
			// The first ball of the match landed before discovery listed any
			// innings: the over itself names the side batting.
			mini.BatTeamID = teamIDForName(snapshot, latest.BatTeamName, fixture.LocalTeamID, fixture.VisitorTeamID)
		}
	}
	if latest.InningsID != mini.InningsID {
		// The ball feed lags discovery: the new innings has no over yet, and the
		// batters, bowler and score of the previous one do not belong to it.
		return mini
	}
	legal := 0
	wicketThisOver := false
	for _, token := range latest.Balls {
		outcome, err := ParseBallToken(token)
		if err != nil {
			continue
		}
		if outcome.LegalBall {
			legal++
		}
		if outcome.IsWicket {
			wicketThisOver = true
		}
	}
	mini.BatTeamScore = latest.Score
	mini.BatTeamWickets = latest.Wickets
	mini.Overs = client.FlexFloat(oversNotation(int(math.Round(latest.OverNumber.Float64())), legal))
	mini.RecentOvers = latest.OverSummary
	if wicketThisOver {
		mini.LastWicket = "Wicket this over"
	}
	inningsOvers := oversOfInnings(snapshot.Overs, mini.InningsID)
	strikerName, nonStrikerName := battersAtCrease(inningsOvers)
	mini.Striker = client.BattingLine{Name: strikerName, Runs: latest.BatStrikerRuns, Balls: latest.BatStrikerBalls}
	mini.NonStriker = client.BattingLine{Name: nonStrikerName, Runs: latest.BatNonStrikerRuns, Balls: latest.BatNonStrikerBalls}
	mini.BowlerStriker = client.BowlingLine{
		Name: firstName(latest.BowlNames), Overs: latest.BowlOvers,
		Maidens: latest.BowlMaidens, Runs: latest.BowlRuns, Wickets: latest.BowlWickets,
	}
	mini.Partnership = partnershipFromOvers(inningsOvers)
	return mini
}

// oversOfInnings is the innings' visible overs with balls in them, oldest first.
func oversOfInnings(data client.OversData, innings int) []client.OverItem {
	overs := make([]client.OverItem, 0, len(data.Overs))
	for _, over := range data.Overs {
		if over.InningsID == innings && len(over.Balls) > 0 {
			overs = append(overs, over)
		}
	}
	sort.SliceStable(overs, func(i, j int) bool {
		return overs[i].OverNumber.Float64() < overs[j].OverNumber.Float64()
	})
	return overs
}

// batterTrack follows one batter's running figures through the overs window.
type batterTrack struct {
	name  string
	runs  int
	balls int
	live  bool
}

// continues reports whether figures could be this batter's later figures: a
// batter's runs and balls only ever grow.
func (t batterTrack) continues(runs, balls int) bool {
	return t.live && runs >= t.runs && balls >= t.balls
}

// battersAtCrease names the striker and non-striker of an innings' latest
// over. In most overs CricLive prints both batters under batStrikerNames and
// leaves batNonStrikerNames empty, so the names carry no role — the striker
// figures belong to the first name in one over and the second in the next.
// The figures do carry the role, and a batter's runs and balls only grow, so
// walking the window forward keeps track of who is who: an over that names
// both roles binds a name to each set of figures, a batter whose figures
// reset was dismissed, and a name new to the list is the one who replaced
// them. A window that never disambiguates falls back to list order.
func battersAtCrease(overs []client.OverItem) (striker, nonStriker string) {
	var on, off batterTrack
	seen := map[string]bool{}
	var listed []string
	for _, over := range overs {
		listed = cleanNames(over.BatStrikerNames)
		nonNames := cleanNames(over.BatNonStrikerNames)
		sRuns, sBalls := over.BatStrikerRuns.Int(), over.BatStrikerBalls.Int()
		nRuns, nBalls := over.BatNonStrikerRuns.Int(), over.BatNonStrikerBalls.Int()
		if len(nonNames) > 0 && len(listed) == 1 {
			on = batterTrack{name: listed[0], runs: sRuns, balls: sBalls, live: true}
			off = batterTrack{name: nonNames[0], runs: nRuns, balls: nBalls, live: true}
			listed = append(listed, nonNames[0])
			seen[on.name], seen[off.name] = true, true
			continue
		}
		prev := [2]batterTrack{on, off}
		sFrom, nFrom := resolveBatterTracks(prev, sRuns, sBalls, nRuns, nBalls)
		next := [2]batterTrack{
			{runs: sRuns, balls: sBalls, live: true},
			{runs: nRuns, balls: nBalls, live: true},
		}
		if sFrom >= 0 {
			next[0].name = prev[sFrom].name
		}
		if nFrom >= 0 {
			next[1].name = prev[nFrom].name
		}
		fresh := make([]string, 0, 1)
		for _, name := range listed {
			if !seen[name] {
				fresh = append(fresh, name)
			}
		}
		// Figures that continue nobody belong to a batter who has just come
		// in; a name new to the window is that batter.
		if sFrom < 0 && next[0].name == "" && len(fresh) == 1 {
			next[0].name = fresh[0]
		}
		if nFrom < 0 && next[1].name == "" && len(fresh) == 1 {
			next[1].name = fresh[0]
		}
		// With two names listed, whichever the other batter is not is this one.
		if len(listed) == 2 {
			for i := range next {
				if next[i].name == "" && next[1-i].name != "" {
					next[i].name = otherName(listed, next[1-i].name)
				}
			}
		}
		on, off = next[0], next[1]
		for _, name := range listed {
			seen[name] = true
		}
	}
	if on.name == "" && off.name == "" && len(listed) > 0 {
		on.name = listed[0]
	}
	if on.name == "" {
		on.name = otherName(listed, off.name)
	}
	if off.name == "" {
		off.name = otherName(listed, on.name)
	}
	return on.name, off.name
}

// resolveBatterTracks matches the striker and non-striker figures of an over
// to the two batters of the previous over by continuity. It returns the index
// of the batter each set of figures continues, or -1 when nobody does (a new
// batter) or when the figures fit both.
func resolveBatterTracks(prev [2]batterTrack, sRuns, sBalls, nRuns, nBalls int) (int, int) {
	var sMask, nMask int
	for i, track := range prev {
		if track.continues(sRuns, sBalls) {
			sMask |= 1 << i
		}
		if track.continues(nRuns, nBalls) {
			nMask |= 1 << i
		}
	}
	single := func(mask int) int {
		switch mask {
		case 1:
			return 0
		case 2:
			return 1
		}
		return -1
	}
	sFrom, nFrom := single(sMask), single(nMask)
	// When one role is settled the other takes whoever is left.
	if sFrom >= 0 && nFrom < 0 {
		nFrom = single(nMask &^ (1 << sFrom))
	}
	if nFrom >= 0 && sFrom < 0 {
		sFrom = single(sMask &^ (1 << nFrom))
	}
	if sFrom >= 0 && sFrom == nFrom {
		return -1, -1
	}
	return sFrom, nFrom
}

func otherName(listed []string, taken string) string {
	for _, name := range listed {
		if name != taken {
			return name
		}
	}
	return ""
}

func cleanNames(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// latestOverItem is the most recent over with any balls in it.
func latestOverItem(data client.OversData) *client.OverItem {
	var latest *client.OverItem
	for i := range data.Overs {
		over := &data.Overs[i]
		if len(over.Balls) == 0 {
			continue
		}
		if latest == nil || over.InningsID > latest.InningsID ||
			(over.InningsID == latest.InningsID && over.OverNumber.Float64() > latest.OverNumber.Float64()) {
			latest = over
		}
	}
	return latest
}

// oversNotation renders "legal balls into over N" in CricLive's dotted form,
// where a completed over is written ".6" rather than rolling to the next whole
// number (12.6 is twelve overs and six balls; 13.0 is never emitted).
func oversNotation(overNumber, legalBalls int) float64 {
	if overNumber <= 0 {
		return 0
	}
	if legalBalls > 6 {
		legalBalls = 6
	}
	return float64(overNumber-1) + float64(legalBalls)/10
}

// partnershipFromOvers replays the innings' visible overs (oldest first) and
// counts runs and balls since the last wicket. /cricket/overs returns only
// recent overs, so a stand that began before the window is under-counted from
// the window's edge; the miniscore had the exact figure, but not at twice the
// request cost.
func partnershipFromOvers(overs []client.OverItem) client.Partnership {
	runs, balls := 0, 0
	for _, over := range overs {
		for _, token := range over.Balls {
			outcome, err := ParseBallToken(token)
			if err != nil {
				continue
			}
			if outcome.IsWicket {
				runs, balls = 0, 0
				continue
			}
			runs += outcome.TotalRuns
			if outcome.LegalBall {
				balls++
			}
		}
	}
	return client.Partnership{Runs: client.FlexInt(runs), Balls: client.FlexInt(balls)}
}
