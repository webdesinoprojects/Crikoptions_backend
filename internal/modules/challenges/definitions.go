package challenges

// Academy IDs mirror the client's presentation grouping. Only these two have
// tradable instruments; the spread/iron-fly/iron-condor academies advertised in
// the UI have no matching market kind yet and are not served from here.
const (
	AcademyLongCall  = "long-call"
	AcademyShortCall = "short-call"
)

// lockedNoHoldTracking explains the one rule the platform cannot yet police:
// nothing records the over count at which a position was opened, so "held for N
// overs" is unverifiable. Shown to the user rather than silently approved.
const lockedNoHoldTracking = "Hold duration is not tracked yet — coming soon"

// definition is the server-owned source of truth for a challenge. Rewards live
// here and are never read from the request, so a claim cannot be inflated.
type definition struct {
	ID          string
	AcademyID   string
	Title       string
	Description string
	Target      int
	Reward      float64
	// LockedReason marks a challenge that cannot be honestly verified today.
	LockedReason string
	// Progress measures how far the user has come using only authoritative
	// position data. Nil when the challenge is locked.
	Progress func(s sideStats) int
}

// definitions is ordered; the client renders academies in this order.
var definitions = []definition{
	{
		ID: "lc-1", AcademyID: AcademyLongCall,
		Title: "First Trade", Description: "Buy your first call option.",
		Target: 1, Reward: 500,
		Progress: func(s sideStats) int { return s.opened },
	},
	{
		ID: "lc-2", AcademyID: AcademyLongCall,
		Title: "Green Candle", Description: "Close one call option with profit.",
		Target: 1, Reward: 1_000,
		Progress: func(s sideStats) int { return s.profitableCloses },
	},
	{
		ID: "lc-3", AcademyID: AcademyLongCall,
		Title: "Momentum Catcher", Description: "Complete 3 profitable long call trades in a single inning.",
		Target: 3, Reward: 2_500,
		Progress: func(s sideStats) int { return s.bestInningsWins },
	},
	{
		ID: "lc-4", AcademyID: AcademyLongCall,
		Title: "Call Expert", Description: "Finish 5 consecutive long call trades in a single inning.",
		Target: 5, Reward: 5_000,
		Progress: func(s sideStats) int { return s.bestInningsStreak },
	},
	{
		ID: "lc-5", AcademyID: AcademyLongCall,
		Title: "Rider", Description: "Hold a long call trade for at least 50 overs.",
		Target: 50, Reward: 10_000, LockedReason: lockedNoHoldTracking,
	},
	{
		ID: "sc-1", AcademyID: AcademyShortCall,
		Title: "First Premium", Description: "Sell your first call option.",
		Target: 1, Reward: 500,
		Progress: func(s sideStats) int { return s.opened },
	},
	{
		ID: "sc-2", AcademyID: AcademyShortCall,
		Title: "Premium Collector", Description: "Close one short call with profit.",
		Target: 1, Reward: 1_000,
		Progress: func(s sideStats) int { return s.profitableCloses },
	},
	{
		ID: "sc-3", AcademyID: AcademyShortCall,
		Title: "The Seller", Description: "Complete 3 profitable short call trades in a single inning.",
		Target: 3, Reward: 2_500,
		Progress: func(s sideStats) int { return s.bestInningsWins },
	},
	{
		ID: "sc-4", AcademyID: AcademyShortCall,
		Title: "Expert Seller", Description: "Finish 5 consecutive short call trades in a single inning.",
		Target: 5, Reward: 5_000,
		Progress: func(s sideStats) int { return s.bestInningsStreak },
	},
	{
		ID: "sc-5", AcademyID: AcademyShortCall,
		Title: "Rider", Description: "Hold a short call trade for at least 50 overs.",
		Target: 50, Reward: 10_000, LockedReason: lockedNoHoldTracking,
	},
}

// definitionByID indexes the table so a claim can resolve its reward without
// trusting anything on the request.
var definitionByID = func() map[string]definition {
	out := make(map[string]definition, len(definitions))
	for _, d := range definitions {
		out[d.ID] = d
	}
	return out
}()
