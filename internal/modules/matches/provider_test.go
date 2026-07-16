package matches

import (
	"context"
	"testing"
	"time"
)

func TestMemoryTradingGateFailsClosedWhenFeedExpires(t *testing.T) {
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
	_, allowed, err = repository.VerifyTradingGate(context.Background(), match.ID, 4, 2)
	if err != nil || allowed {
		t.Fatalf("expired gate allowed=%t err=%v", allowed, err)
	}
}
