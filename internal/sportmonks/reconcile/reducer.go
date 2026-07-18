package reconcile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

var (
	ErrIncompleteSnapshot = errors.New("incomplete Sportmonks snapshot")
	ErrUnknownScore       = errors.New("unknown Sportmonks score type")
	ErrAggregateDrift     = errors.New("Sportmonks aggregate does not match deliveries")
	ErrUnsupportedFormat  = errors.New("unsupported cricket format")
)

type ScoreDefinition struct {
	ID         int64
	Name       string
	Runs       int
	Four       bool
	Six        bool
	Bye        int
	LegBye     int
	NoBall     int
	NoBallRuns int
	Penalty    int
	IsWicket   bool
	LegalBall  bool
	Out        bool
}

type Catalog map[int64]ScoreDefinition

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
	FixtureID         int64
	LeagueID          int64
	SeasonID          int64
	LocalTeamID       int64
	VisitorTeamID     int64
	LocalTeamName     string
	VisitorTeamName   string
	StartTime         time.Time
	Format            string
	ScheduledBalls    int
	ProviderStatus    string
	Status            string
	CurrentInnings    int
	BattingTeamID     int64
	CurrentScore      int
	Wickets           int
	LegalBalls        int
	Target            int
	Innings           []Innings
	Deliveries        []Delivery
	LiveContext       *matches.LiveMatchContext
	MatchPulse        *matches.MatchPulse
	ThisOver          []matches.OverBall
	ProviderUpdatedAt *time.Time
	SnapshotHash      string
}

// CatalogFromJSON accepts either an array, a {data:[...]} envelope, or a
// Sportmonks relation wrapper. It is intentionally strict about IDs so a
// catalog schema change fails closed instead of silently mispricing a ball.
func CatalogFromJSON(raw []byte) (Catalog, error) {
	value, err := decodeJSON(raw)
	if err != nil {
		return nil, err
	}
	items, err := unwrapItems(value)
	if err != nil {
		return nil, fmt.Errorf("scores catalog: %w", err)
	}
	catalog := make(Catalog, len(items))
	for _, item := range items {
		id, ok := int64Field(item, "id")
		if !ok || id <= 0 {
			return nil, fmt.Errorf("scores catalog entry has no valid id")
		}
		if _, duplicate := catalog[id]; duplicate {
			return nil, fmt.Errorf("scores catalog contains duplicate id %d", id)
		}
		name, hasName := stringField(item, "name")
		runs, hasRuns := intField(item, "runs")
		legal, hasLegal := boolField(item, "ball")
		wicket, hasWicket := boolField(item, "is_wicket")
		out, hasOut := boolField(item, "out")
		if !hasName || name == "" || !hasRuns || runs < 0 || !hasLegal || !hasWicket || !hasOut {
			return nil, fmt.Errorf("scores catalog entry %d lacks required semantics", id)
		}
		bye := extraRuns(item["bye"], runs)
		legBye := extraRuns(item["leg_bye"], runs)
		noBallRuns, hasNoBallRuns := intField(item, "noball_runs")
		if hasNoBallRuns && noBallRuns < 0 {
			return nil, fmt.Errorf("scores catalog entry %d has negative no-ball runs", id)
		}
		noBall := extraRuns(item["noball"], 1)
		penalty, validPenalty := penaltyRuns(name)
		if strings.Contains(strings.ToLower(name), "penalty") && !validPenalty {
			return nil, fmt.Errorf("scores catalog penalty entry %d has no explicit run value", id)
		}
		if noBall > 0 || strings.Contains(strings.ToLower(name), "wide") {
			legal = false
		}
		four, _ := boolField(item, "four")
		six, _ := boolField(item, "six")
		catalog[id] = ScoreDefinition{
			ID: id, Name: name, Runs: runs, Four: four, Six: six,
			Bye: bye, LegBye: legBye, NoBall: noBall,
			NoBallRuns: noBallRuns, Penalty: penalty, IsWicket: wicket,
			LegalBall: legal, Out: out,
		}
	}
	return catalog, nil
}

// ReduceFixtureJSON normalizes and validates one fixture object. It accepts a
// direct object or {data:{...}}, which keeps the reducer independent of the
// HTTP client's envelope representation.
func ReduceFixtureJSON(raw []byte, catalog Catalog) (Projection, error) {
	value, err := decodeJSON(raw)
	if err != nil {
		return Projection{}, err
	}
	root, err := unwrapObject(value)
	if err != nil {
		return Projection{}, err
	}

	fixtureID, ok := int64Field(root, "id")
	if !ok || fixtureID <= 0 {
		return Projection{}, fmt.Errorf("fixture id is missing")
	}
	leagueID, hasLeague := int64Field(root, "league_id")
	seasonID, hasSeason := int64Field(root, "season_id")
	localID, hasLocal := int64Field(root, "localteam_id")
	visitorID, hasVisitor := int64Field(root, "visitorteam_id")
	startTime, hasStart := timeField(root, "starting_at")
	status, hasStatus := stringField(root, "status")
	if !hasLeague || leagueID <= 0 || !hasSeason || seasonID <= 0 ||
		!hasLocal || localID <= 0 || !hasVisitor || visitorID <= 0 || localID == visitorID ||
		!hasStart || startTime.IsZero() || !hasStatus || status == "" {
		return Projection{}, fmt.Errorf("%w: fixture identity is incomplete", ErrIncompleteSnapshot)
	}
	if superOver, _ := boolField(root, "super_over"); superOver {
		return Projection{}, fmt.Errorf("%w: super over", ErrUnsupportedFormat)
	}
	if meaningfulValue(root["rpc_overs"]) || meaningfulValue(root["rpc_target"]) ||
		meaningfulDLSData(root["localteam_dl_data"]) || meaningfulDLSData(root["visitorteam_dl_data"]) {
		return Projection{}, fmt.Errorf("%w: reduced/DLS target is not deterministic", ErrUnsupportedFormat)
	}
	formatRaw, _ := stringField(root, "type")
	format, scheduledBalls, err := normalizeSnapshotFormat(root, formatRaw)
	if err != nil {
		return Projection{}, err
	}

	ballItems, err := requiredRelation(root, "balls")
	if err != nil {
		return Projection{}, err
	}
	runItems, err := requiredRelation(root, "runs")
	if err != nil {
		return Projection{}, err
	}
	scoreboardItems, err := requiredRelation(root, "scoreboards")
	if err != nil {
		return Projection{}, err
	}
	battingItems, err := requiredRelation(root, "batting")
	if err != nil {
		return Projection{}, err
	}
	bowlingItems, err := requiredRelation(root, "bowling")
	if err != nil {
		return Projection{}, err
	}

	currentInnings := inningsFromStatus(status)
	if currentInnings > 2 {
		return Projection{}, fmt.Errorf("%w: innings %d", ErrUnsupportedFormat, currentInnings)
	}
	deliveries := make([]Delivery, 0, len(ballItems))
	providerUpdated := (*time.Time)(nil)
	seenDeliveryIDs := make(map[string]struct{}, len(ballItems))
	for _, ball := range ballItems {
		delivery, err := reduceBall(ball, catalog)
		if err != nil {
			return Projection{}, fmt.Errorf("fixture %d: %w", fixtureID, err)
		}
		if _, duplicate := seenDeliveryIDs[delivery.ProviderEventID]; duplicate {
			return Projection{}, fmt.Errorf("%w: duplicate delivery id %s", ErrIncompleteSnapshot, delivery.ProviderEventID)
		}
		seenDeliveryIDs[delivery.ProviderEventID] = struct{}{}
		deliveries = append(deliveries, delivery)
		if delivery.ProviderUpdatedAt != nil && (providerUpdated == nil || delivery.ProviderUpdatedAt.After(*providerUpdated)) {
			t := *delivery.ProviderUpdatedAt
			providerUpdated = &t
		}
	}
	sort.Slice(deliveries, func(i, j int) bool {
		if deliveries[i].Innings != deliveries[j].Innings {
			return deliveries[i].Innings < deliveries[j].Innings
		}
		left, _ := strconv.ParseInt(deliveries[i].ProviderEventID, 10, 64)
		right, _ := strconv.ParseInt(deliveries[j].ProviderEventID, 10, 64)
		return left < right
	})
	sequences := make(map[int]int64)
	for i := range deliveries {
		sequences[deliveries[i].Innings]++
		deliveries[i].Sequence = sequences[deliveries[i].Innings]
		deliveries[i].PayloadHash = deliveryHash(deliveries[i])
	}

	derived := deriveInnings(deliveries, scheduledBalls)
	runs, err := aggregateRecords(runItems, "runs")
	if err != nil {
		return Projection{}, err
	}
	scoreboards, err := aggregateRecords(scoreboardItems, "scoreboards")
	if err != nil {
		return Projection{}, err
	}
	allInnings := unionInnings(derived, runs, scoreboards)
	if len(allInnings) == 0 && currentInnings > 0 {
		allInnings = []int{currentInnings}
	}
	innings := make([]Innings, 0, len(allInnings))
	for _, number := range allInnings {
		d := derived[number]
		r, hasRun := runs[number]
		s, hasScoreboard := scoreboards[number]
		if !hasRun || !hasScoreboard {
			return Projection{}, fmt.Errorf("%w: innings %d aggregate missing", ErrIncompleteSnapshot, number)
		}
		if d.Runs != r.Runs || d.Wickets != r.Wickets || d.Runs != s.Runs || d.Wickets != s.Wickets {
			return Projection{}, fmt.Errorf("%w: innings %d balls=%d/%d runs=%d/%d scoreboard=%d/%d",
				ErrAggregateDrift, number, d.Runs, d.Wickets, r.Runs, r.Wickets, s.Runs, s.Wickets)
		}
		if !r.HasLegalBalls || !s.HasLegalBalls {
			return Projection{}, fmt.Errorf("%w: innings %d aggregate lacks legal-ball count", ErrIncompleteSnapshot, number)
		}
		if r.LegalBalls != d.LegalBalls {
			return Projection{}, fmt.Errorf("%w: innings %d legal balls=%d runs relation=%d", ErrAggregateDrift, number, d.LegalBalls, r.LegalBalls)
		}
		if s.LegalBalls != d.LegalBalls {
			return Projection{}, fmt.Errorf("%w: innings %d legal balls=%d scoreboard=%d", ErrAggregateDrift, number, d.LegalBalls, s.LegalBalls)
		}
		d.Number = number
		d.ScheduledBalls = scheduledBalls
		d.BattingTeamID = firstNonZero(d.BattingTeamID, r.TeamID, s.TeamID)
		if d.BattingTeamID != 0 && d.BattingTeamID != localID && d.BattingTeamID != visitorID {
			return Projection{}, fmt.Errorf("%w: innings %d batting team %d is not a fixture team", ErrIncompleteSnapshot, number, d.BattingTeamID)
		}
		d.Complete = inningsComplete(status, number, currentInnings, d.LegalBalls, scheduledBalls, d.Wickets)
		d.SnapshotHash = inningsProjectionHash(d, deliveries)
		innings = append(innings, d)
	}

	if currentInnings == 0 && len(innings) > 0 {
		currentInnings = innings[len(innings)-1].Number
	}
	target, _ := intField(root, "target")
	if target == 0 && (currentInnings == 2 || normalizeStatus(status) == matches.StatusInningsBreak) {
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
			innings[i].SnapshotHash = inningsProjectionHash(innings[i], deliveries)
		}
	}
	current := Innings{Number: currentInnings, ScheduledBalls: scheduledBalls}
	for _, in := range innings {
		if in.Number == currentInnings {
			current = in
			break
		}
	}
	if currentInnings > 0 && current.BattingTeamID == 0 {
		if IsExplicitTerminalProviderStatus(status) && len(innings) > 0 {
			current = innings[len(innings)-1]
			currentInnings = current.Number
		} else {
			return Projection{}, fmt.Errorf("%w: current innings batting team is missing", ErrIncompleteSnapshot)
		}
	}

	localName := relationName(root["localteam"])
	visitorName := relationName(root["visitorteam"])
	liveInput := LiveContextInput{
		CurrentInnings: currentInnings, BattingTeamID: current.BattingTeamID,
		LocalTeamID: localID, VisitorTeamID: visitorID,
		LocalTeamName: localName, VisitorTeamName: visitorName,
		CurrentScore: current.Runs, Wickets: current.Wickets,
		LegalBalls: current.LegalBalls, ScheduledBalls: scheduledBalls,
		Target: target, Deliveries: deliveries,
	}
	projection := Projection{
		FixtureID: fixtureID, LeagueID: leagueID, SeasonID: seasonID,
		LocalTeamID: localID, VisitorTeamID: visitorID,
		LocalTeamName: localName, VisitorTeamName: visitorName,
		StartTime: startTime, Format: format, ScheduledBalls: scheduledBalls,
		ProviderStatus: status, Status: normalizeStatus(status),
		CurrentInnings: currentInnings, BattingTeamID: current.BattingTeamID,
		CurrentScore: current.Runs, Wickets: current.Wickets,
		LegalBalls: current.LegalBalls, Target: target,
		Innings: innings, Deliveries: deliveries, ProviderUpdatedAt: providerUpdated,
		LiveContext: BuildLiveContext(battingItems, bowlingItems, liveInput),
		MatchPulse:  BuildMatchPulse(liveInput),
		ThisOver:    BuildThisOver(deliveries, currentInnings, current.LegalBalls),
	}
	projection.SnapshotHash = projectionHash(projection)
	return projection, nil
}

func reduceBall(ball map[string]any, catalog Catalog) (Delivery, error) {
	id, ok := int64Field(ball, "id")
	if !ok || id <= 0 {
		return Delivery{}, fmt.Errorf("delivery has no stable id")
	}
	scoreID, ok := int64Field(ball, "score_id")
	if !ok {
		return Delivery{}, fmt.Errorf("delivery %d has no score_id", id)
	}
	score, ok := catalog[scoreID]
	if !ok {
		return Delivery{}, fmt.Errorf("%w %d on delivery %d", ErrUnknownScore, scoreID, id)
	}
	providerBall, _ := stringField(ball, "ball")
	if providerBall == "" {
		if n, exists := numberField(ball, "ball"); exists {
			providerBall = n.String()
		}
	}
	scoreboard, _ := stringField(ball, "scoreboard")
	innings := inningsNumber(scoreboard)
	if innings == 0 {
		if n, exists := intField(ball, "inning"); exists {
			innings = n
		}
	}
	if innings <= 0 || innings > 2 {
		return Delivery{}, fmt.Errorf("%w: delivery %d innings %q", ErrUnsupportedFormat, id, scoreboard)
	}

	extras := matches.DeliveryExtras{Byes: score.Bye, LegByes: score.LegBye, NoBalls: score.NoBallRuns, Penalties: score.Penalty}
	lowerName := strings.ToLower(score.Name)
	if score.NoBall > 0 && extras.NoBalls == 0 {
		extras.NoBalls = score.NoBall
	}
	if strings.Contains(lowerName, "wide") {
		extras.Wides = score.Runs
	}
	batterRuns := score.Runs
	if extras.Wides > 0 || extras.Byes > 0 || extras.LegByes > 0 || extras.Penalties > 0 {
		batterRuns = 0
	} else if extras.NoBalls > 0 {
		batterRuns -= extras.NoBalls
	}
	if batterRuns < 0 {
		return Delivery{}, fmt.Errorf("delivery %d wide/no-ball extras exceed catalog runs", id)
	}
	teamRuns := batterRuns + extras.Total()
	teamID, _ := int64Field(ball, "team_id")
	batterID, _ := int64Field(ball, "batsman_id")
	bowlerID, _ := int64Field(ball, "bowler_id")
	updatedAt, hasUpdated := timeField(ball, "updated_at")
	var updatedPtr *time.Time
	if hasUpdated {
		updatedPtr = &updatedAt
	}

	var dismissal *matches.Dismissal
	if score.IsWicket || score.Out {
		playerID, _ := int64Field(ball, "batsmanout_id")
		fielderID, _ := int64Field(ball, "catchstump_id")
		if fielderID == 0 {
			fielderID, _ = int64Field(ball, "runout_by_id")
		}
		dismissal = &matches.Dismissal{
			Kind: dismissalKind(score.Name), PlayerID: playerID,
			FielderID: fielderID, BowlerCredit: bowlerGetsCredit(score.Name),
		}
	}
	delivery := Delivery{
		ProviderEventID: strconv.FormatInt(id, 10), ProviderScoreID: scoreID,
		ProviderBall: providerBall, Innings: innings, TeamID: teamID,
		BatterID: batterID, BowlerID: bowlerID, TeamRuns: teamRuns,
		BatterRuns: batterRuns, LegalBall: score.LegalBall, Extras: extras,
		Dismissal: dismissal, ProviderUpdatedAt: updatedPtr,
	}
	return delivery, nil
}

type aggregate struct {
	Runs          int
	Wickets       int
	LegalBalls    int
	HasLegalBalls bool
	TeamID        int64
}

func aggregateRecords(items []map[string]any, relation string) (map[int]aggregate, error) {
	out := make(map[int]aggregate)
	for _, item := range items {
		inning := 0
		if n, ok := intField(item, "inning"); ok {
			inning = n
		}
		if inning == 0 {
			value, _ := stringField(item, "scoreboard")
			inning = inningsNumber(value)
		}
		if inning <= 0 || inning > 2 {
			continue
		}
		runs, ok := intField(item, "score")
		if !ok {
			runs, ok = intField(item, "total")
		}
		if !ok {
			runs, ok = intField(item, "runs")
		}
		wickets, hasWickets := intField(item, "wickets")
		if !ok || !hasWickets {
			return nil, fmt.Errorf("%w: %s innings %d lacks score/wickets", ErrIncompleteSnapshot, relation, inning)
		}
		entry := aggregate{Runs: runs, Wickets: wickets}
		entry.TeamID, _ = int64Field(item, "team_id")
		if overs, hasOvers := stringField(item, "overs"); hasOvers {
			if balls, valid := ballsFromOvers(overs); valid {
				entry.LegalBalls, entry.HasLegalBalls = balls, true
			}
		} else if n, exists := numberField(item, "overs"); exists {
			if balls, valid := ballsFromOvers(n.String()); valid {
				entry.LegalBalls, entry.HasLegalBalls = balls, true
			}
		}
		// Prefer the total scoreboard row if the relation contains multiple
		// records for the same innings.
		kind, _ := stringField(item, "type")
		if previous, exists := out[inning]; !exists || strings.EqualFold(kind, "total") || entry.Runs >= previous.Runs {
			out[inning] = entry
		}
	}
	return out, nil
}

func deriveInnings(deliveries []Delivery, scheduled int) map[int]Innings {
	out := make(map[int]Innings)
	for _, delivery := range deliveries {
		inning := out[delivery.Innings]
		inning.Number = delivery.Innings
		inning.ScheduledBalls = scheduled
		if inning.BattingTeamID == 0 {
			inning.BattingTeamID = delivery.TeamID
		}
		inning.Runs += delivery.TeamRuns
		if delivery.LegalBall {
			inning.LegalBalls++
		}
		if delivery.Dismissal != nil {
			inning.Wickets++
		}
		out[delivery.Innings] = inning
	}
	return out
}

func requiredRelation(root map[string]any, name string) ([]map[string]any, error) {
	value, exists := root[name]
	if !exists || value == nil {
		return nil, fmt.Errorf("%w: relation %s is missing", ErrIncompleteSnapshot, name)
	}
	items, err := unwrapItems(value)
	if err != nil {
		return nil, fmt.Errorf("%w: relation %s: %v", ErrIncompleteSnapshot, name, err)
	}
	if wrapper, ok := value.(map[string]any); ok {
		if err := requireTerminalPagination(wrapper, "pagination"); err != nil {
			return nil, fmt.Errorf("%w: relation %s pagination: %v", ErrIncompleteSnapshot, name, err)
		}
		if metaValue, exists := wrapper["meta"]; exists && metaValue != nil {
			meta, ok := metaValue.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%w: relation %s meta is malformed", ErrIncompleteSnapshot, name)
			}
			if err := requireTerminalPagination(meta, "pagination"); err != nil {
				return nil, fmt.Errorf("%w: relation %s pagination: %v", ErrIncompleteSnapshot, name, err)
			}
		}
	}
	return items, nil
}

func requireTerminalPagination(container map[string]any, key string) error {
	value, exists := container[key]
	if !exists {
		return nil
	}
	pagination, ok := value.(map[string]any)
	if !ok || !paginationComplete(pagination) {
		return errors.New("terminal page is not proven")
	}
	return nil
}

func paginationComplete(p map[string]any) bool {
	current, hasCurrent := intField(p, "current_page")
	total, hasTotal := intField(p, "total_pages")
	terminalEvidence := hasCurrent && hasTotal && current > 0 && total > 0 && current >= total
	if hasCurrent != hasTotal || hasCurrent && (current <= 0 || total <= 0 || current < total) {
		return false
	}
	if next, exists := p["next_page"]; exists {
		if paginationNextPresent(next) {
			return false
		}
		terminalEvidence = true
	}
	if linksValue, exists := p["links"]; exists {
		links, ok := linksValue.(map[string]any)
		if !ok {
			return false
		}
		next, exists := links["next"]
		if !exists || paginationNextPresent(next) {
			return false
		}
		terminalEvidence = true
	}
	return terminalEvidence
}

func paginationNextPresent(value any) bool {
	if value == nil {
		return false
	}
	return strings.TrimSpace(fmt.Sprint(value)) != ""
}

func decodeJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func unwrapObject(value any) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected fixture object")
	}
	if data, exists := object["data"]; exists {
		if nested, ok := data.(map[string]any); ok {
			return nested, nil
		}
	}
	return object, nil
}

func unwrapItems(value any) ([]map[string]any, error) {
	if wrapper, ok := value.(map[string]any); ok {
		if data, exists := wrapper["data"]; exists {
			return unwrapItems(data)
		}
		return []map[string]any{wrapper}, nil
	}
	array, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("expected array")
	}
	items := make([]map[string]any, 0, len(array))
	for _, value := range array {
		item, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("array contains a non-object")
		}
		items = append(items, item)
	}
	return items, nil
}

func normalizeFormat(raw string) (string, int, error) {
	clean := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(raw), "_", ""))
	clean = strings.ReplaceAll(clean, " ", "")
	switch clean {
	case "t20", "t20i":
		return "T20", 120, nil
	case "odi", "lista":
		return "ODI", 300, nil
	default:
		return "", 0, fmt.Errorf("%w: %s", ErrUnsupportedFormat, raw)
	}
}

func normalizeSnapshotFormat(root map[string]any, raw string) (string, int, error) {
	format, scheduledBalls, err := normalizeFormat(raw)
	if err != nil {
		return "", 0, err
	}
	expectedOvers := scheduledBalls / 6
	structuredOvers := 0
	hasStructuredOvers := false
	for _, field := range []string{"total_overs_played", "scheduled_overs", "max_overs", "number_of_overs", "innings_overs"} {
		value, exists := root[field]
		if !exists || !meaningfulValue(value) {
			continue
		}
		overs, ok := intField(root, field)
		if !ok || overs <= 0 {
			return "", 0, fmt.Errorf("%w: %s is malformed", ErrUnsupportedFormat, field)
		}
		if hasStructuredOvers && structuredOvers != overs {
			return "", 0, fmt.Errorf("%w: conflicting structured over limits", ErrUnsupportedFormat)
		}
		structuredOvers, hasStructuredOvers = overs, true
	}
	if hasStructuredOvers && structuredOvers != expectedOvers {
		return "", 0, fmt.Errorf("%w: %s scheduled for %d overs", ErrUnsupportedFormat, raw, structuredOvers)
	}
	clean := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(raw), "_", ""))
	clean = strings.ReplaceAll(clean, " ", "")
	// Standard ODI fixtures are published as 50 overs even when Sportmonks has not
	// yet populated total_overs_played (typical for NS / upcoming). List A remains
	// unsupported until a structured 50-over schedule is present.
	if clean == "lista" && !hasStructuredOvers {
		return "", 0, fmt.Errorf("%w: %s has no structured 50-over schedule", ErrUnsupportedFormat, raw)
	}
	return format, scheduledBalls, nil
}

// ClassifyFormat exposes the reducer's allowlist to fixture discovery without
// requiring a full live snapshot.
func ClassifyFormat(raw string) (string, int, error) {
	return normalizeFormat(raw)
}

// ClassifyFixtureFormat applies the stricter publication rules that may depend
// on structured fixture metadata. Discovery can still poll a nominal List A
// fixture, but it must remain unsupported publicly until 50 overs are explicit.
func ClassifyFixtureFormat(raw []byte, fallbackType string) (string, int, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return normalizeSnapshotFormat(map[string]any{}, fallbackType)
	}
	value, err := decodeJSON(raw)
	if err != nil {
		return "", 0, err
	}
	root, err := unwrapObject(value)
	if err != nil {
		return "", 0, err
	}
	fixtureType, ok := stringField(root, "type")
	if !ok || fixtureType == "" {
		fixtureType = fallbackType
	}
	return normalizeSnapshotFormat(root, fixtureType)
}

func inningsFromStatus(status string) int {
	lower := strings.ToLower(strings.TrimSpace(status))
	switch {
	case strings.Contains(lower, "1st innings"), strings.Contains(lower, "first innings"):
		return 1
	case strings.Contains(lower, "2nd innings"), strings.Contains(lower, "second innings"):
		return 2
	case strings.Contains(lower, "3rd innings"), strings.Contains(lower, "third innings"):
		return 3
	case strings.Contains(lower, "4th innings"), strings.Contains(lower, "fourth innings"):
		return 4
	default:
		return 0
	}
}

func normalizeStatus(status string) string {
	lower := strings.ToLower(strings.TrimSpace(status))
	switch {
	case lower == "ns", strings.Contains(lower, "not started"), strings.Contains(lower, "delayed"), strings.Contains(lower, "postp"):
		return matches.StatusUpcoming
	case strings.Contains(lower, "innings"), strings.Contains(lower, "lunch"), strings.Contains(lower, "tea"), strings.Contains(lower, "dinner"), strings.Contains(lower, "int."):
		if strings.Contains(lower, "break") || strings.Contains(lower, "lunch") || strings.Contains(lower, "tea") || strings.Contains(lower, "dinner") || strings.Contains(lower, "int.") {
			return matches.StatusInningsBreak
		}
		return matches.StatusLive
	case strings.Contains(lower, "finished"):
		return matches.StatusCompleted
	case strings.Contains(lower, "aban"), strings.Contains(lower, "cancl"):
		return matches.StatusAbandoned
	default:
		return matches.StatusUpcoming
	}
}

// IsNotStartedStatus is intentionally strict: unknown provider phases remain
// non-tradable but may not create a new public match.
func IsNotStartedStatus(status string) bool {
	lower := strings.ToLower(strings.TrimSpace(status))
	return lower == "ns" || strings.Contains(lower, "not started") ||
		strings.Contains(lower, "delayed") || strings.Contains(lower, "postp")
}

func inningsComplete(status string, number, current, legalBalls, scheduledBalls, wickets int) bool {
	localStatus := normalizeStatus(status)
	if localStatus == matches.StatusCompleted {
		return true
	}
	if current > number {
		return true
	}
	return legalBalls >= scheduledBalls || wickets >= 10
}

var inningsDigits = regexp.MustCompile(`(?i)(?:^|[^0-9])([1-4])(?:$|[^0-9])`)
var penaltyValue = regexp.MustCompile(`(?i)([0-9]+)\s*penalty`)

func inningsNumber(value string) int {
	if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
		return n
	}
	match := inningsDigits.FindStringSubmatch(value)
	if len(match) != 2 {
		return 0
	}
	n, _ := strconv.Atoi(match[1])
	return n
}

func ballsFromOvers(raw string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) == 1 {
		overs, err := strconv.Atoi(parts[0])
		return overs * 6, err == nil && overs >= 0
	}
	if len(parts) != 2 {
		return 0, false
	}
	overs, err1 := strconv.Atoi(parts[0])
	ballPart := parts[1]
	balls, err2 := strconv.Atoi(ballPart)
	if strings.Trim(ballPart, "0") == "" {
		balls = 0
		err2 = nil
	} else if len(ballPart) != 1 {
		return 0, false
	}
	if err1 != nil || err2 != nil || overs < 0 || balls < 0 || balls > 5 {
		return 0, false
	}
	return overs*6 + balls, true
}

func dismissalKind(name string) string {
	lower := strings.ToLower(name)
	for _, kind := range []string{"run out", "stumped", "caught", "bowled", "lbw", "hit wicket", "obstructing", "retired"} {
		if strings.Contains(lower, kind) {
			return strings.ReplaceAll(kind, " ", "_")
		}
	}
	return "wicket"
}

func bowlerGetsCredit(name string) bool {
	lower := strings.ToLower(name)
	return !strings.Contains(lower, "run out") && !strings.Contains(lower, "retired") && !strings.Contains(lower, "obstructing")
}

func deliveryHash(delivery Delivery) string {
	copy := delivery
	copy.PayloadHash = ""
	copy.ProviderUpdatedAt = nil
	encoded, _ := json.Marshal(copy)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func projectionHash(projection Projection) string {
	copy := projection
	copy.SnapshotHash = ""
	copy.ProviderUpdatedAt = nil
	for i := range copy.Deliveries {
		copy.Deliveries[i].ProviderUpdatedAt = nil
	}
	encoded, _ := json.Marshal(copy)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func inningsProjectionHash(innings Innings, deliveries []Delivery) string {
	type hashInput struct {
		Innings    Innings
		Deliveries []Delivery
	}
	copy := innings
	copy.SnapshotHash = ""
	relevant := make([]Delivery, 0)
	for _, delivery := range deliveries {
		if delivery.Innings == innings.Number {
			delivery.ProviderUpdatedAt = nil
			relevant = append(relevant, delivery)
		}
	}
	encoded, _ := json.Marshal(hashInput{Innings: copy, Deliveries: relevant})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func unionInnings(derived map[int]Innings, groups ...map[int]aggregate) []int {
	set := make(map[int]struct{})
	for number := range derived {
		set[number] = struct{}{}
	}
	for _, group := range groups {
		for number := range group {
			set[number] = struct{}{}
		}
	}
	numbers := make([]int, 0, len(set))
	for number := range set {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	return numbers
}

func firstNonZero(values ...int64) int64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func relationName(value any) string {
	items, err := unwrapItems(value)
	if err != nil || len(items) == 0 {
		return ""
	}
	name, _ := stringField(items[0], "name")
	return name
}

func extraRuns(value any, total int) int {
	switch typed := value.(type) {
	case bool:
		if typed {
			return total
		}
	case json.Number:
		n, _ := strconv.Atoi(typed.String())
		return n
	case string:
		n, _ := strconv.Atoi(typed)
		return n
	}
	return 0
}

func penaltyRuns(name string) (int, bool) {
	match := penaltyValue.FindStringSubmatch(name)
	if len(match) != 2 {
		return 0, false
	}
	runs, err := strconv.Atoi(match[1])
	return runs, err == nil && runs > 0
}

func meaningfulValue(value any) bool {
	if value == nil {
		return false
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != "" && strings.TrimSpace(typed) != "0"
	case json.Number:
		return typed.String() != "" && typed.String() != "0"
	case bool:
		return typed
	default:
		return true
	}
}

func meaningfulDLSData(value any) bool {
	if value == nil {
		return false
	}
	object, ok := value.(map[string]any)
	if !ok {
		return meaningfulValue(value)
	}
	for _, field := range []string{"score", "overs", "wickets_out", "target"} {
		if meaningfulValue(object[field]) {
			return true
		}
	}
	return false
}

func intField(object map[string]any, key string) (int, bool) {
	number, ok := numberField(object, key)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(number.String())
	return n, err == nil
}

func int64Field(object map[string]any, key string) (int64, bool) {
	number, ok := numberField(object, key)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(number.String(), 10, 64)
	return n, err == nil
}

func numberField(object map[string]any, key string) (json.Number, bool) {
	value, exists := object[key]
	if !exists || value == nil {
		return "", false
	}
	switch typed := value.(type) {
	case json.Number:
		return typed, true
	case string:
		return json.Number(strings.TrimSpace(typed)), true
	case float64:
		return json.Number(strconv.FormatFloat(typed, 'f', -1, 64)), true
	default:
		return "", false
	}
}

func stringField(object map[string]any, key string) (string, bool) {
	value, exists := object[key]
	if !exists || value == nil {
		return "", false
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed), true
	case json.Number:
		return typed.String(), true
	default:
		return "", false
	}
}

func boolField(object map[string]any, key string) (bool, bool) {
	value, exists := object[key]
	if !exists || value == nil {
		return false, false
	}
	switch typed := value.(type) {
	case bool:
		return typed, true
	case json.Number:
		return typed.String() == "1", true
	case string:
		value, err := strconv.ParseBool(strings.TrimSpace(typed))
		return value, err == nil
	default:
		return false, false
	}
}

func timeField(object map[string]any, key string) (time.Time, bool) {
	raw, ok := stringField(object, key)
	if !ok || raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}
