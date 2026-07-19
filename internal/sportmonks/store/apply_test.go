package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/reconcile"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type failingMarketProjector struct {
	err error
}

func (p *failingMarketProjector) ListMarketsByMatchID(context.Context, string) ([]markets.Market, error) {
	return nil, p.err
}

func (p *failingMarketProjector) EnsureProviderInningsMarket(context.Context, markets.ProviderInningsMarketSpec) error {
	return nil
}

func (p *failingMarketProjector) SetProviderMarketGate(context.Context, string, int, string, []string, *int, int64) error {
	return nil
}

func (p *failingMarketProjector) SetProviderManualBlocker(context.Context, primitive.ObjectID, bool) (*markets.Market, error) {
	return nil, nil
}

func TestProjectMarketsFailsClosedOnReadFailure(t *testing.T) {
	readErr := errors.New("market read failed")
	store := &Store{markets: &failingMarketProjector{err: readErr}}
	match := matches.Match{
		ID: primitive.NewObjectID(), Status: matches.StatusAbandoned, Innings: 1,
		ProviderBattingTeamID: 1, ProviderTeamBID: 2,
		TeamAName: "Alpha", Format: "T20", ScheduledBalls: 120,
	}

	err := store.projectMarkets(context.Background(), match, reconcile.Projection{})
	if !errors.Is(err, readErr) {
		t.Fatalf("projectMarkets() error = %v, want %v", err, readErr)
	}
}

func TestVoidJobConversionUpdateClearsSettlementState(t *testing.T) {
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	update := voidJobConversionUpdate(SettlementJob{
		MatchID: "match-1", Innings: 2,
		FormulaVersion: "innings_score_v1",
		Action:         "void", Status: "pending", UpdatedAt: now,
	})

	set, ok := update["$set"].(primitive.M)
	if !ok {
		t.Fatalf("$set = %T, want primitive.M", update["$set"])
	}
	if set["matchId"] != "match-1" || set["innings"] != 2 || set["action"] != "void" || set["status"] != "pending" {
		t.Fatalf("unexpected conversion fields: %#v", set)
	}
	unset, ok := update["$unset"].(primitive.M)
	if !ok {
		t.Fatalf("$unset = %T, want primitive.M", update["$unset"])
	}
	for _, field := range []string{"finalScore", "finalRevision", "snapshotHash", "leaseOwner", "leaseUntil", "lastError"} {
		if _, ok := unset[field]; !ok {
			t.Errorf("conversion does not clear %s: %#v", field, unset)
		}
	}
}

func TestClassifyUnconvertedVoidJob(t *testing.T) {
	tests := []struct {
		name    string
		status  string
		wantErr error
	}{
		{name: "pending race fails closed", status: "pending", wantErr: ErrConcurrentApply},
		{name: "failed race fails closed", status: "failed", wantErr: ErrConcurrentApply},
		{name: "held race fails closed", status: "held", wantErr: ErrConcurrentApply},
		{name: "complete is preserved", status: "complete"},
		{name: "processing fails closed", status: "processing", wantErr: ErrSettlementInFlight},
		{name: "unknown fails closed", status: "mystery", wantErr: errors.New("unknown")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyUnconvertedVoidJob(tt.status)
			if tt.wantErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if errors.Is(tt.wantErr, ErrConcurrentApply) && !errors.Is(err, ErrConcurrentApply) {
				t.Fatalf("error = %v, want ErrConcurrentApply", err)
			}
			if errors.Is(tt.wantErr, ErrSettlementInFlight) && !errors.Is(err, ErrSettlementInFlight) {
				t.Fatalf("error = %v, want ErrSettlementInFlight", err)
			}
			if tt.status == "mystery" && err == nil {
				t.Fatal("unknown status was accepted")
			}
		})
	}
}

func testProjection(status, hash string) reconcile.Projection {
	return reconcile.Projection{
		FixtureID: 42, LeagueID: 7, SeasonID: 8, LocalTeamID: 10, VisitorTeamID: 11,
		LocalTeamName: "Alpha", VisitorTeamName: "Beta", Format: "T20", ScheduledBalls: 120,
		ProviderStatus: "1st Innings", Status: status, CurrentInnings: 1,
		BattingTeamID: 10, CurrentScore: 10, Wickets: 1, LegalBalls: 6,
		Innings:      []reconcile.Innings{{Number: 1, BattingTeamID: 10, Runs: 10, Wickets: 1, LegalBalls: 6, ScheduledBalls: 120}},
		SnapshotHash: hash,
	}
}

func TestProjectMatchOpensOnFirstLiveSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	current := initialMatch(testProjection(matches.StatusLive, "one"), now)
	first := projectMatch(current, testProjection(matches.StatusLive, "one"), now, time.Minute, 2*time.Minute, 50*time.Second, 1)
	if first.FeedState != matches.FeedStateHealthy || first.TradingState != "open" || len(first.TradingBlockers) != 0 {
		t.Fatalf("first live snapshot should open: state=%s/%s blockers=%v", first.FeedState, first.TradingState, first.TradingBlockers)
	}
	second := projectMatch(first, testProjection(matches.StatusLive, "two"), now.Add(5*time.Second), time.Minute, 2*time.Minute, 50*time.Second, 2)
	if second.FeedState != matches.FeedStateHealthy || second.TradingState != "open" {
		t.Fatalf("second snapshot=%+v", second)
	}
}

func TestProjectMatchOpensImmediatelyOnNewInnings(t *testing.T) {
	now := time.Now().UTC()
	current := initialMatch(testProjection(matches.StatusLive, "innings-one"), now)
	current.Status = matches.StatusInningsBreak
	current.FeedState = matches.FeedStateHealthy
	current.HealthySnapshotCount = 8
	current.TradingBlockers = []string{"innings_break"}
	projection := testProjection(matches.StatusLive, "innings-two")
	projection.CurrentInnings = 2
	projection.BattingTeamID = projection.VisitorTeamID
	projection.Innings = []reconcile.Innings{{
		Number: 2, BattingTeamID: projection.VisitorTeamID, Runs: 0, ScheduledBalls: 120,
	}}
	next := projectMatch(current, projection, now.Add(time.Minute), time.Minute, 2*time.Minute, 50*time.Second, 9)
	if next.HealthySnapshotCount != 1 || next.FeedState != matches.FeedStateHealthy || next.TradingState != "open" {
		t.Fatalf("new live innings should open immediately: %+v", next)
	}
	if containsValue(next.TradingBlockers, "warming") || containsValue(next.TradingBlockers, "innings_break") {
		t.Fatalf("stale blockers should clear on new live innings: %v", next.TradingBlockers)
	}
}

func TestProjectMatchPreservesManualBlocker(t *testing.T) {
	now := time.Now().UTC()
	current := initialMatch(testProjection(matches.StatusLive, "one"), now)
	current.HealthySnapshotCount = 1
	current.TradingBlockers = []string{"manual", "warming"}
	next := projectMatch(current, testProjection(matches.StatusLive, "two"), now, time.Minute, time.Minute, 50*time.Second, 2)
	if next.TradingState != "blocked" || len(next.TradingBlockers) != 1 || next.TradingBlockers[0] != "manual" {
		t.Fatalf("manual blocker was not preserved: %s %v", next.TradingState, next.TradingBlockers)
	}
}

func TestTradingGateRefreshesAfterCancellationCompletes(t *testing.T) {
	current := initialMatch(testProjection(matches.StatusLive, "stable"), time.Now().UTC())
	current.Status = matches.StatusLive
	current.FeedState = matches.FeedStateHealthy
	current.HealthySnapshotCount = 2
	current.TradingState = "blocked"
	current.TradingBlockers = nil
	if !tradingGateRefreshNeeded(current, testProjection(matches.StatusLive, "stable")) {
		t.Fatal("a drained cancellation gate must reopen on the next reconciled poll")
	}
	current.TradingBlockers = []string{"manual"}
	if tradingGateRefreshNeeded(current, testProjection(matches.StatusLive, "stable")) {
		t.Fatal("automatic recovery must not clear a manual blocker")
	}
}

func TestStructurallyCompleteLiveInningsBlocksImmediately(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	projection := testProjection(matches.StatusLive, "innings-final")
	projection.Innings[0].Complete = true
	current := initialMatch(projection, now.Add(-time.Second))
	current.Status = matches.StatusLive
	current.FeedState = matches.FeedStateHealthy
	current.HealthySnapshotCount = 2
	current.TradingState = "open"
	current.TradingBlockers = nil

	next := projectMatch(current, projection, now, time.Minute, 2*time.Minute, 50*time.Second, 2)
	if next.FeedState != matches.FeedStateFinalizing || next.TradingState != "blocked" || !containsValue(next.TradingBlockers, "finalizing") {
		t.Fatalf("complete live innings remained tradable: state=%s trading=%s blockers=%v", next.FeedState, next.TradingState, next.TradingBlockers)
	}
}

func TestDeliveryCorrectionRecoversOnNextLivePoll(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	projection := testProjection(matches.StatusLive, "corrected")
	current := initialMatch(projection, now.Add(-time.Second))
	current.Status = matches.StatusLive
	current.FeedState = matches.FeedStateHealthy
	current.HealthySnapshotCount = 8
	current.TradingState = "open"
	current.TradingBlockers = nil
	resetCorrectionRecovery(&current, 1, 0)

	one := projectMatch(current, projection, now, time.Minute, 2*time.Minute, 50*time.Second, 2)
	if one.FeedState != matches.FeedStateHealthy || one.TradingState != "open" || one.HealthySnapshotCount != 1 {
		t.Fatalf("corrected live poll should reopen immediately: state=%s trading=%s count=%d", one.FeedState, one.TradingState, one.HealthySnapshotCount)
	}
}

func TestFinalizationRequiresThreeIdenticalPollsAndHold(t *testing.T) {
	start := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	projection := testProjection(matches.StatusCompleted, "final")
	current := initialMatch(projection, start)
	one := projectMatch(current, projection, start, time.Minute, 2*time.Minute, 50*time.Second, 1)
	two := projectMatch(one, projection, start.Add(time.Minute), time.Minute, 2*time.Minute, 50*time.Second, 2)
	if two.FeedState != matches.FeedStateFinalizing {
		t.Fatalf("premature terminal state: %s", two.FeedState)
	}
	three := projectMatch(two, projection, start.Add(2*time.Minute), time.Minute, 2*time.Minute, 50*time.Second, 3)
	if three.FeedState != matches.FeedStateTerminal || three.Status != matches.StatusCompleted {
		t.Fatalf("final state=%s/%s", three.FeedState, three.Status)
	}
	if three.FinalCandidate.Revision != one.FinalCandidate.Revision {
		t.Fatalf("identical final revision changed: %d -> %d", one.FinalCandidate.Revision, three.FinalCandidate.Revision)
	}
	corrected := projection
	corrected.SnapshotHash = "corrected"
	reset := projectMatch(two, corrected, start.Add(2*time.Minute), time.Minute, 2*time.Minute, 50*time.Second, 3)
	if reset.FeedState != matches.FeedStateFinalizing || reset.FinalCandidate.IdenticalPolls != 1 {
		t.Fatalf("correction did not reset hold: %+v", reset.FinalCandidate)
	}
}

func TestInningsSettlementReadyRequiresThreeIdenticalPollsAndHold(t *testing.T) {
	start := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	projection := testProjection(matches.StatusInningsBreak, "break")
	projection.Innings[0].Complete = true
	current := initialMatch(projection, start)
	one := projectMatch(current, projection, start, time.Minute, 2*time.Minute, 50*time.Second, 1)
	two := projectMatch(one, projection, start.Add(30*time.Second), time.Minute, 2*time.Minute, 50*time.Second, 2)
	if two.InningsSummaries[0].SettlementReady {
		t.Fatal("innings became settlement-ready before hold")
	}
	three := projectMatch(two, projection, start.Add(time.Minute), time.Minute, 2*time.Minute, 50*time.Second, 3)
	if !three.InningsSummaries[0].SettlementReady || three.InningsSummaries[0].FinalCandidate.IdenticalPolls != 3 {
		t.Fatalf("innings candidate=%+v", three.InningsSummaries[0])
	}
	corrected := projection
	corrected.Innings = append([]reconcile.Innings(nil), projection.Innings...)
	corrected.Innings[0].Runs++
	reset := projectMatch(two, corrected, start.Add(time.Minute), time.Minute, 2*time.Minute, 50*time.Second, 3)
	if reset.InningsSummaries[0].SettlementReady || reset.InningsSummaries[0].FinalCandidate.IdenticalPolls != 1 {
		t.Fatalf("corrected innings did not reset hold: %+v", reset.InningsSummaries[0])
	}
}

func TestInningsDeliveryCorrectionResetsFinalHold(t *testing.T) {
	start := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	projection := testProjection(matches.StatusInningsBreak, "break")
	projection.Innings[0].Complete = true
	projection.Innings[0].SnapshotHash = "deliveries-v1"
	one := projectMatch(initialMatch(projection, start), projection, start, time.Minute, 2*time.Minute, 50*time.Second, 1)
	two := projectMatch(one, projection, start.Add(30*time.Second), time.Minute, 2*time.Minute, 50*time.Second, 2)
	corrected := projection
	corrected.Innings = append([]reconcile.Innings(nil), projection.Innings...)
	corrected.Innings[0].SnapshotHash = "deliveries-v2"
	reset := projectMatch(two, corrected, start.Add(time.Minute), time.Minute, 2*time.Minute, 50*time.Second, 3)
	if reset.InningsSummaries[0].SettlementReady || reset.InningsSummaries[0].FinalCandidate.IdenticalPolls != 1 {
		t.Fatalf("delivery correction did not reset hold: %+v", reset.InningsSummaries[0])
	}
}

func TestDeliveryEventKeepsDetailedExtrasAndProviderIdentity(t *testing.T) {
	now := time.Now().UTC()
	event := deliveryEvent("match", 42, reconcile.Delivery{
		ProviderEventID: "100", ProviderScoreID: 15, ProviderBall: "0.1",
		Innings: 1, Sequence: 2, TeamRuns: 5, LegalBall: false,
		Extras: matches.DeliveryExtras{NoBalls: 1, Byes: 4}, PayloadHash: "hash",
	}, 1, primitive.NewObjectID(), now, now)
	if event.ProviderEventID != "100" || event.ProviderBall != "0.1" || event.Sequence != 2 {
		t.Fatalf("provider identity=%+v", event)
	}
	if event.Extras.NoBalls != 1 || event.Extras.Byes != 4 || event.Extra == nil || *event.Extra != matches.ExtraNoBall {
		t.Fatalf("extras=%+v legacy=%v", event.Extras, event.Extra)
	}
}

func TestShadowDiffMeasuresCorrectionsAndMissingDeliveries(t *testing.T) {
	previous := []reconcile.Delivery{
		{ProviderEventID: "1", PayloadHash: "same"},
		{ProviderEventID: "2", PayloadHash: "old"},
		{ProviderEventID: "3", PayloadHash: "missing"},
	}
	next := []reconcile.Delivery{
		{ProviderEventID: "1", PayloadHash: "same"},
		{ProviderEventID: "2", PayloadHash: "corrected"},
		{ProviderEventID: "4", PayloadHash: "new"},
	}
	corrections, missing := shadowDiff(previous, next)
	if corrections != 1 || missing != 1 {
		t.Fatalf("corrections/missing = %d/%d", corrections, missing)
	}
}

func TestFixtureIdentityChangeFailsClosed(t *testing.T) {
	current := matches.Match{
		ProviderFixtureID: 99,
		ProviderLeagueID:  7,
		ProviderSeasonID:  8,
		ProviderTeamAID:   10,
		ProviderTeamBID:   11,
	}
	projection := reconcile.Projection{
		FixtureID: 99, LeagueID: 7, SeasonID: 8,
		LocalTeamID: 10, VisitorTeamID: 11,
	}
	if fixtureIdentityChanged(current, projection) {
		t.Fatal("unchanged identity was rejected")
	}
	projection.LeagueID = 9
	if !fixtureIdentityChanged(current, projection) {
		t.Fatal("league identity drift was accepted")
	}
}

func TestAbandonmentFreezesContractDisposition(t *testing.T) {
	start := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	current := matches.Match{
		Status: matches.StatusInningsBreak,
		InningsSummaries: []matches.InningsSummary{
			{Innings: 1, Runs: 181, Complete: true, SettlementReady: true, FinalCandidate: &matches.FinalCandidate{Revision: 7, SnapshotHash: "final-1", IdenticalPolls: 3, FirstSeenAt: start.Add(-time.Minute)}},
			{Innings: 2, Runs: 40, Complete: true, FinalCandidate: &matches.FinalCandidate{Revision: 8, SnapshotHash: "final-2", IdenticalPolls: 1, FirstSeenAt: start}},
		},
	}
	projection := reconcile.Projection{
		Status: matches.StatusAbandoned, CurrentInnings: 2, Format: "T20", ScheduledBalls: 120,
		Innings: []reconcile.Innings{
			{Number: 1, Runs: 181, Complete: true, SnapshotHash: "final-1"},
			{Number: 2, Runs: 40, Complete: true, SnapshotHash: "final-2"},
		},
	}
	first := projectMatch(current, projection, start, time.Minute, 2*time.Minute, 50*time.Second, 9)
	if first.InningsSummaries[0].FinalDisposition != matches.FinalDispositionSettle ||
		first.InningsSummaries[1].FinalDisposition != matches.FinalDispositionVoid {
		t.Fatalf("first abandonment dispositions = %+v", first.InningsSummaries)
	}
	corrected := projection
	corrected.Innings = append([]reconcile.Innings(nil), projection.Innings...)
	corrected.Innings[0].SnapshotHash = "corrected-final-1"
	pending := projectMatch(first, corrected, start.Add(time.Second), time.Minute, 2*time.Minute, 50*time.Second, 10)
	if pending.InningsSummaries[0].FinalDisposition != matches.FinalDispositionSettle ||
		pending.InningsSummaries[0].SettlementReady || pending.FeedState != matches.FeedStateFinalizing {
		t.Fatalf("corrected settle disposition did not re-enter finalization: %+v", pending)
	}
	second := projectMatch(first, projection, start.Add(2*time.Minute), time.Minute, 2*time.Minute, 50*time.Second, 10)
	if second.InningsSummaries[1].SettlementReady || second.InningsSummaries[1].FinalCandidate != nil ||
		second.InningsSummaries[1].FinalDisposition != matches.FinalDispositionVoid {
		t.Fatalf("void disposition advanced after abandonment: %+v", second.InningsSummaries[1])
	}
}

func TestAbandonmentPreservesFinalizedInningsMarket(t *testing.T) {
	ctx := context.Background()
	marketService := markets.NewService(markets.NewMemoryRepository())
	matchID := primitive.NewObjectID()
	for innings := 1; innings <= 2; innings++ {
		if err := marketService.EnsureProviderInningsMarket(ctx, markets.ProviderInningsMarketSpec{
			MatchID: matchID.Hex(), Innings: innings, BattingTeamName: "Alpha",
			Format: "T20", ScheduledBalls: 120, StateVersion: 8, TradingVersion: 4,
			FeedState: matches.FeedStateHealthy,
		}); err != nil {
			t.Fatal(err)
		}
	}
	match := matches.Match{
		ID: matchID, Status: matches.StatusAbandoned, Innings: 2,
		ProviderBattingTeamID: 11, ProviderTeamBID: 11,
		TeamAName: "Alpha", TeamBName: "Beta", Format: "T20", ScheduledBalls: 120,
		StateVersion: 8, TradingVersion: 4,
		InningsSummaries: []matches.InningsSummary{
			{Innings: 1, Runs: 181, SettlementReady: true, FinalDisposition: matches.FinalDispositionSettle, FinalCandidate: &matches.FinalCandidate{Revision: 7, SnapshotHash: "final-1"}},
			{Innings: 2, Runs: 40},
		},
	}
	store := &Store{markets: marketService}
	if err := store.projectMarkets(ctx, match, reconcile.Projection{}); err != nil {
		t.Fatal(err)
	}
	marketList, err := marketService.ListMarketsByMatchID(ctx, matchID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	for _, market := range marketList {
		switch market.Innings {
		case 1:
			if market.Lifecycle != markets.MarketLifecycleSettling || market.FinalScore != 181 || market.FinalRevision != 7 || !containsValue(market.Blockers, "finalizing") {
				t.Fatalf("finalized innings market = %+v", market)
			}
		case 2:
			if market.Lifecycle != markets.MarketLifecycleSettling || market.FinalRevision != 0 || !containsValue(market.Blockers, "voiding") {
				t.Fatalf("unfinished innings market = %+v", market)
			}
		}
	}
}

func TestProviderMarketOpensOnFirstHealthySnapshot(t *testing.T) {
	ctx := context.Background()
	marketService := markets.NewService(markets.NewMemoryRepository())
	matchID := primitive.NewObjectID()
	match := matches.Match{
		ID: matchID, Status: matches.StatusLive, Innings: 1,
		ProviderBattingTeamID: 10, ProviderTeamAID: 10,
		TeamAName: "Alpha", TeamBName: "Beta", Format: "T20", ScheduledBalls: 120,
		StateVersion: 50, TradingVersion: 7, HealthySnapshotCount: 1,
		FeedState: matches.FeedStateHealthy, TradingBlockers: []string{},
	}
	store := &Store{markets: marketService}
	if err := store.projectMarkets(ctx, match, reconcile.Projection{}); err != nil {
		t.Fatal(err)
	}
	marketList, err := marketService.ListMarketsByMatchID(ctx, matchID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if len(marketList) != 1 || marketList[0].Lifecycle != markets.MarketLifecycleOpen {
		t.Fatalf("market after first healthy snapshot = %+v", marketList)
	}
}

func TestOpenedProviderMarketStaysTradableDuringSoftSync(t *testing.T) {
	ctx := context.Background()
	marketService := markets.NewService(markets.NewMemoryRepository())
	matchID := primitive.NewObjectID()
	match := matches.Match{
		ID: matchID, Status: matches.StatusLive, Innings: 1,
		ProviderBattingTeamID: 10, ProviderTeamAID: 10,
		TeamAName: "Alpha", TeamBName: "Beta", Format: "T20", ScheduledBalls: 120,
		StateVersion: 2, TradingVersion: 1, HealthySnapshotCount: 2,
		FeedState: matches.FeedStateHealthy,
	}
	store := &Store{markets: marketService}
	if err := store.projectMarkets(ctx, match, reconcile.Projection{}); err != nil {
		t.Fatal(err)
	}

	match.StateVersion++
	match.TradingVersion++
	match.HealthySnapshotCount = 0
	match.FeedState = matches.FeedStateWarming
	match.TradingBlockers = []string{"warming"}
	if err := store.projectMarkets(ctx, match, reconcile.Projection{}); err != nil {
		t.Fatalf("soft sync market gate: %v", err)
	}
	marketList, err := marketService.ListMarketsByMatchID(ctx, matchID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if len(marketList) != 1 || marketList[0].Lifecycle != markets.MarketLifecycleOpen ||
		marketList[0].Status != markets.MarketStatusActive || containsValue(marketList[0].Blockers, "warming") {
		t.Fatalf("market during soft sync must stay tradable = %+v", marketList)
	}
	if !marketService.IsTradable(&marketList[0]) {
		t.Fatal("IsTradable should allow soft-sync markets")
	}

	match.StateVersion++
	match.TradingVersion++
	match.HealthySnapshotCount = 2
	match.FeedState = matches.FeedStateHealthy
	match.TradingBlockers = nil
	if err := store.projectMarkets(ctx, match, reconcile.Projection{}); err != nil {
		t.Fatalf("reopen recovered market: %v", err)
	}
	marketList, err = marketService.ListMarketsByMatchID(ctx, matchID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if marketList[0].Lifecycle != markets.MarketLifecycleOpen || marketList[0].Status != markets.MarketStatusActive || len(marketList[0].Blockers) != 0 {
		t.Fatalf("recovered market = %+v", marketList[0])
	}
}

func TestLiveAdmissionRequiresFutureNotStartedFixture(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	fixture := client.Fixture{Status: "NS"}
	if !futureNotStartedFixture(fixture, now.Add(time.Minute), now) {
		t.Fatal("future not-started fixture was rejected")
	}
	fixture.Status = "1st Innings"
	if futureNotStartedFixture(fixture, now.Add(time.Minute), now) {
		t.Fatal("active fixture was admitted")
	}
	fixture.Status = "NS"
	if futureNotStartedFixture(fixture, now, now) || futureNotStartedFixture(fixture, now.Add(-time.Second), now) {
		t.Fatal("non-future fixture was admitted")
	}
	fixture.Status = "new-provider-phase"
	if futureNotStartedFixture(fixture, now.Add(time.Minute), now) {
		t.Fatal("unknown provider phase was admitted")
	}
}

func TestMidMatchLiveAdmissionRequiresRecentCleanShadowPoll(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	lastSuccess := now.Add(-time.Minute)
	target := &FixtureTarget{
		LastSuccessAt:   &lastSuccess,
		LastSuccessMode: string(client.ModeShadow),
	}
	if !recentlyShadowValidated(target, now) {
		t.Fatal("recent clean shadow poll was rejected")
	}

	target.LastSuccessMode = string(client.ModeLive)
	if recentlyShadowValidated(target, now) {
		t.Fatal("live poll was accepted as shadow validation")
	}
	target.LastSuccessMode = string(client.ModeShadow)
	target.ConsecutiveFailures = 1
	if recentlyShadowValidated(target, now) {
		t.Fatal("failed target was accepted")
	}
	target.LastError = ErrMidMatchPromotion.Error()
	if !recentlyShadowValidated(target, now) {
		t.Fatal("previous mid-match rejection blocked explicit retry")
	}
	target.ConsecutiveFailures = 0
	target.LastError = "provider timeout"
	if recentlyShadowValidated(target, now) {
		t.Fatal("target with last error was accepted")
	}
	target.LastError = ""
	staleSuccess := now.Add(-6 * time.Minute)
	target.LastSuccessAt = &staleSuccess
	if recentlyShadowValidated(target, now) {
		t.Fatal("stale shadow poll was accepted")
	}
}

func TestLiveAdmissionAllowsOnlyProviderLiveOrBreakStates(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	for _, status := range []string{"1st Innings", "2nd Innings", "Innings Break", "Lunch", "Tea", "Int."} {
		if !liveAdmissionAllowed(client.Fixture{Status: status}, true, nil, now) {
			t.Fatalf("status %q was not admitted", status)
		}
	}
	for _, status := range []string{"", "NS", "Not Started", "Delayed", "Postp", "Finished", "Cancl", "Aban", "new-provider-phase"} {
		if liveAdmissionAllowed(client.Fixture{Status: status}, true, nil, now) {
			t.Fatalf("status %q was admitted", status)
		}
	}
	if liveAdmissionAllowed(client.Fixture{Status: "Innings Break"}, false, nil, now) {
		t.Fatal("disabled live admission admitted innings break")
	}
}

func TestFixtureTargetNextPollHandlesReschedulesWithoutRevivingActiveTargets(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	fixture := client.Fixture{Status: "NS"}

	next, apply, replace := fixtureTargetNextPoll(nil, fixture, now.Add(2*time.Hour), now)
	if !apply || !replace || !next.Equal(now.Add(90*time.Minute)) {
		t.Fatalf("new target schedule = %s/%t/%t", next, apply, replace)
	}

	existing := &FixtureTarget{StartTime: now.Add(time.Hour), ProviderStatus: "NS"}
	next, apply, replace = fixtureTargetNextPoll(existing, fixture, now.Add(3*time.Hour), now)
	if !apply || !replace || !next.Equal(now.Add(150*time.Minute)) {
		t.Fatalf("postponed target schedule = %s/%t/%t", next, apply, replace)
	}

	next, apply, replace = fixtureTargetNextPoll(existing, fixture, now.Add(45*time.Minute), now)
	if !apply || replace || !next.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("advanced target schedule = %s/%t/%t", next, apply, replace)
	}

	lastSuccess := now.Add(-time.Minute)
	existing = &FixtureTarget{StartTime: now.Add(time.Hour), ProviderStatus: "1st Innings", LastSuccessAt: &lastSuccess}
	if _, apply, _ = fixtureTargetNextPoll(existing, fixture, now.Add(3*time.Hour), now); apply {
		t.Fatal("fixture sync rescheduled an active target")
	}
}
