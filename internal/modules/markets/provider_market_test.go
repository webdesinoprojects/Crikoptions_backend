package markets

import (
	"context"
	"errors"
	"testing"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

type failingMarketListRepository struct {
	*MemoryRepository
	err error
}

func (r *failingMarketListRepository) ListByMatchID(context.Context, string) ([]Market, error) {
	return nil, r.err
}

func TestListMarketsByMatchIDPreservesReadFailure(t *testing.T) {
	readErr := errors.New("market read failed")
	svc := NewService(&failingMarketListRepository{MemoryRepository: NewMemoryRepository(), err: readErr})

	if _, err := svc.ListMarketsByMatchID(context.Background(), "match-1"); !errors.Is(err, readErr) {
		t.Fatalf("ListMarketsByMatchID() error = %v, want %v", err, readErr)
	}
	_ = svc.GetMarketsByMatchID(context.Background(), "match-1")
}

func TestProviderMarketTradabilityFailsClosed(t *testing.T) {
	svc := NewService(NewMemoryRepository())
	tests := []struct {
		name   string
		market *Market
		want   bool
	}{
		{name: "nil", market: nil, want: false},
		{name: "missing legacy status", market: &Market{}, want: false},
		{name: "active legacy", market: &Market{Status: MarketStatusActive}, want: true},
		{name: "provider missing lifecycle", market: &Market{Kind: MarketKindInningsScore, Status: MarketStatusActive}, want: false},
		{name: "provider open", market: &Market{Kind: MarketKindInningsScore, Lifecycle: MarketLifecycleOpen, Status: MarketStatusActive}, want: true},
		{name: "provider soft sync blocker", market: &Market{Kind: MarketKindInningsScore, Lifecycle: MarketLifecycleOpen, Status: MarketStatusActive, Blockers: []string{"reconciling"}}, want: true},
		{name: "provider blocker", market: &Market{Kind: MarketKindInningsScore, Lifecycle: MarketLifecycleOpen, Status: MarketStatusActive, Blockers: []string{"feed_stale"}}, want: false},
		{name: "provider compatibility suspended", market: &Market{Kind: MarketKindInningsScore, Lifecycle: MarketLifecycleOpen, Status: MarketStatusSuspended}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := svc.IsTradable(tt.market); got != tt.want {
				t.Fatalf("IsTradable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnsureProviderInningsMarketIsIdempotentAndImmutable(t *testing.T) {
	repo := NewMemoryRepository()
	svc := NewService(repo)
	spec := ProviderInningsMarketSpec{
		MatchID:         "provider-match",
		Innings:         1,
		BattingTeamName: "India",
		Format:          "T20I",
		ScheduledBalls:  120,
		StateVersion:    7,
		TradingVersion:  3,
		FeedState:       matches.FeedStateHealthy,
	}
	if err := svc.EnsureProviderInningsMarket(context.Background(), spec); err != nil {
		t.Fatalf("EnsureProviderInningsMarket: %v", err)
	}

	// A later ensure updates projection versions but cannot rewrite the contract.
	spec.Format = "ODI"
	spec.ScheduledBalls = 300
	spec.StateVersion = 8
	spec.TradingVersion = 4
	if err := svc.EnsureProviderInningsMarket(context.Background(), spec); err != nil {
		t.Fatalf("second EnsureProviderInningsMarket: %v", err)
	}

	got := svc.GetMarketsByMatchID(context.Background(), "provider-match")
	if len(got) != 1 {
		t.Fatalf("market count = %d, want 1", len(got))
	}
	market := got[0]
	if market.Title != "India Innings 1 Score" || market.Kind != MarketKindInningsScore || market.FormulaVersion != FormulaVersionInningsScoreV1 {
		t.Fatalf("contract identity = %+v", market)
	}
	if market.Format != "T20I" || market.ScheduledBalls != 120 || market.StrikeMin != 10 || market.StrikeMax != 250 || market.StrikeStep != 10 {
		t.Fatalf("immutable terms changed: %+v", market)
	}
	if market.MatchStateVersion != 8 || market.TradingVersion != 4 {
		t.Fatalf("versions = %d/%d, want 8/4", market.MatchStateVersion, market.TradingVersion)
	}
}

func TestSetProviderMarketGateStoresFinalInputsMonotonically(t *testing.T) {
	repo := NewMemoryRepository()
	svc := NewService(repo)
	if err := svc.EnsureProviderInningsMarket(context.Background(), ProviderInningsMarketSpec{
		MatchID: "provider-match", Innings: 1, BattingTeamName: "India", Format: "T20", ScheduledBalls: 120,
	}); err != nil {
		t.Fatal(err)
	}

	final := 176
	if err := svc.SetProviderMarketGate(context.Background(), "provider-match", 1, MarketLifecycleSettling, []string{"finalizing", "finalizing"}, &final, 11); err != nil {
		t.Fatalf("SetProviderMarketGate: %v", err)
	}
	older := 170
	if err := svc.SetProviderMarketGate(context.Background(), "provider-match", 1, MarketLifecycleSettling, []string{"finalizing"}, &older, 10); err != nil {
		t.Fatalf("older revision should be idempotently ignored: %v", err)
	}
	conflict := 177
	if err := svc.SetProviderMarketGate(context.Background(), "provider-match", 1, MarketLifecycleSettling, []string{"finalizing"}, &conflict, 11); !errors.Is(err, ErrFinalRevisionConflict) {
		t.Fatalf("same-revision conflict error = %v", err)
	}

	market := svc.GetMarketsByMatchID(context.Background(), "provider-match")[0]
	if market.FinalScore != 176 || market.FinalRevision != 11 {
		t.Fatalf("final input = %d@%d, want 176@11", market.FinalScore, market.FinalRevision)
	}
	if len(market.Blockers) != 1 || market.Blockers[0] != "finalizing" {
		t.Fatalf("blockers = %v", market.Blockers)
	}
	if err := svc.SetProviderMarketGate(context.Background(), "provider-match", 1, MarketLifecycleOpen, []string{"finalizing"}, nil, 0); err != nil {
		t.Fatalf("unclaimed settlement must allow a correction hold reset: %v", err)
	}
	market = svc.GetMarketsByMatchID(context.Background(), "provider-match")[0]
	if market.FinalScore != 0 || market.FinalRevision != 0 {
		t.Fatalf("reopened provisional final was retained: %d@%d", market.FinalScore, market.FinalRevision)
	}
	if err := svc.SetProviderMarketGate(context.Background(), "provider-match", 1, MarketLifecycleSettling, []string{"finalizing"}, &final, 12); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetProviderMarketGate(context.Background(), "provider-match", 1, MarketLifecycleVoid, nil, nil, 0); err != nil {
		t.Fatalf("void unclaimed provisional final: %v", err)
	}
	market = svc.GetMarketsByMatchID(context.Background(), "provider-match")[0]
	if market.Lifecycle != MarketLifecycleVoid || market.FinalScore != 0 || market.FinalRevision != 0 {
		t.Fatalf("void retained provisional final: %+v", market)
	}
}

func TestVerifyProviderMarketGateConditionallyTouchesOnlyOpenVersion(t *testing.T) {
	repo := NewMemoryRepository()
	svc := NewService(repo)
	if err := svc.EnsureProviderInningsMarket(context.Background(), ProviderInningsMarketSpec{
		MatchID: "provider-match", Innings: 1, BattingTeamName: "India", Format: "T20", ScheduledBalls: 120,
		StateVersion: 9, TradingVersion: 4, FeedState: matches.FeedStateHealthy,
	}); err != nil {
		t.Fatal(err)
	}
	market := svc.GetMarketsByMatchID(context.Background(), "provider-match")[0]
	verified, valid, err := svc.VerifyProviderMarketGate(context.Background(), market.ID.Hex(), 9, 4)
	if err != nil || !valid {
		t.Fatalf("verify = %v/%v", valid, err)
	}
	if verified.GateCheckSeq != 1 || verified.TradingGateCheckedAt == nil {
		t.Fatalf("gate touch not persisted: %+v", verified)
	}
	if err := svc.SetProviderMarketGate(context.Background(), "provider-match", 1, MarketLifecycleOpen, []string{"feed_stale"}, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, valid, err := svc.VerifyProviderMarketGate(context.Background(), market.ID.Hex(), 9, 4); err != nil || valid {
		t.Fatalf("suspended gate verify = %v/%v, want false/nil", valid, err)
	}
}

func TestProviderManualSuspensionSurvivesFeedRecovery(t *testing.T) {
	repo := NewMemoryRepository()
	svc := NewService(repo)
	if err := svc.EnsureProviderInningsMarket(context.Background(), ProviderInningsMarketSpec{
		MatchID: "provider-match", Innings: 1, BattingTeamName: "India", Format: "T20", ScheduledBalls: 120,
		StateVersion: 9, TradingVersion: 4, FeedState: matches.FeedStateHealthy,
	}); err != nil {
		t.Fatal(err)
	}
	market := svc.GetMarketsByMatchID(context.Background(), "provider-match")[0]
	if _, err := svc.SetMarketStatus(context.Background(), market.ID.Hex(), MarketStatusSuspended); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetProviderMarketGate(context.Background(), "provider-match", 1, MarketLifecycleOpen, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	market = svc.GetMarketsByMatchID(context.Background(), "provider-match")[0]
	if market.Status != MarketStatusSuspended || len(market.Blockers) != 1 || market.Blockers[0] != "manual" {
		t.Fatalf("manual suspension was cleared by feed recovery: %+v", market)
	}
	if _, err := svc.SetMarketStatus(context.Background(), market.ID.Hex(), MarketStatusActive); err != nil {
		t.Fatal(err)
	}
	market = svc.GetMarketsByMatchID(context.Background(), "provider-match")[0]
	if market.Status != MarketStatusActive || len(market.Blockers) != 0 {
		t.Fatalf("manual suspension was not cleared explicitly: %+v", market)
	}
}

func TestProviderSettlementClaimFreezesFinalRevision(t *testing.T) {
	repo := NewMemoryRepository()
	svc := NewService(repo)
	if err := svc.EnsureProviderInningsMarket(context.Background(), ProviderInningsMarketSpec{
		MatchID: "provider-match", Innings: 1, BattingTeamName: "India", Format: "T20", ScheduledBalls: 120,
	}); err != nil {
		t.Fatal(err)
	}
	final := 176
	if err := svc.SetProviderMarketGate(context.Background(), "provider-match", 1, MarketLifecycleSettling, []string{"finalizing"}, &final, 11); err != nil {
		t.Fatal(err)
	}
	claimed, err := svc.ClaimProviderSettlement(context.Background(), "provider-match", 1, 11)
	if err != nil || !claimed {
		t.Fatalf("claim = %v/%v", claimed, err)
	}
	if err := svc.SetProviderMarketGate(context.Background(), "provider-match", 1, MarketLifecycleOpen, []string{"finalizing"}, nil, 0); !errors.Is(err, ErrFinalRevisionConflict) {
		t.Fatalf("claimed settlement reopened: %v", err)
	}
	corrected := 177
	if err := svc.SetProviderMarketGate(context.Background(), "provider-match", 1, MarketLifecycleSettling, []string{"finalizing"}, &corrected, 12); !errors.Is(err, ErrFinalRevisionConflict) {
		t.Fatalf("post-claim correction error = %v", err)
	}
	if err := svc.SetProviderMarketGate(context.Background(), "provider-match", 1, MarketLifecycleSettled, nil, &final, 11); err != nil {
		t.Fatalf("settle claimed revision: %v", err)
	}
	if err := svc.SetProviderMarketGate(context.Background(), "provider-match", 1, MarketLifecycleSettling, []string{"finalizing"}, &final, 11); err != nil {
		t.Fatalf("terminal heartbeat should be idempotent: %v", err)
	}
	market := svc.GetMarketsByMatchID(context.Background(), "provider-match")[0]
	if market.Lifecycle != MarketLifecycleSettled || market.FinalRevision != 11 || market.FinalScore != final {
		t.Fatalf("settled market changed: %+v", market)
	}
}
