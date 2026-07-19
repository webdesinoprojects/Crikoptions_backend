package matches

import "strings"

// SoftSyncFeedStates are transient feed phases where the scoreboard is still
// live and buy/sell must remain available (UI may show SYNCING).
var SoftSyncFeedStates = map[string]struct{}{
	FeedStateHealthy:     {},
	FeedStateReconciling: {},
	FeedStateWarming:     {},
}

// SoftSyncTradingBlockers are feed-sync markers. They must not block orders
// while the match is live — only hard blockers (stale, innings break, etc.).
var SoftSyncTradingBlockers = map[string]struct{}{
	"reconciling": {},
	"warming":     {},
}

// FeedAllowsTrading reports whether feedState is healthy or a soft sync phase.
func FeedAllowsTrading(feedState string) bool {
	_, ok := SoftSyncFeedStates[strings.TrimSpace(feedState)]
	return ok
}

// HardTradingBlockers returns blockers that must suspend buy/sell.
func HardTradingBlockers(blockers []string) []string {
	out := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		blocker = strings.TrimSpace(blocker)
		if blocker == "" {
			continue
		}
		if _, soft := SoftSyncTradingBlockers[blocker]; soft {
			continue
		}
		out = append(out, blocker)
	}
	return out
}

// HasHardTradingBlockers is true when any non-soft-sync blocker is present.
func HasHardTradingBlockers(blockers []string) bool {
	return len(HardTradingBlockers(blockers)) > 0
}

// IsTradable reports whether buy/sell should be allowed for this match.
// Soft SYNC (reconciling/warming) remains tradable while status is live.
func IsTradable(match *Match) bool {
	if match == nil {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(match.Status))
	provider := strings.EqualFold(strings.TrimSpace(match.DataSource), DataSourceSportmonks) ||
		strings.EqualFold(strings.TrimSpace(match.Provider), DataSourceSportmonks)
	if provider {
		return status == StatusLive &&
			FeedAllowsTrading(match.FeedState) &&
			strings.EqualFold(strings.TrimSpace(match.TradingState), "open") &&
			!HasHardTradingBlockers(match.TradingBlockers)
	}
	switch status {
	case StatusLive, StatusInningsBreak:
		return true
	default:
		return false
	}
}

// AnnotateTradable sets the computed Tradable flag for API responses.
func AnnotateTradable(match *Match) {
	if match == nil {
		return
	}
	match.Tradable = IsTradable(match)
}
