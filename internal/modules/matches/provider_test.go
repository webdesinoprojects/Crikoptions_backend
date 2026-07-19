package matches

import (
	"context"
	"testing"
	"time"
)

func TestMemoryTradingGateAllowsHealthyWhenFeedValidUntilLapses(t *testing.T) {
	repository := NewMemoryRepository()
	validUntil := time.Now().UTC().Add(time.Minute)
	match, err := repository.Create(context.Background(), Match{
		DataSource: DataSourceSportmonks, StateVersion: 4, TradingVersion: 2,
		FeedState: FeedStateHealthy, TradingState: "open", TradingBlockers: []string{},
		FeedValidUntil: &validUntil,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, allowed, err := repository.VerifyTradingGate(context.Background(), match.ID, 4, 2)
	if err != nil || !allowed {
		t.Fatalf("valid gate allowed=%t err=%v", allowed, err)
	}
	expired := time.Now().UTC().Add(-time.Second)
	repository.mu.Lock()
	for i := range repository.matches {
		if repository.matches[i].ID == match.ID {
			repository.matches[i].FeedValidUntil = &expired
		}
	}
	repository.mu.Unlock()
	// Timestamp lapse alone must not block trading; worker flips feedState when stale.
	_, allowed, err = repository.VerifyTradingGate(context.Background(), match.ID, 4, 2)
	if err != nil || !allowed {
		t.Fatalf("lapsed feedValidUntil allowed=%t err=%v, want true", allowed, err)
	}
	repository.mu.Lock()
	for i := range repository.matches {
		if repository.matches[i].ID == match.ID {
			repository.matches[i].FeedState = FeedStateStale
		}
	}
	repository.mu.Unlock()
	_, allowed, err = repository.VerifyTradingGate(context.Background(), match.ID, 4, 2)
	if err != nil || allowed {
		t.Fatalf("stale feed allowed=%t err=%v, want false", allowed, err)
	}

	repository.mu.Lock()
	for i := range repository.matches {
		if repository.matches[i].ID == match.ID {
			repository.matches[i].FeedState = FeedStateReconciling
			repository.matches[i].TradingState = "open"
			repository.matches[i].TradingBlockers = []string{"reconciling"}
		}
	}
	repository.mu.Unlock()
	_, allowed, err = repository.VerifyTradingGate(context.Background(), match.ID, 4, 2)
	if err != nil || !allowed {
		t.Fatalf("soft sync reconciling allowed=%t err=%v, want true", allowed, err)
	}
}

func TestIsTradableAllowsSoftSync(t *testing.T) {
	match := &Match{
		DataSource: DataSourceSportmonks, Status: StatusLive,
		FeedState: FeedStateReconciling, TradingState: "open",
		TradingBlockers: []string{"reconciling"},
	}
	AnnotateTradable(match)
	if !match.Tradable || !IsTradable(match) {
		t.Fatal("soft sync live match must be tradable")
	}
	match.FeedState = FeedStateStale
	match.TradingBlockers = []string{"feed_stale"}
	match.TradingState = "blocked"
	AnnotateTradable(match)
	if match.Tradable {
		t.Fatal("stale feed must not be tradable")
	}
}
