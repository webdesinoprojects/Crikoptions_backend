package reconcile

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

// LiveContextInput carries provider scoreboard rows for the active innings.
type LiveContextInput struct {
	CurrentInnings  int
	BattingTeamID   int64
	LocalTeamID     int64
	VisitorTeamID   int64
	LocalTeamName   string
	VisitorTeamName string
	CurrentScore    int
	Wickets         int
	LegalBalls      int
	ScheduledBalls  int
	Target          int
	Deliveries      []Delivery
}

// BuildLiveContext maps Sportmonks batting/bowling scoreboards to the on-field matrix.
func BuildLiveContext(battingItems, bowlingItems []map[string]any, input LiveContextInput) *matches.LiveMatchContext {
	if input.CurrentInnings <= 0 {
		return nil
	}
	scoreboard := fmt.Sprintf("S%d", input.CurrentInnings)

	striker, nonStriker, partnership := battingPair(battingItems, scoreboard, input.Deliveries, input.CurrentInnings)
	bowler := activeBowler(bowlingItems, scoreboard, input.Deliveries, input.CurrentInnings)
	// Partial on-field data is still useful for the UI. Only skip when nothing
	// resolved — trading health no longer depends on a full matrix.
	if striker.Name == "" && nonStriker.Name == "" && bowler.Name == "" {
		return nil
	}
	if partnership.Runs == 0 && partnership.Balls == 0 {
		partnership = matches.PartnershipStats{
			Runs:  striker.Runs + nonStriker.Runs,
			Balls: striker.Balls + nonStriker.Balls,
		}
	}
	return &matches.LiveMatchContext{
		Striker:     striker,
		NonStriker:  nonStriker,
		Bowler:      bowler,
		Partnership: partnership,
	}
}

// BuildMatchPulse derives momentum / volatility / pressure labels from recent deliveries.
func BuildMatchPulse(input LiveContextInput) *matches.MatchPulse {
	if input.CurrentInnings <= 0 || len(input.Deliveries) == 0 {
		return nil
	}
	battingName := input.LocalTeamName
	bowlingName := input.VisitorTeamName
	if input.BattingTeamID == input.VisitorTeamID {
		battingName = input.VisitorTeamName
		bowlingName = input.LocalTeamName
	}
	if battingName == "" {
		battingName = "Batting side"
	}
	if bowlingName == "" {
		bowlingName = "Bowling side"
	}

	lastWicket := "No wicket this over"
	currentOver := currentOverIndex(input.LegalBalls)
	for _, d := range reverseInningsDeliveries(input.Deliveries, input.CurrentInnings) {
		if d.Innings != input.CurrentInnings {
			continue
		}
		over, _ := displayBallIndex(d.ProviderBall)
		if over == currentOver && d.Dismissal != nil {
			lastWicket = "Wicket this over"
			break
		}
		if d.Dismissal != nil {
			break
		}
	}

	recent := recentLegalDeliveries(input.Deliveries, input.CurrentInnings, 6)
	recentRuns := 0
	boundaryCount := 0
	for _, d := range recent {
		recentRuns += d.TeamRuns
		if d.BatterRuns >= 4 {
			boundaryCount++
		}
	}

	momentum := "Even phase"
	momentumLevel := "even"
	switch {
	case recentRuns >= 12:
		momentum = battingName + " attacking"
		momentumLevel = "attacking"
	case recentRuns <= 3 && len(recent) >= 4:
		momentum = bowlingName + " control"
		momentumLevel = "defensive"
	}

	volatility := "Stable"
	volatilityLevel := "stable"
	switch {
	case boundaryCount >= 2 || recentRuns >= 15:
		volatility = "High"
		volatilityLevel = "high"
	case boundaryCount == 1 || recentRuns >= 8:
		volatility = "Moderate"
		volatilityLevel = "moderate"
	}

	pressure := "Balanced phase"
	pressureLevel := "balanced"
	if input.CurrentInnings == 2 && input.Target > 0 {
		ballsBowled := input.LegalBalls
		ballsLeft := max(0, input.ScheduledBalls-ballsBowled)
		runsNeeded := max(0, input.Target-input.CurrentScore)
		if ballsLeft > 0 && runsNeeded > 0 {
			requiredRR := float64(runsNeeded) / float64(ballsLeft) * 6
			currentRR := 0.0
			if ballsBowled > 0 {
				currentRR = float64(input.CurrentScore) / float64(ballsBowled) * 6
			}
			if requiredRR > currentRR+1.5 {
				pressure = "On " + battingName
				pressureLevel = "chase"
			} else if currentRR > requiredRR+1.5 {
				pressure = "On " + bowlingName
				pressureLevel = "defend"
			}
		} else if runsNeeded == 0 {
			pressure = "Target reached"
			pressureLevel = "complete"
		}
	}

	return &matches.MatchPulse{
		LastWicket:       lastWicket,
		Momentum:         momentum,
		MomentumLevel:    momentumLevel,
		MarketVolatility: volatility,
		VolatilityLevel:  volatilityLevel,
		Pressure:         pressure,
		PressureLevel:    pressureLevel,
	}
}

// BuildThisOver returns ball slots for the active over from provider deliveries.
func BuildThisOver(deliveries []Delivery, innings, legalBalls int) []matches.OverBall {
	if innings <= 0 || legalBalls <= 0 {
		return nil
	}
	currentOver := currentOverIndex(legalBalls)
	out := make([]matches.OverBall, 0, 6)
	for _, d := range deliveries {
		if d.Innings != innings {
			continue
		}
		over, _ := displayBallIndex(d.ProviderBall)
		if over != currentOver {
			continue
		}
		extra := ""
		if d.Extras.Wides > 0 {
			extra = matches.ExtraWide
		} else if d.Extras.NoBalls > 0 {
			extra = matches.ExtraNoBall
		} else if d.Extras.Byes > 0 {
			extra = matches.ExtraBye
		} else if d.Extras.LegByes > 0 {
			extra = matches.ExtraLegBye
		}
		out = append(out, matches.OverBall{
			Runs:      d.TeamRuns,
			IsWicket:  d.Dismissal != nil,
			LegalBall: d.LegalBall,
			Extra:     extra,
		})
	}
	return out
}

// creaseBatter pairs a batter's display stats with the identity needed to match
// them against the ball-by-ball feed.
type creaseBatter struct {
	stats  matches.BatterStats
	id     int64
	active bool
	notOut bool
}

// battingPair resolves the two batters currently at the crease and which of them
// is on strike.
//
// Strike order is taken from the ball-by-ball feed rather than the scoreboard:
// the last delivery records who faced it, and standard rotation (odd runs run,
// end of over) gives who is on strike now. That holds regardless of how the
// provider populates active, and unlike the previous implementation it never
// depends on the order rows happen to arrive in.
func battingPair(items []map[string]any, scoreboard string, deliveries []Delivery, innings int) (matches.BatterStats, matches.BatterStats, matches.PartnershipStats) {
	var partnership matches.PartnershipStats
	var candidates []creaseBatter

	for _, item := range items {
		if !scoreboardMatches(item, scoreboard) {
			continue
		}
		active, hasActive := boolField(item, "active")
		candidates = append(candidates, creaseBatter{
			stats: matches.BatterStats{
				Name:  playerName(item, "batsman", "player"),
				Runs:  statInt(item, "score", "runs"),
				Balls: statInt(item, "ball", "balls"),
			},
			id:     batterIdentity(item),
			active: active && hasActive,
			notOut: batsmanAtCrease(item),
		})
		if pRuns := statInt(item, "partnership_runs"); pRuns > 0 {
			partnership.Runs = pRuns
		}
		if pBalls := statInt(item, "partnership_balls"); pBalls > 0 {
			partnership.Balls = pBalls
		}
		if nested, ok := item["partnership"].(map[string]any); ok {
			if pRuns := statInt(nested, "runs", "score"); pRuns > 0 {
				partnership.Runs = pRuns
			}
			if pBalls := statInt(nested, "balls", "ball"); pBalls > 0 {
				partnership.Balls = pBalls
			}
		}
	}

	atCrease := selectAtCrease(candidates)
	if len(atCrease) == 0 {
		return matches.BatterStats{}, matches.BatterStats{}, partnership
	}
	if len(atCrease) == 1 {
		return atCrease[0].stats, matches.BatterStats{}, partnership
	}

	strikerIdx := strikerIndex(atCrease, deliveries, innings)
	striker := atCrease[strikerIdx].stats
	nonStriker := atCrease[1-strikerIdx].stats
	return striker, nonStriker, partnership
}

// selectAtCrease narrows the innings scoreboard to the two batters out in the
// middle.
//
// Providers disagree on what active means: some flag both crease batters, others
// flag only the one on strike. Rather than betting on either reading, we treat
// active rows as definitely at the crease and top up from the not-out rows when
// only one is flagged. Dismissed batters are excluded in both paths — letting
// them through was why a long-departed top scorer could appear on the card.
func selectAtCrease(candidates []creaseBatter) []creaseBatter {
	var active, notOut []creaseBatter
	for _, candidate := range candidates {
		if candidate.stats.Name == "" {
			continue
		}
		if candidate.active {
			active = append(active, candidate)
			continue
		}
		if candidate.notOut {
			notOut = append(notOut, candidate)
		}
	}
	switch {
	case len(active) >= 2:
		// Both crease batters flagged: take the most recent pair.
		return active[len(active)-2:]
	case len(active) == 1:
		// Only the striker is flagged; partner is the latest not-out batter.
		// active stays first so it wins the strike tie-break below.
		out := active
		if len(notOut) > 0 {
			out = append(out, notOut[len(notOut)-1])
		}
		return out
	default:
		if len(notOut) > 2 {
			notOut = notOut[len(notOut)-2:]
		}
		return notOut
	}
}

// strikerIndex returns which of the two at-crease batters is on strike, derived
// from the last delivery of the innings. Falls back to index 0 when the feed
// cannot resolve it (for example immediately after a wicket, before the new
// batter has faced a ball).
func strikerIndex(pair []creaseBatter, deliveries []Delivery, innings int) int {
	last, ok := lastDelivery(deliveries, innings)
	if !ok || last.BatterID <= 0 {
		return 0
	}
	faced := -1
	for i, batter := range pair {
		if batter.id > 0 && batter.id == last.BatterID {
			faced = i
			break
		}
	}
	if faced < 0 {
		// Whoever faced the last ball is no longer at the crease — they were just
		// dismissed. Which end the replacement takes depends on whether the batters
		// had crossed, which the feed does not tell us, so keep the provider's own
		// ordering (active row first) until the next ball resolves it.
		return 0
	}
	if strikeRotates(last, deliveries, innings) {
		return 1 - faced
	}
	return faced
}

// strikeRotates reports whether the batters changed ends after this delivery:
// an odd number of runs physically run, and/or the completion of an over.
func strikeRotates(last Delivery, deliveries []Delivery, innings int) bool {
	run := last.BatterRuns + last.Extras.Byes + last.Extras.LegByes
	if last.Extras.Wides > 0 {
		// The one-run wide penalty is not run between the wickets; anything beyond
		// it is. No-ball penalties work the same way and are already excluded from
		// BatterRuns by the reducer.
		run += last.Extras.Wides - 1
	}
	rotate := run%2 == 1
	if last.LegalBall && legalBallsInInnings(deliveries, innings)%6 == 0 {
		rotate = !rotate
	}
	return rotate
}

func lastDelivery(deliveries []Delivery, innings int) (Delivery, bool) {
	for i := len(deliveries) - 1; i >= 0; i-- {
		if deliveries[i].Innings == innings {
			return deliveries[i], true
		}
	}
	return Delivery{}, false
}

func legalBallsInInnings(deliveries []Delivery, innings int) int {
	count := 0
	for _, d := range deliveries {
		if d.Innings == innings && d.LegalBall {
			count++
		}
	}
	return count
}

// batterIdentity extracts the provider player id from a batting scoreboard row
// so it can be matched against a delivery's batsman_id.
func batterIdentity(item map[string]any) int64 {
	return playerIdentity(item,
		[]string{"batsman_id", "player_id", "batsmanId", "playerId"},
		[]string{"batsman", "player"},
	)
}

// playerIdentity reads a provider player id from a scoreboard row, checking flat
// id fields first and then the embedded player relation.
func playerIdentity(item map[string]any, idKeys, relations []string) int64 {
	for _, key := range idKeys {
		if id, ok := int64Field(item, key); ok && id > 0 {
			return id
		}
	}
	for _, key := range relations {
		nested, exists := item[key]
		if !exists || nested == nil {
			continue
		}
		if items, err := unwrapItems(nested); err == nil && len(items) > 0 {
			if id, ok := int64Field(items[0], "id"); ok && id > 0 {
				return id
			}
		}
		if object, ok := nested.(map[string]any); ok {
			if id, ok := int64Field(object, "id"); ok && id > 0 {
				return id
			}
		}
	}
	return 0
}

// activeBowler resolves who is currently bowling. As with the batting pair, the
// last delivery's bowler_id is authoritative; the active flag is the fallback.
// Array position is deliberately not trusted — the previous behaviour of taking
// the first row pinned the display to the opening bowler for the whole innings.
func activeBowler(items []map[string]any, scoreboard string, deliveries []Delivery, innings int) matches.BowlerStats {
	type bowlerCandidate struct {
		stats  matches.BowlerStats
		id     int64
		active bool
	}
	var candidates []bowlerCandidate
	for _, item := range items {
		if !scoreboardMatches(item, scoreboard) {
			continue
		}
		name := playerName(item, "bowler", "player")
		if name == "" {
			continue
		}
		balls := 0
		if overs, ok := stringField(item, "overs"); ok {
			if parsed, valid := ballsFromOvers(overs); valid {
				balls = parsed
			}
		}
		active, hasActive := boolField(item, "active")
		candidates = append(candidates, bowlerCandidate{
			stats: matches.BowlerStats{
				Name:    name,
				Balls:   balls,
				Maidens: statInt(item, "medians", "maidens"),
				Runs:    statInt(item, "runs"),
				Wickets: statInt(item, "wickets"),
			},
			id:     playerIdentity(item, []string{"bowler_id", "player_id", "bowlerId", "playerId"}, []string{"bowler", "player"}),
			active: active && hasActive,
		})
	}
	if len(candidates) == 0 {
		return matches.BowlerStats{}
	}

	selected := -1
	if last, ok := lastDelivery(deliveries, innings); ok && last.BowlerID > 0 {
		for i, candidate := range candidates {
			if candidate.id > 0 && candidate.id == last.BowlerID {
				selected = i
				break
			}
		}
	}
	if selected < 0 {
		for i, candidate := range candidates {
			if candidate.active {
				selected = i
				break
			}
		}
	}
	if selected < 0 {
		// Most recently added row is the closest thing to "current" we have left.
		selected = len(candidates) - 1
	}

	stats := candidates[selected].stats
	stats.CurrentOverRuns = bowlerOverRuns(deliveries, innings, stats.Name)
	return stats
}

func bowlerOverRuns(deliveries []Delivery, innings int, bowlerName string) int {
	if bowlerName == "" {
		return 0
	}
	legalCount := 0
	for _, d := range deliveries {
		if d.Innings == innings && d.LegalBall {
			legalCount++
		}
	}
	if legalCount == 0 {
		return 0
	}
	currentOver := currentOverIndex(legalCount)
	total := 0
	for _, d := range deliveries {
		if d.Innings != innings {
			continue
		}
		over, _ := displayBallIndex(d.ProviderBall)
		if over == currentOver {
			total += d.TeamRuns
		}
	}
	return total
}

// batsmanAtCrease reports whether a batting row belongs to a batter who has not
// been dismissed.
//
// Sportmonks does not send a how-out string on the batting resource, so the
// original text-only check passed every row through and dismissed batters stayed
// eligible for the on-field card. The structured dismissal columns
// (catch_stump_player_id, runout_by_id) and the fall-of-wicket pair are the
// signals that actually appear.
func batsmanAtCrease(item map[string]any) bool {
	for _, key := range []string{"wicket_type", "how_out", "dismissal"} {
		if value, ok := stringField(item, key); ok {
			lower := strings.ToLower(strings.TrimSpace(value))
			if lower == "" || strings.Contains(lower, "not out") {
				continue
			}
			return false
		}
	}
	for _, key := range []string{"catch_stump_player_id", "runout_by_id", "catchstump_id"} {
		if id, ok := int64Field(item, key); ok && id > 0 {
			return false
		}
	}
	// A recorded fall of wicket means this batter's wicket fell.
	for _, key := range []string{"fow_score", "fow_balls"} {
		if value, ok := int64Field(item, key); ok && value > 0 {
			return false
		}
	}
	return true
}

func isActivePlayer(item map[string]any, key string) bool {
	if item == nil {
		return false
	}
	active, ok := boolField(item, key)
	return ok && active
}

func scoreboardMatches(item map[string]any, scoreboard string) bool {
	value, ok := stringField(item, "scoreboard")
	if !ok {
		if inning, ok := intField(item, "inning"); ok {
			return fmt.Sprintf("S%d", inning) == scoreboard
		}
		return false
	}
	return strings.EqualFold(strings.TrimSpace(value), scoreboard)
}

func playerName(item map[string]any, keys ...string) string {
	for _, key := range keys {
		nested, exists := item[key]
		if !exists || nested == nil {
			continue
		}
		if items, err := unwrapItems(nested); err == nil && len(items) > 0 {
			if name := formattedPlayerName(items[0]); name != "" {
				return name
			}
		}
		if object, ok := nested.(map[string]any); ok {
			if name := formattedPlayerName(object); name != "" {
				return name
			}
		}
	}
	if id, ok := int64Field(item, "batsman_id"); ok && id > 0 {
		return fmt.Sprintf("Player %d", id)
	}
	if id, ok := int64Field(item, "player_id"); ok && id > 0 {
		return fmt.Sprintf("Player %d", id)
	}
	return ""
}

func formattedPlayerName(item map[string]any) string {
	if name, ok := stringField(item, "fullname"); ok && name != "" {
		return name
	}
	if name, ok := stringField(item, "full_name"); ok && name != "" {
		return name
	}
	first, _ := stringField(item, "firstname")
	last, _ := stringField(item, "lastname")
	name := strings.TrimSpace(strings.TrimSpace(first) + " " + strings.TrimSpace(last))
	if name != "" {
		return name
	}
	if name, ok := stringField(item, "name"); ok {
		return name
	}
	return ""
}

func currentOverIndex(legalBalls int) int {
	if legalBalls <= 0 {
		return 0
	}
	return int(math.Ceil(float64(legalBalls)/6)) - 1
}

func displayBallIndex(providerBall string) (int, int) {
	return displayBall(providerBall)
}

func displayBall(value string) (int, int) {
	parts := strings.SplitN(strings.TrimSpace(value), ".", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	over, _ := strconv.Atoi(parts[0])
	ball, _ := strconv.Atoi(parts[1])
	return over, ball
}

func recentLegalDeliveries(deliveries []Delivery, innings, limit int) []Delivery {
	out := make([]Delivery, 0, limit)
	for i := len(deliveries) - 1; i >= 0 && len(out) < limit; i-- {
		if deliveries[i].Innings != innings || !deliveries[i].LegalBall {
			continue
		}
		out = append(out, deliveries[i])
	}
	return out
}

func reverseInningsDeliveries(deliveries []Delivery, innings int) []Delivery {
	out := make([]Delivery, 0)
	for i := len(deliveries) - 1; i >= 0; i-- {
		if deliveries[i].Innings == innings {
			out = append(out, deliveries[i])
		}
	}
	return out
}

func statInt(item map[string]any, keys ...string) int {
	for _, key := range keys {
		if n, ok := intField(item, key); ok {
			return n
		}
		value, exists := item[key]
		if !exists || value == nil {
			continue
		}
		switch typed := value.(type) {
		case int:
			return typed
		case int64:
			return int(typed)
		case int32:
			return int(typed)
		}
	}
	return 0
}
