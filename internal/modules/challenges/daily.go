package challenges

import (
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
)

const AcademyToday = "today"

const (
	DailyPowerplayPro      = "powerplay-pro"
	DailyMiddleOverGenius  = "middle-over-genius"
	DailyDeathOverAssassin = "death-over-assassin"
	DailyLastOverHero      = "last-over-hero"
)

var dailyDefinitions = []definition{
	{
		ID: DailyPowerplayPro, AcademyID: AcademyToday,
		Title:       "Powerplay Pro",
		Description: "Complete 3 profitable trades during 1–6 overs in T20, or 1–10 overs in ODI.",
		Target:      3, Reward: 750,
	},
	{
		ID: DailyMiddleOverGenius, AcademyID: AcademyToday,
		Title:       "Middle Over Genius",
		Description: "Complete 3 profitable trades during 7–15 overs in T20, or 11–40 overs in ODI.",
		Target:      3, Reward: 1000,
	},
	{
		ID: DailyDeathOverAssassin, AcademyID: AcademyToday,
		Title:       "Death Over Assassin",
		Description: "Finish 3 profitable trades after the 16th over in T20, or after the 40th over in ODI.",
		Target:      3, Reward: 1250,
	},
	{
		ID: DailyLastOverHero, AcademyID: AcademyToday,
		Title:       "Last Over Hero",
		Description: "Open and close a position profitably in the final over.",
		Target:      1, Reward: 1500,
	},
}

var dailyByID = func() map[string]definition {
	out := make(map[string]definition, len(dailyDefinitions))
	for _, d := range dailyDefinitions {
		out[d.ID] = d
	}
	return out
}()

func isDailyChallenge(id string) bool {
	_, ok := dailyByID[id]
	return ok
}

func lookupDefinition(id string) (definition, bool) {
	if d, ok := definitionByID[id]; ok {
		return d, true
	}
	if d, ok := dailyByID[id]; ok {
		return d, true
	}
	return definition{}, false
}

func emptyDailyChallenges() []Challenge {
	out := make([]Challenge, 0, len(dailyDefinitions))
	for _, d := range dailyDefinitions {
		out = append(out, finalizeDaily(Challenge{
			ID: d.ID, AcademyID: d.AcademyID, Title: d.Title,
			Description: d.Description, Target: d.Target, Reward: d.Reward,
		}))
	}
	return out
}

func finalizeDaily(c Challenge) Challenge {
	if c.Progress >= c.Target && c.Target > 0 {
		c.Progress = c.Target
		c.Status = StatusComplete
		return c
	}
	if c.Progress < 0 {
		c.Progress = 0
	}
	c.Status = StatusInProgress
	return c
}

func matchingDailyIDs(open, close executions.MatchClock, realizedPnL float64) []string {
	if realizedPnL <= 0 {
		return nil
	}
	format := executions.NormalizeChallengeFormat(close.Format)
	if format == "" {
		return nil
	}
	closeBalls := executions.EffectiveLegalBalls(close)
	var ids []string
	if inPowerplay(format, closeBalls) {
		ids = append(ids, DailyPowerplayPro)
	}
	if inMiddle(format, closeBalls) {
		ids = append(ids, DailyMiddleOverGenius)
	}
	if inDeath(format, closeBalls) {
		ids = append(ids, DailyDeathOverAssassin)
	}
	if lastOverHero(open, close, format) {
		ids = append(ids, DailyLastOverHero)
	}
	return ids
}

func inPowerplay(format string, balls int) bool {
	switch format {
	case "T20":
		return balls >= 1 && balls <= 36
	case "ODI":
		return balls >= 1 && balls <= 60
	default:
		return false
	}
}

func inMiddle(format string, balls int) bool {
	switch format {
	case "T20":
		return balls >= 37 && balls <= 90
	case "ODI":
		return balls >= 61 && balls <= 240
	default:
		return false
	}
}

func inDeath(format string, balls int) bool {
	switch format {
	case "T20":
		return balls >= 91
	case "ODI":
		return balls >= 241
	default:
		return false
	}
}

func inLastOver(format string, balls int) bool {
	switch format {
	case "T20":
		return balls >= 115 && balls <= 120
	case "ODI":
		return balls >= 295 && balls <= 300
	default:
		return false
	}
}

func lastOverHero(open, close executions.MatchClock, closeFormat string) bool {
	openFormat := executions.NormalizeChallengeFormat(open.Format)
	if openFormat == "" {
		openFormat = closeFormat
	}
	if openFormat != closeFormat {
		return false
	}
	openMatch := open.MatchID
	closeMatch := close.MatchID
	if openMatch == "" || closeMatch == "" || openMatch != closeMatch {
		return false
	}
	if open.Innings != close.Innings {
		return false
	}
	return inLastOver(closeFormat, executions.EffectiveLegalBalls(open)) &&
		inLastOver(closeFormat, executions.EffectiveLegalBalls(close))
}
