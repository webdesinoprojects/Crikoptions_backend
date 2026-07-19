package store

import (
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/reconcile"
)

func TestMarkFeedFrozenRequiresStalePollsNotQuietScoreboard(t *testing.T) {
	now := time.Now().UTC()
	freshPoll := now.Add(-10 * time.Second)
	quietBoard := now.Add(-2 * time.Minute)
	cutoff := now.Add(-90 * time.Second)

	match := matches.Match{
		Status:               matches.StatusLive,
		LastSuccessfulPollAt: &freshPoll,
		LastStateChangeAt:    &quietBoard,
	}
	shouldFreeze := match.Status == matches.StatusLive &&
		!(match.LastSuccessfulPollAt == nil || match.LastSuccessfulPollAt.After(cutoff)) &&
		match.LastStateChangeAt != nil && !match.LastStateChangeAt.After(cutoff)
	if shouldFreeze {
		t.Fatal("fresh polls must not freeze on a quiet scoreboard")
	}

	stalePoll := now.Add(-3 * time.Minute)
	match.LastSuccessfulPollAt = &stalePoll
	shouldFreeze = match.Status == matches.StatusLive &&
		!(match.LastSuccessfulPollAt == nil || match.LastSuccessfulPollAt.After(cutoff)) &&
		match.LastStateChangeAt != nil && !match.LastStateChangeAt.After(cutoff)
	if !shouldFreeze {
		t.Fatal("stale polls with quiet scoreboard should freeze")
	}
}

func TestApplyFeedHealthOpensLiveWithoutFullMatrix(t *testing.T) {
	match := matches.Match{
		FeedState:       matches.FeedStateStale,
		TradingState:    "blocked",
		TradingBlockers: []string{"feed_stale"},
	}
	applyFeedHealth(&match, reconcile.Projection{Status: matches.StatusLive, CurrentInnings: 1, CurrentScore: 58})
	if match.FeedState != matches.FeedStateHealthy || match.TradingState != "open" {
		t.Fatalf("got feed=%s trading=%s blockers=%v", match.FeedState, match.TradingState, match.TradingBlockers)
	}
	if containsValue(match.TradingBlockers, "feed_stale") {
		t.Fatalf("feed_stale should be cleared: %v", match.TradingBlockers)
	}
}
