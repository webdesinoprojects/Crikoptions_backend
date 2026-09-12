package reconcile

import (
	"math"
	"strings"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

// LiveContextInput carries the match-level facts the pulse needs that the
// miniscore does not name directly (which side is batting, the scheduled ball
// count for the format).
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
	Status          string
}

// BuildLiveContext maps the CricLive miniscore straight onto the on-field
// context. CricLive names both batters at the crease and both bowlers, so no
// inference from the ball-by-ball feed is required.
func BuildLiveContext(mini client.MiniScore) *matches.LiveMatchContext {
	striker := batterStats(mini.Striker)
	nonStriker := batterStats(mini.NonStriker)
	bowler := bowlerStats(mini.BowlerStriker)
	if striker.Name == "" && nonStriker.Name == "" && bowler.Name == "" {
		return nil
	}
	return &matches.LiveMatchContext{
		Striker:    striker,
		NonStriker: nonStriker,
		Bowler:     bowler,
		Partnership: matches.PartnershipStats{
			Runs:  mini.Partnership.Runs.Int(),
			Balls: mini.Partnership.Balls.Int(),
		},
	}
}

func batterStats(line client.BattingLine) matches.BatterStats {
	return matches.BatterStats{
		Name:  strings.TrimSpace(line.Name),
		Runs:  line.Runs.Int(),
		Balls: line.Balls.Int(),
	}
}

func bowlerStats(line client.BowlingLine) matches.BowlerStats {
	return matches.BowlerStats{
		Name:    strings.TrimSpace(line.Name),
		Balls:   OversToBalls(line.Overs.Float64()),
		Maidens: line.Maidens.Int(),
		Runs:    line.Runs.Int(),
		Wickets: line.Wickets.Int(),
	}
}

// BuildMatchPulse derives the on-field matrix analytics. The wicket line comes
// from CricLive verbatim because it already reads as a scorecard entry
// ("Emilio Gay b Razaullah 60(114) - 143/4 in 33.4 ov.").
func BuildMatchPulse(mini client.MiniScore, input LiveContextInput) *matches.MatchPulse {
	if input.CurrentInnings <= 0 {
		return nil
	}
	battingName := input.LocalTeamName
	bowlingName := input.VisitorTeamName
	if input.BattingTeamID == input.VisitorTeamID {
		battingName = input.VisitorTeamName
		bowlingName = input.LocalTeamName
	}
	if strings.TrimSpace(battingName) == "" {
		battingName = "Batting side"
	}
	if strings.TrimSpace(bowlingName) == "" {
		bowlingName = "Bowling side"
	}

	lastWicket := strings.TrimSpace(mini.LastWicket)
	if lastWicket == "" {
		lastWicket = "No wicket this over"
	}

	recentRuns, boundaryCount := recentOverShape(mini.RecentOvers)

	momentum := "Even phase"
	momentumLevel := "even"
	switch {
	case recentRuns >= 12:
		momentum = battingName + " attacking"
		momentumLevel = "attacking"
	case recentRuns <= 3 && recentRuns >= 0 && boundaryCount == 0:
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
		ballsLeft := input.ScheduledBalls - input.LegalBalls
		if ballsLeft < 0 {
			ballsLeft = 0
		}
		runsNeeded := input.Target - input.CurrentScore
		if runsNeeded < 0 {
			runsNeeded = 0
		}
		switch {
		case runsNeeded == 0:
			pressure = "Target reached"
			pressureLevel = "complete"
		case ballsLeft > 0:
			requiredRR := mini.RequiredRate.Float64()
			if requiredRR <= 0 {
				requiredRR = float64(runsNeeded) / float64(ballsLeft) * 6
			}
			currentRR := mini.CurrentRunRate.Float64()
			if currentRR <= 0 && input.LegalBalls > 0 {
				currentRR = float64(input.CurrentScore) / float64(input.LegalBalls) * 6
			}
			if requiredRR > currentRR+1.5 {
				pressure = "On " + battingName
				pressureLevel = "chase"
			} else if currentRR > requiredRR+1.5 {
				pressure = "On " + bowlingName
				pressureLevel = "defend"
			}
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

// recentOverShape sums the most recent over from the miniscore's recent_overs
// string ("...  | 0 3 1 0 0 0  | 0 0 2 0 0 0"), whose last segment is the over
// in progress.
func recentOverShape(recentOvers string) (runs int, boundaries int) {
	segments := strings.Split(recentOvers, "|")
	for i := len(segments) - 1; i >= 0; i-- {
		segment := strings.TrimSpace(segments[i])
		if segment == "" || strings.Contains(segment, "...") {
			continue
		}
		for _, token := range strings.Fields(segment) {
			outcome, err := ParseBallToken(token)
			if err != nil {
				continue
			}
			runs += outcome.TotalRuns
			if outcome.BatterRuns >= 4 {
				boundaries++
			}
		}
		return runs, boundaries
	}
	return 0, 0
}

// BuildThisOver returns the ball strip for the over in progress. It reads the
// reduced deliveries rather than the raw over payload so the progressive
// ball-by-ball replay and the final snapshot render the strip identically.
func BuildThisOver(deliveries []Delivery, innings, legalBalls int) []matches.OverBall {
	if innings <= 0 || legalBalls <= 0 {
		return nil
	}
	currentOver := currentOverIndex(legalBalls)
	out := make([]matches.OverBall, 0, 6)
	for _, delivery := range deliveries {
		if delivery.Innings != innings {
			continue
		}
		over, _ := displayBall(delivery.ProviderBall)
		if over != currentOver {
			continue
		}
		extra := ""
		switch {
		case delivery.Extras.Wides > 0:
			extra = matches.ExtraWide
		case delivery.Extras.NoBalls > 0:
			extra = matches.ExtraNoBall
		case delivery.Extras.Byes > 0:
			extra = matches.ExtraBye
		case delivery.Extras.LegByes > 0:
			extra = matches.ExtraLegBye
		case delivery.Extras.Penalties > 0:
			extra = matches.ExtraPenalty
		}
		out = append(out, matches.OverBall{
			Runs:      delivery.TeamRuns,
			IsWicket:  delivery.Dismissal != nil,
			LegalBall: delivery.LegalBall,
			Extra:     extra,
		})
	}
	return out
}

// currentOverIndex is the zero-based index of the over on display: the one in
// progress, or the one just completed when the over has ended.
func currentOverIndex(legalBalls int) int {
	if legalBalls <= 0 {
		return 0
	}
	return int(math.Ceil(float64(legalBalls)/6)) - 1
}
