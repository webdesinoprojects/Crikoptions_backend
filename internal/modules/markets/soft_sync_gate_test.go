package markets

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// A soft sync marker must not suspend the market. Suspending it flipped the
// terminal's Buy/Sell to disabled (the client treats status SUSPENDED as a hard
// block) for the whole duration of a feed reconcile.
func TestCompatibilityStatusIgnoresSoftBlockers(t *testing.T) {
	for _, blocker := range []string{"reconciling", "warming"} {
		if got := compatibilityStatus(MarketLifecycleOpen, []string{blocker}); got != MarketStatusActive {
			t.Fatalf("compatibilityStatus(open, [%s]) = %q, want %q", blocker, got, MarketStatusActive)
		}
	}
	if got := compatibilityStatus(MarketLifecycleOpen, []string{"reconciling", "manual"}); got != MarketStatusSuspended {
		t.Fatalf("a hard blocker must still suspend, got %q", got)
	}
	if got := compatibilityStatus(MarketLifecycleSettled, nil); got != MarketStatusClosed {
		t.Fatalf("settled = %q, want %q", got, MarketStatusClosed)
	}
}

// VerifyProviderMarketGate used to require a literally empty blockers array,
// making it stricter than IsTradable: the pre-transaction checks passed and the
// order still 409'd inside the transaction for the length of a sync.
func TestVerifyProviderMarketGateAdmitsSoftSyncBlockers(t *testing.T) {
	repo := NewMemoryRepository()
	id := primitive.NewObjectID()
	repo.markets = append(repo.markets, Market{
		ID:             id,
		Kind:           MarketKindInningsScore,
		Lifecycle:      MarketLifecycleOpen,
		Status:         MarketStatusActive,
		Blockers:       []string{"reconciling"},
		FormulaVersion: FormulaVersionInningsScoreV1,
	})

	market, valid, err := repo.VerifyProviderMarketGate(context.Background(), id, 12, 3)
	if err != nil {
		t.Fatalf("VerifyProviderMarketGate: %v", err)
	}
	if !valid || market == nil {
		t.Fatal("soft sync blocker must not fail the market gate")
	}
	if !containsBlocker(market.Blockers, "reconciling") {
		t.Fatalf("gate must leave feed-owned blockers alone, got %v", market.Blockers)
	}
}

func TestVerifyProviderMarketGateRejectsHardBlockers(t *testing.T) {
	repo := NewMemoryRepository()
	id := primitive.NewObjectID()
	repo.markets = append(repo.markets, Market{
		ID:             id,
		Kind:           MarketKindInningsScore,
		Lifecycle:      MarketLifecycleOpen,
		Status:         MarketStatusActive,
		Blockers:       []string{"reconciling", "innings_break"},
		FormulaVersion: FormulaVersionInningsScoreV1,
	})

	if _, valid, err := repo.VerifyProviderMarketGate(context.Background(), id, 12, 3); err != nil || valid {
		t.Fatalf("hard blocker must fail the gate: valid=%v err=%v", valid, err)
	}
}

func containsBlocker(in []string, want string) bool {
	for _, value := range in {
		if value == want {
			return true
		}
	}
	return false
}
