package worker

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/reconcile"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/store"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Only the snapshot method is used. No test makes an HTTP/provider request.
type validityProvider struct {
	Provider
	mini client.MiniScore
	err  error
}

func (p *validityProvider) SnapshotEndpoint() string { return "test/match/live" }

func (p *validityProvider) MatchSnapshot(_ context.Context, fixture client.Fixture) (client.Snapshot, client.RateLimit, error) {
	return client.Snapshot{
		MatchID: fixture.ID, Fixture: fixture,
		Commentary: client.CommentaryData{MiniScore: p.mini},
	}, client.RateLimit{}, p.err
}

type validityHarness struct {
	t        *testing.T
	ctx      context.Context
	db       *mongo.Database
	store    *store.Store
	worker   *Worker
	provider *validityProvider
	now      time.Time
	matchID  primitive.ObjectID
	fixture  int64
}

func newValidityHarness(t *testing.T) *validityHarness {
	t.Helper()
	uri := strings.TrimSpace(os.Getenv("MONGO_INTEGRATION_URI"))
	if uri == "" {
		t.Skip("MONGO_INTEGRATION_URI is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	mongoClient, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	db := mongoClient.Database("fix_a_it_" + primitive.NewObjectID().Hex())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if err := db.Drop(cleanupCtx); err != nil {
			t.Error(err)
		}
		if err := mongoClient.Disconnect(cleanupCtx); err != nil {
			t.Error(err)
		}
	})
	marketRepo := markets.NewMongoRepository(db)
	marketService := markets.NewService(marketRepo)
	feedStore := store.New(db, marketService)
	if err := feedStore.RequireLiveCapabilities(ctx); err != nil {
		t.Fatal(err)
	}
	if err := marketRepo.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	if err := feedStore.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	provider := &validityProvider{mini: client.MiniScore{
		State: client.StateComplete, CustomStatus: "Sydney Sixers Women won by 15 runs",
		MatchFormat: "T20", InningsID: 2, BatTeamID: 202,
		BatTeamScore: 134, BatTeamWickets: 7, Overs: 20,
		InningsScores: []client.InningsScore{
			{InningsID: 1, BatTeamID: 101, Score: 149, Wickets: 8, Overs: 20},
			{InningsID: 2, BatTeamID: 202, Score: 134, Wickets: 7, Overs: 20},
		},
	}}
	cfg := client.Config{
		Mode: client.ModeLive, HTTPTimeout: 15 * time.Second,
		MinPollInterval: 6 * time.Second, MaxPollInterval: 10 * time.Second,
		BreakInterval: time.Minute, FinalizingInterval: 15 * time.Second,
		StaleMinimum: time.Minute, InningsFinalizationHold: time.Minute,
		MatchFinalizationHold: 2 * time.Minute, LeaseTTL: 30 * time.Second,
		HourlyRequestLimit: 900, DailyRequestLimit: 5000, MaxConcurrency: 1,
		AllowLiveCorrections: true,
	}
	w, err := New(cfg, provider, feedStore, "fix-a-test", discardLogger{})
	if err != nil {
		t.Fatal(err)
	}
	h := &validityHarness{
		t: t, ctx: ctx, db: db, store: feedStore, worker: w, provider: provider,
		now: time.Now().UTC().Truncate(time.Millisecond), matchID: primitive.NewObjectID(), fixture: 1000000000492985,
	}
	w.now = func() time.Time { return h.now }
	_, err = db.Collection("provider_fixtures").InsertOne(ctx, store.FixtureTarget{
		ID: h.fixture, LeagueID: 77, SeasonID: 77, LocalTeamID: 101, VisitorTeamID: 202,
		LocalTeamShort: "SSW", VisitorTeamShort: "PSW", Format: "T20", ScheduledBalls: 120,
		ProviderStatus: client.StateComplete, StartTime: h.now.Add(-3 * time.Hour),
		Eligible: true, Supported: true, ParkedReason: store.ParkReasonAbsentFromLive,
		NextPollAt: h.now, CreatedAt: h.now, UpdatedAt: h.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Collection("matches").InsertOne(ctx, matches.Match{
		ID: h.matchID, Provider: store.ProviderName, DataSource: store.ProviderName,
		ProviderFixtureID: h.fixture, ProviderLeagueID: 77, ProviderSeasonID: 77,
		ProviderTeamAID: 101, ProviderTeamBID: 202,
		TeamAName: "SSW", TeamBName: "PSW", Format: "T20", ScheduledBalls: 120,
		StartTime: h.now.Add(-3 * time.Hour), Status: matches.StatusLive,
		Innings: 2, StateVersion: 1, TradingVersion: 1,
		FeedState: matches.FeedStateHealthy, TradingState: "open", HealthySnapshotCount: 1,
		CreatedAt: h.now, UpdatedAt: h.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	for innings := 1; innings <= 2; innings++ {
		if err := marketService.EnsureProviderInningsMarket(ctx, markets.ProviderInningsMarketSpec{
			MatchID: h.matchID.Hex(), Innings: innings, BattingTeamName: "Test team",
			Format: "T20", ScheduledBalls: 120, StateVersion: 1, TradingVersion: 1,
			FeedState: matches.FeedStateHealthy,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *validityHarness) target() store.FixtureTarget {
	h.t.Helper()
	var target store.FixtureTarget
	if err := h.db.Collection("provider_fixtures").FindOne(h.ctx, bson.M{"_id": h.fixture}).Decode(&target); err != nil {
		h.t.Fatal(err)
	}
	return target
}

func (h *validityHarness) match() matches.Match {
	h.t.Helper()
	var match matches.Match
	if err := h.db.Collection("matches").FindOne(h.ctx, bson.M{"_id": h.matchID}).Decode(&match); err != nil {
		h.t.Fatal(err)
	}
	return match
}

func (h *validityHarness) poll() matches.Match {
	h.t.Helper()
	target := h.target()
	token, claimed, err := h.store.ClaimTarget(h.ctx, h.fixture, h.worker.owner, h.now, h.worker.cfg.LeaseTTL)
	if err != nil || !claimed {
		h.t.Fatalf("claim target: claimed=%t err=%v", claimed, err)
	}
	h.worker.pollTarget(h.ctx, target, token, 10*time.Second)
	if h.provider.err == nil {
		target = h.target()
		if target.LastSuccessAt == nil || !target.LastSuccessAt.Equal(h.now) || target.ConsecutiveFailures != 0 {
			h.t.Fatalf("successful poll was not committed: %+v", target)
		}
	}
	return h.match()
}

func (h *validityHarness) expire(at time.Time, want int) {
	h.t.Helper()
	got, err := h.store.ExpireStaleFeeds(h.ctx, at, 100)
	if err != nil || got != want {
		h.t.Fatalf("watchdog at %s: expired=%d want=%d err=%v", at, got, want, err)
	}
}

func (h *validityHarness) jobs(want int64) {
	h.t.Helper()
	got, err := h.db.Collection("settlement_jobs").CountDocuments(h.ctx, bson.M{"matchId": h.matchID.Hex()})
	if err != nil || got != want {
		h.t.Fatalf("settlement jobs=%d want=%d err=%v", got, want, err)
	}
}

func assertValidityCandidates(t *testing.T, match matches.Match, polls int) {
	t.Helper()
	if match.FinalCandidate == nil || match.FinalCandidate.IdenticalPolls != polls {
		t.Fatalf("match candidate=%+v, want %d polls", match.FinalCandidate, polls)
	}
	for _, innings := range match.InningsSummaries {
		if innings.FinalCandidate == nil || innings.FinalCandidate.IdenticalPolls != polls {
			t.Fatalf("innings %d candidate=%+v, want %d polls", innings.Innings, innings.FinalCandidate, polls)
		}
		if innings.SettlementReady != (polls >= 3) {
			t.Fatalf("innings %d ready=%t at poll %d", innings.Innings, innings.SettlementReady, polls)
		}
	}
}

func TestAbsentFinalizationSurvivesWatchdogBetweenPolls(t *testing.T) {
	h := newValidityHarness(t)
	if oldValidity := feedValidityForInterval(h.worker.cfg, 15*time.Second); oldValidity != 85*time.Second {
		t.Fatalf("regression setup: old validity=%s, want 85s", oldValidity)
	}
	var firstHash string
	for poll := 1; poll <= 3; poll++ {
		match := h.poll()
		assertValidityCandidates(t, match, poll)
		if poll == 1 {
			firstHash = match.LastSnapshotHash
		} else if match.LastSnapshotHash != firstHash {
			t.Fatal("the regression snapshots must remain identical")
		}
		if poll == 3 {
			if validity := match.FeedValidUntil.Sub(h.now); validity > time.Hour {
				t.Fatalf("terminal parking leaked into feed validity: %s", validity)
			}
			if match.Status != matches.StatusCompleted || match.FeedState != matches.FeedStateTerminal {
				t.Fatalf("final state=%s/%s", match.Status, match.FeedState)
			}
			h.jobs(2)
			if next := h.target().NextPollAt; next.Sub(h.now) < 300*24*time.Hour {
				t.Fatalf("terminal target was not parked: %s", next)
			}
			break
		}
		if match.Status != matches.StatusInningsBreak || match.FeedState != matches.FeedStateFinalizing {
			t.Fatalf("premature closure: %s/%s", match.Status, match.FeedState)
		}
		h.jobs(0)
		next := h.target().NextPollAt
		if next.Sub(h.now) != 5*time.Minute {
			t.Fatalf("absence schedule=%s, want 5m", next.Sub(h.now))
		}
		for tick := h.now.Add(5 * time.Second); !tick.After(next); tick = tick.Add(5 * time.Second) {
			h.expire(tick, 0)
		}
		assertValidityCandidates(t, h.match(), poll)
		if got := match.FeedValidUntil.Sub(h.now); got != 20*time.Minute+25*time.Second {
			t.Fatalf("validity=%s, want existing 4-interval margin applied to 5m", got)
		}
		h.now = next
	}
}

func TestAbsentFinalizationStillResetsOnOutage(t *testing.T) {
	t.Run("missed deadline", func(t *testing.T) {
		h := newValidityHarness(t)
		match := h.poll()
		h.expire(match.FeedValidUntil.Add(-time.Millisecond), 0)
		h.expire(match.FeedValidUntil.Add(5*time.Second), 1)
		assertValidityReset(t, h.match())
		h.jobs(0)
	})
	t.Run("failed poll before deadline", func(t *testing.T) {
		h := newValidityHarness(t)
		match := h.poll()
		h.now = h.target().NextPollAt
		if !h.now.Before(*match.FeedValidUntil) {
			t.Fatal("failure must be tested inside the freshness allowance")
		}
		h.provider.err = errors.New("test provider unavailable")
		failed := h.poll()
		if failed.FinalCandidate != nil {
			t.Fatal("failed poll retained match finalization")
		}
		for _, innings := range failed.InningsSummaries {
			if innings.FinalCandidate != nil || innings.SettlementReady {
				t.Fatalf("failed poll retained innings finalization: %+v", innings)
			}
		}
		if h.target().ConsecutiveFailures != 1 {
			t.Fatal("poll failure was not recorded")
		}
		h.jobs(0)
	})
}

func assertValidityReset(t *testing.T, match matches.Match) {
	t.Helper()
	if match.FeedState != matches.FeedStateStale || match.TradingState != "blocked" || match.FinalCandidate != nil {
		t.Fatalf("outage did not stale/reset the match: %+v", match)
	}
	for _, innings := range match.InningsSummaries {
		if innings.FinalCandidate != nil || innings.SettlementReady {
			t.Fatalf("outage retained innings finalization: %+v", innings)
		}
	}
}

func TestAbsentFinalizationCorrectionRestartsHold(t *testing.T) {
	h := newValidityHarness(t)
	h.poll()
	h.now = h.target().NextPollAt
	assertValidityCandidates(t, h.poll(), 2)
	h.jobs(0)
	h.provider.mini.BatTeamScore = 135
	h.provider.mini.InningsScores[1].Score = 135
	h.provider.mini.CustomStatus = "Sydney Sixers Women won by 14 runs"
	h.now = h.target().NextPollAt
	corrected := h.poll()
	if corrected.FinalCandidate == nil || corrected.FinalCandidate.IdenticalPolls != 1 || corrected.FeedState != matches.FeedStateFinalizing {
		t.Fatalf("correction did not restart match hold: %+v", corrected.FinalCandidate)
	}
	if corrected.InningsSummaries[1].FinalCandidate == nil || corrected.InningsSummaries[1].FinalCandidate.IdenticalPolls != 1 || corrected.InningsSummaries[1].SettlementReady {
		t.Fatalf("correction did not restart chase hold: %+v", corrected.InningsSummaries[1])
	}
	if !corrected.InningsSummaries[0].SettlementReady {
		t.Fatal("unchanged first innings unnecessarily restarted its hold")
	}
	h.jobs(1) // Only the unchanged first innings is ready.
}

func TestPollValidityPreservesLiveAndBreakSchedules(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      string
		overs      client.FlexFloat
		quiet      bool
		wantFeed   string
		wantStatus string
		interval   time.Duration
	}{
		{"live", client.StateInProgress, 10, false, matches.FeedStateHealthy, matches.StatusLive, 10 * time.Second},
		{"quiet live", client.StateInProgress, 10, true, matches.FeedStateHealthy, matches.StatusLive, time.Minute},
		{"innings break", client.StateInnings, 10, false, matches.FeedStateHealthy, matches.StatusInningsBreak, time.Minute},
		{"live completed innings", client.StateInProgress, 20, false, matches.FeedStateFinalizing, matches.StatusInningsBreak, 15 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newValidityHarness(t)
			h.provider.mini.State = tc.state
			h.provider.mini.CustomStatus = "Play paused"
			h.provider.mini.Overs = tc.overs
			h.provider.mini.InningsScores[1].Overs = tc.overs
			if _, err := h.db.Collection("provider_fixtures").UpdateByID(h.ctx, h.fixture, bson.M{
				"$unset": bson.M{"parkedReason": ""},
			}); err != nil {
				t.Fatal(err)
			}
			if tc.quiet {
				snapshot, _, _ := h.provider.MatchSnapshot(h.ctx, fixtureFromTarget(h.target()))
				projection, err := reconcile.ReduceSnapshot(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.db.Collection("provider_fixtures").UpdateByID(h.ctx, h.fixture, bson.M{
					"$set": bson.M{"lastSnapshotHash": projection.SnapshotHash, "unchangedSince": h.now.Add(-10 * time.Minute)},
				}); err != nil {
					t.Fatal(err)
				}
			}
			match := h.poll()
			if match.FeedState != tc.wantFeed || match.Status != tc.wantStatus {
				t.Fatalf("state=%s/%s, want %s/%s", match.Status, match.FeedState, tc.wantStatus, tc.wantFeed)
			}
			if tc.wantStatus == matches.StatusLive && match.TradingState != "open" {
				t.Fatalf("healthy live trading=%s, want open", match.TradingState)
			}
			if tc.wantStatus == matches.StatusInningsBreak && match.TradingState != "blocked" {
				t.Fatalf("break trading=%s, want blocked", match.TradingState)
			}
			next := h.target().NextPollAt
			gap := next.Sub(h.now)
			if gap < tc.interval*9/10 || gap > tc.interval*11/10 {
				t.Fatalf("next poll in %s, want %s +/-10%%", gap, tc.interval)
			}
			if !match.FeedValidUntil.After(next.Add(h.worker.cfg.HTTPTimeout)) {
				t.Fatalf("deadline %s does not cover scheduled poll %s", match.FeedValidUntil, next)
			}
			h.expire(next, 0)
		})
	}
}

func TestAbandonmentPendingHoldDoesNotGetTerminalValidity(t *testing.T) {
	h := newValidityHarness(t)
	h.provider.mini.State = client.StateAbandon
	h.provider.mini.CustomStatus = "Match abandoned"
	if _, err := h.db.Collection("matches").UpdateByID(h.ctx, h.matchID, bson.M{
		"$set": bson.M{"inningsSummaries": []matches.InningsSummary{{
			Innings: 1, BattingTeamID: 101, Runs: 148, Complete: true, SettlementReady: true,
			FinalCandidate: &matches.FinalCandidate{Revision: 1, SnapshotHash: "prior-score", IdenticalPolls: 3},
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	match := h.poll()
	if match.Status != matches.StatusAbandoned || match.FeedState != matches.FeedStateFinalizing {
		t.Fatalf("state=%s/%s, want abandonment with pending correction hold", match.Status, match.FeedState)
	}
	if gap := h.target().NextPollAt.Sub(h.now); gap != 5*time.Minute {
		t.Fatalf("pending abandonment next poll in %s, want 5m", gap)
	}
	if validity := match.FeedValidUntil.Sub(h.now); validity != 20*time.Minute+25*time.Second {
		t.Fatalf("pending abandonment validity=%s, must not inherit year-long terminal schedule", validity)
	}
	h.expire(match.FeedValidUntil.Add(5*time.Second), 1)
	assertValidityReset(t, h.match())
}
