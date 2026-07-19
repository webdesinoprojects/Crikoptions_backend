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
	CurrentInnings int
	BattingTeamID  int64
	LocalTeamID    int64
	VisitorTeamID  int64
	LocalTeamName  string
	VisitorTeamName string
	CurrentScore   int
	Wickets        int
	LegalBalls     int
	ScheduledBalls int
	Target         int
	Deliveries     []Delivery
}

// BuildLiveContext maps Sportmonks batting/bowling scoreboards to the on-field matrix.
func BuildLiveContext(battingItems, bowlingItems []map[string]any, input LiveContextInput) *matches.LiveMatchContext {
	if input.CurrentInnings <= 0 {
		return nil
	}
	scoreboard := fmt.Sprintf("S%d", input.CurrentInnings)

	striker, nonStriker, partnership := battingPair(battingItems, scoreboard)
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

func battingPair(items []map[string]any, scoreboard string) (matches.BatterStats, matches.BatterStats, matches.PartnershipStats) {
	var atCrease []matches.BatterStats
	var partnership matches.PartnershipStats
	for _, item := range items {
		if !scoreboardMatches(item, scoreboard) {
			continue
		}
		if !batsmanAtCrease(item) {
			continue
		}
		runs := statInt(item, "score", "runs")
		balls := statInt(item, "ball", "balls")
		atCrease = append(atCrease, matches.BatterStats{
			Name:  playerName(item, "batsman", "player"),
			Runs:  runs,
			Balls: balls,
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
	if len(atCrease) == 0 {
		return matches.BatterStats{}, matches.BatterStats{}, partnership
	}

	var striker, nonStriker matches.BatterStats
	for _, batter := range atCrease {
		if item := itemForBatter(items, scoreboard, batter.Name); item != nil && isActivePlayer(item, "active") {
			striker = batter
			break
		}
	}
	if striker.Name == "" {
		striker = atCrease[0]
		for _, batter := range atCrease[1:] {
			if batter.Runs > striker.Runs || (batter.Runs == striker.Runs && batter.Balls > striker.Balls) {
				striker = batter
			}
		}
	}
	for _, batter := range atCrease {
		if batter.Name != striker.Name {
			if nonStriker.Name == "" || batter.Balls > nonStriker.Balls {
				nonStriker = batter
			}
		}
	}
	return striker, nonStriker, partnership
}

func itemForBatter(items []map[string]any, scoreboard, name string) map[string]any {
	for _, item := range items {
		if !scoreboardMatches(item, scoreboard) {
			continue
		}
		if playerName(item, "batsman", "player") == name {
			return item
		}
	}
	return nil
}

func activeBowler(items []map[string]any, scoreboard string, deliveries []Delivery, innings int) matches.BowlerStats {
	var selected matches.BowlerStats
	found := false
	for _, item := range items {
		if !scoreboardMatches(item, scoreboard) {
			continue
		}
		name := playerName(item, "bowler", "player")
		if name == "" {
			continue
		}
		runs := statInt(item, "runs")
		wickets := statInt(item, "wickets")
		maidens := statInt(item, "medians", "maidens")
		balls := 0
		if overs, ok := stringField(item, "overs"); ok {
			if parsed, valid := ballsFromOvers(overs); valid {
				balls = parsed
			}
		}
		candidate := matches.BowlerStats{
			Name:    name,
			Balls:   balls,
			Maidens: maidens,
			Runs:    runs,
			Wickets: wickets,
		}
		if isActivePlayer(item, "active") || !found {
			selected = candidate
			found = true
			if isActivePlayer(item, "active") {
				break
			}
		}
	}
	if selected.Name != "" {
		selected.CurrentOverRuns = bowlerOverRuns(deliveries, innings, selected.Name)
	}
	return selected
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

func batsmanAtCrease(item map[string]any) bool {
	for _, key := range []string{"wicket_type", "how_out", "dismissal"} {
		if value, ok := stringField(item, key); ok {
			lower := strings.ToLower(strings.TrimSpace(value))
			if lower == "" || strings.Contains(lower, "not out") {
				return true
			}
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
