package challenges

import (
	"sort"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/positions"
)

// Position sides. The client calls these "long call" and "short call"; the
// market model only distinguishes buy from sell.
const (
	sideBuy  = "BUY"
	sideSell = "SELL"
)

// sideStats summarises one side of a user's book. Every field is derived from
// settled position data, never from anything the client sends.
type sideStats struct {
	// opened is how many positions were ever taken on this side.
	opened int
	// profitableCloses counts positions with lots closed at a realised profit.
	profitableCloses int
	// bestInningsWins is the most profitable closes inside one innings market.
	bestInningsWins int
	// bestInningsStreak is the longest unbroken run of profitable closes inside
	// one innings market, ordered by when each position was last settled.
	bestInningsStreak int
}

// closed reports whether any lots of this position have been settled.
func closed(p positions.Position) bool { return p.MatchedLots > 0 }

// profitable reports a realised gain on the settled slice.
func profitable(p positions.Position) bool { return closed(p) && p.RealizedPnL > 0 }

func buildSideStats(pos []positions.Position, side string) sideStats {
	var stats sideStats
	// A market is one innings-score contract, so grouping by market is grouping
	// by innings.
	byMarket := make(map[string][]positions.Position)
	for _, p := range pos {
		if p.Side != side {
			continue
		}
		stats.opened++
		if profitable(p) {
			stats.profitableCloses++
		}
		byMarket[p.MarketID] = append(byMarket[p.MarketID], p)
	}

	for _, inMarket := range byMarket {
		sort.SliceStable(inMarket, func(i, j int) bool {
			return inMarket[i].UpdatedAt.Before(inMarket[j].UpdatedAt)
		})
		wins, streak := 0, 0
		for _, p := range inMarket {
			if !closed(p) {
				continue // still running; neither a win nor a break
			}
			if p.RealizedPnL > 0 {
				wins++
				streak++
				if streak > stats.bestInningsStreak {
					stats.bestInningsStreak = streak
				}
				continue
			}
			streak = 0
		}
		if wins > stats.bestInningsWins {
			stats.bestInningsWins = wins
		}
	}
	return stats
}

// EvaluatePositions derives every challenge's progress from a user's positions.
// It is the only place completion is decided.
func EvaluatePositions(pos []positions.Position) []Challenge {
	statsBySide := map[string]sideStats{
		AcademyLongCall:  buildSideStats(pos, sideBuy),
		AcademyShortCall: buildSideStats(pos, sideSell),
	}

	out := make([]Challenge, 0, len(definitions))
	for _, d := range definitions {
		c := Challenge{
			ID: d.ID, AcademyID: d.AcademyID, Title: d.Title,
			Description: d.Description, Target: d.Target, Reward: d.Reward,
			LockedReason: d.LockedReason,
		}
		if d.LockedReason != "" || d.Progress == nil {
			c.Status = StatusLocked
			out = append(out, c)
			continue
		}
		c.Progress = d.Progress(statsBySide[d.AcademyID])
		out = append(out, finalize(c))
	}
	return out
}

func finalize(c Challenge) Challenge {
	switch {
	case c.Progress >= c.Target:
		c.Progress = c.Target
		c.Status = StatusComplete
	case c.Progress > 0:
		c.Status = StatusInProgress
	default:
		c.Status = StatusLocked
	}
	return c
}
