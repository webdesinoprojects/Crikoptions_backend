package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/reconcile"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// End-to-end against a real replica set: a rain-shortened fixture carrying a
// whole innings of deliveries must apply in one pass, go LIVE at the reduced
// ball count, and stay untradable. The logged apply duration is the number the
// poll's lease has to outlive — the reason pollTarget now heartbeats its lease.
//
// MONGO_INTEGRATION_URI="mongodb://localhost:27017/?replicaSet=rs0" \
//
//	go test ./internal/sportmonks/store/ -run ReducedOversApply -v
func TestReducedOversApplyEndToEnd(t *testing.T) {
	uri := strings.TrimSpace(os.Getenv("MONGO_INTEGRATION_URI"))
	if uri == "" {
		t.Skip("MONGO_INTEGRATION_URI is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		t.Fatalf("ping primary: %v", err)
	}
	db := client.Database("sm_reduced_" + primitive.NewObjectID().Hex())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = db.Drop(cleanupCtx)
		_ = client.Disconnect(cleanupCtx)
	})

	marketRepository := markets.NewMongoRepository(db)
	store := New(db, markets.NewService(marketRepository))
	if err := store.RequireLiveCapabilities(ctx); err != nil {
		t.Fatalf("Mongo lacks required live capabilities: %v", err)
	}
	if err := marketRepository.EnsureIndexes(ctx); err != nil {
		t.Fatalf("ensure market indexes: %v", err)
	}
	if err := store.EnsureIndexes(ctx); err != nil {
		t.Fatalf("ensure provider indexes: %v", err)
	}

	// Mirrors production fixture 69609: an ODI cut to 47 overs a side.
	const (
		fixtureID  = int64(69609)
		overs      = 47
		totalBalls = overs * 6
		deliveries = 280
		batting    = int64(100)
	)
	base := time.Now().UTC().Truncate(time.Millisecond)

	if _, err := store.fixtures.InsertOne(ctx, FixtureTarget{
		ID: fixtureID, LeagueID: 2, SeasonID: 1731,
		LocalTeamID: batting, VisitorTeamID: 46,
		Format: "ODI", ScheduledBalls: totalBalls, ProviderStatus: "1st Innings",
		StartTime: base, Eligible: true, Supported: true, NextPollAt: base,
		CreatedAt: base, UpdatedAt: base,
	}); err != nil {
		t.Fatalf("seed fixture target: %v", err)
	}

	const owner = "reduced-overs-worker"
	token, claimed, err := store.ClaimTarget(ctx, fixtureID, owner, base, 10*time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim fixture: claimed=%v err=%v", claimed, err)
	}

	projection := reconcile.Projection{
		FixtureID: fixtureID, LeagueID: 2, SeasonID: 1731,
		LocalTeamID: batting, VisitorTeamID: 46,
		LocalTeamName: "Ireland", VisitorTeamName: "Afghanistan",
		StartTime: base.Add(-2 * time.Hour), Format: "ODI",
		ScheduledBalls: totalBalls, ScheduledOvers: overs, ReducedOvers: true,
		ProviderStatus: "1st Innings", Status: matches.StatusLive,
		CurrentInnings: 1, BattingTeamID: batting,
		SnapshotHash: "reduced-overs-e2e",
	}
	for i := 0; i < deliveries; i++ {
		id := fmt.Sprintf("ball-%d", i+1)
		projection.Deliveries = append(projection.Deliveries, reconcile.Delivery{
			ProviderEventID: id, ProviderScoreID: 1,
			ProviderBall: fmt.Sprintf("%d.%d", i/6, i%6+1),
			Innings:      1, Sequence: int64(i + 1), TeamID: batting,
			BatterID: 301, BowlerID: 401,
			TeamRuns: 1, BatterRuns: 1, LegalBall: true, PayloadHash: id + "-v1",
		})
	}
	projection.CurrentScore = deliveries
	projection.LegalBalls = deliveries
	projection.Innings = []reconcile.Innings{{
		Number: 1, BattingTeamID: batting, Runs: deliveries, Wickets: 0,
		LegalBalls: deliveries, ScheduledBalls: totalBalls, SnapshotHash: "innings-v1",
	}}

	if _, err := store.matches.InsertOne(ctx, initialMatch(projection, base)); err != nil {
		t.Fatalf("seed provider admission: %v", err)
	}

	started := time.Now()
	result, err := store.ApplyProjection(ctx, projection, []byte(`{"fixture":"reduced"}`), base.Add(2*time.Second), ApplyOptions{
		Mode: "live", LeaseOwner: owner, LeaseToken: token,
		FeedValidity: time.Minute, RawPayloadTTL: time.Hour,
	})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("apply reduced-overs projection: %v", err)
	}
	if !result.Applied {
		t.Fatalf("projection was not applied: %+v", result)
	}
	t.Logf("first apply of %d deliveries took %s — the lease must outlive this", deliveries, elapsed)

	var match matches.Match
	if err := store.matches.FindOne(ctx,
		bson.M{"provider": ProviderName, "providerFixtureId": fixtureID}).Decode(&match); err != nil {
		t.Fatalf("load match: %v", err)
	}

	if match.Status != matches.StatusLive {
		t.Fatalf("status = %q, want live so the scoreboard is visible", match.Status)
	}
	if match.ScheduledBalls != totalBalls || match.ScheduledOvers != overs || !match.ReducedOvers {
		t.Fatalf("schedule = %d balls / %d overs / reduced=%v, want %d/%d/true",
			match.ScheduledBalls, match.ScheduledOvers, match.ReducedOvers, totalBalls, overs)
	}
	if match.CurrentScore != deliveries {
		t.Fatalf("score = %d, want %d", match.CurrentScore, deliveries)
	}
	if match.BallsLeft != totalBalls-deliveries {
		t.Fatalf("ballsLeft = %d, want %d", match.BallsLeft, totalBalls-deliveries)
	}
	if match.TradingState != "blocked" || !matches.HasHardTradingBlockers(match.TradingBlockers) {
		t.Fatalf("trading = %q %v, want blocked with a hard blocker",
			match.TradingState, match.TradingBlockers)
	}
	if matches.IsTradable(&match) {
		t.Fatal("a reduced-overs match must not be tradable")
	}

	events, err := store.events.CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	t.Logf("persisted %d delivery events in the first apply", events)
	if events != int64(deliveries) {
		t.Fatalf("events = %d, want %d", events, deliveries)
	}

	// The lease must still be ours after an apply this large, otherwise the
	// poll result is discarded and the fixture retries forever.
	if err := store.CompleteTargetPoll(ctx, fixtureID, owner, token, "live",
		projection.SnapshotHash, projection.ProviderStatus,
		base.Add(3*time.Second), base.Add(5*time.Second)); err != nil {
		t.Fatalf("complete poll after a long apply: %v", err)
	}
}
