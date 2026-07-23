package challenges

import (
	"math"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/positions"
)

func EvaluatePositions(pos []positions.Position) []Challenge {
	var challenges []Challenge

	// 1. The First Step
	c1 := Challenge{ID: "first_step", Title: "The First Step", Description: "Execute your very first trade", Target: 1, XP: 100}
	c1.Progress = len(pos)
	challenges = append(challenges, finalize(c1))

	// 2. Diversification
	c2 := Challenge{ID: "diversification", Title: "Diversification", Description: "Place trades in at least 3 different matches", Target: 3, XP: 200}
	matches := make(map[string]bool)
	for _, p := range pos {
		matches[p.MatchID] = true
	}
	c2.Progress = len(matches)
	challenges = append(challenges, finalize(c2))

	// 3. Volume Trader
	c3 := Challenge{ID: "volume_trader", Title: "Volume Trader", Description: "Trade 10 different contracts", Target: 10, XP: 300}
	c3.Progress = len(pos)
	challenges = append(challenges, finalize(c3))

	// 4. First Blood
	c4 := Challenge{ID: "first_blood", Title: "First Blood", Description: "Close your first profitable trade", Target: 1, XP: 150}
	for _, p := range pos {
		if p.RealizedPnL > 0 {
			c4.Progress = 1
			break
		}
	}
	challenges = append(challenges, finalize(c4))

	// 5. The Hat-Trick
	c5 := Challenge{ID: "hat_trick", Title: "The Hat-Trick", Description: "Close 3 profitable trades", Target: 3, XP: 250}
	profCount := 0
	for _, p := range pos {
		if p.RealizedPnL > 0 {
			profCount++
		}
	}
	c5.Progress = profCount
	challenges = append(challenges, finalize(c5))

	// 6. Hot Hand
	c6 := Challenge{ID: "hot_hand", Title: "Hot Hand", Description: "Close 5 profitable trades", Target: 5, XP: 400}
	c6.Progress = profCount
	challenges = append(challenges, finalize(c6))

	// 7. Matchday Hero
	c7 := Challenge{ID: "matchday_hero", Title: "Matchday Hero", Description: "Achieve a total net profit > Rs 1000", Target: 1000, XP: 500}
	totalPnL := 0.0
	for _, p := range pos {
		totalPnL += (p.PnL + p.RealizedPnL)
	}
	c7.Progress = int(math.Max(0, totalPnL))
	challenges = append(challenges, finalize(c7))

	// 8. Diamond Hands
	c8 := Challenge{ID: "diamond_hands", Title: "Diamond Hands", Description: "Hold an open position with > Rs 500 profit", Target: 500, XP: 300}
	maxOpenPnl := 0.0
	for _, p := range pos {
		if p.Status == "open" && p.PnL > maxOpenPnl {
			maxOpenPnl = p.PnL
		}
	}
	c8.Progress = int(maxOpenPnl)
	challenges = append(challenges, finalize(c8))

	// 9. The Scalper
	c9 := Challenge{ID: "scalper", Title: "The Scalper", Description: "Realize > Rs 500 profit on a single trade", Target: 500, XP: 350}
	maxRealized := 0.0
	for _, p := range pos {
		if p.RealizedPnL > maxRealized {
			maxRealized = p.RealizedPnL
		}
	}
	c9.Progress = int(maxRealized)
	challenges = append(challenges, finalize(c9))

	// 10. High Roller
	c10 := Challenge{ID: "high_roller", Title: "High Roller", Description: "Trade 50 or more lots in a single contract", Target: 50, XP: 450}
	maxLots := 0
	for _, p := range pos {
		total := int(math.Abs(float64(p.Lots))) + p.MatchedLots
		if total > maxLots {
			maxLots = total
		}
	}
	c10.Progress = maxLots
	challenges = append(challenges, finalize(c10))

	return challenges
}

func finalize(c Challenge) Challenge {
	if c.Progress >= c.Target {
		c.Progress = c.Target
		c.Status = "COMPLETE"
	} else if c.Progress > 0 {
		c.Status = "IN_PROGRESS"
	} else {
		c.Status = "LOCKED"
	}
	return c
}
