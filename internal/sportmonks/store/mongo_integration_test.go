package store

import (
	"context"
	"errors"
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

func TestMongoLiveProjectionTransaction(t *testing.T) {
	uri := strings.TrimSpace(os.Getenv("MONGO_INTEGRATION_URI"))
	if uri == "" {
		t.Skip("MONGO_INTEGRATION_URI is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect to Mongo replica set: %v", err)
	}
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		t.Fatalf("ping Mongo primary: %v", err)
	}

	db := client.Database("sm_it_" + primitive.NewObjectID().Hex())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if err := db.Drop(cleanupCtx); err != nil {
			t.Errorf("drop integration database: %v", err)
		}
		if err := client.Disconnect(cleanupCtx); err != nil {
			t.Errorf("disconnect integration client: %v", err)
		}
	})

	marketRepository := markets.NewMongoRepository(db)
	marketService := markets.NewService(marketRepository)
	store := New(db, marketService)
	if err := store.RequireLiveCapabilities(ctx); err != nil {
		t.Fatalf("Mongo lacks required live capabilities: %v", err)
	}
	if err := marketRepository.EnsureIndexes(ctx); err != nil {
		t.Fatalf("ensure market indexes: %v", err)
	}
	if err := store.EnsureIndexes(ctx); err != nil {
		t.Fatalf("ensure provider indexes: %v", err)
	}

	const fixtureID int64 = 910000001
	base := time.Now().UTC().Truncate(time.Millisecond)
	_, err = store.fixtures.InsertOne(ctx, FixtureTarget{
		ID: fixtureID, LeagueID: 77, SeasonID: 88,
		LocalTeamID: 101, VisitorTeamID: 202,
		Format: "T20", ScheduledBalls: 120, ProviderStatus: "1st Innings",
		StartTime: base, Eligible: true, Supported: true, NextPollAt: base,
		CreatedAt: base, UpdatedAt: base,
	})
	if err != nil {
		t.Fatalf("seed fixture target: %v", err)
	}

	const firstOwner = "integration-worker-a"
	firstToken, claimed, err := store.ClaimTarget(ctx, fixtureID, firstOwner, base, 10*time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim fixture: claimed=%v err=%v", claimed, err)
	}

	projection := integrationProjection(fixtureID)
	admitted := initialMatch(projection, base)
	if _, err := store.matches.InsertOne(ctx, admitted); err != nil {
		t.Fatalf("seed pre-match provider admission: %v", err)
	}
	applyConfig := ApplyOptions{
		Mode: "live", LeaseOwner: firstOwner, LeaseToken: firstToken,
		AllowCorrections: true, FeedValidity: time.Minute, RawPayloadTTL: time.Hour,
	}

	invalid := projection
	invalid.Format = "unsupported"
	invalid.SnapshotHash = "invalid-format"
	if _, err := store.ApplyProjection(ctx, invalid, []byte(`{"fixture":"invalid"}`), base.Add(time.Second), applyConfig); err == nil {
		t.Fatal("invalid market contract unexpectedly committed")
	}
	assertAuthoritativeCounts(t, ctx, db, authoritativeCounts{matches: 1})
	assertApplyGeneration(t, ctx, store.fixtures, fixtureID, 0)

	result, err := store.ApplyProjection(ctx, projection, []byte(`{"fixture":"valid"}`), base.Add(2*time.Second), applyConfig)
	if err != nil {
		t.Fatalf("apply valid projection: %v", err)
	}
	if !result.Applied || result.MatchID == "" || result.StateVersion != 1 {
		t.Fatalf("unexpected apply result: %+v", result)
	}

	want := authoritativeCounts{
		matches: 1, events: 1, revisions: 1, markets: 1,
		marketSnapshots: 1, outbox: 2,
	}
	assertAuthoritativeCounts(t, ctx, db, want)
	assertApplyGeneration(t, ctx, store.fixtures, fixtureID, 1)
	assertCommittedProjection(t, ctx, db, result, projection)

	if err := store.CompleteTargetPoll(
		ctx, fixtureID, firstOwner, firstToken, "live", projection.SnapshotHash,
		projection.ProviderStatus, base.Add(3*time.Second), base.Add(3*time.Second),
	); err != nil {
		t.Fatalf("complete first fixture lease: %v", err)
	}

	const secondOwner = "integration-worker-b"
	secondToken, claimed, err := store.ClaimTarget(ctx, fixtureID, secondOwner, base.Add(4*time.Second), 10*time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim replacement fixture lease: claimed=%v err=%v", claimed, err)
	}

	changed := projection
	changed.CurrentScore = 5
	changed.LegalBalls = 2
	changed.SnapshotHash = "snapshot-v2"
	changed.Innings = append([]reconcile.Innings(nil), projection.Innings...)
	changed.Innings[0].Runs = 5
	changed.Innings[0].LegalBalls = 2
	changed.Deliveries = append(append([]reconcile.Delivery(nil), projection.Deliveries...), reconcile.Delivery{
		ProviderEventID: "ball-2", ProviderScoreID: 1, ProviderBall: "0.2",
		Innings: 1, Sequence: 2, TeamID: 101, BatterID: 302, BowlerID: 401,
		TeamRuns: 1, BatterRuns: 1, LegalBall: true, PayloadHash: "ball-2-v1",
	})

	rejected := []ApplyOptions{
		{Mode: "live", LeaseOwner: secondOwner, LeaseToken: "wrong-token", AllowCorrections: true},
		{Mode: "live", LeaseOwner: firstOwner, LeaseToken: firstToken, AllowCorrections: true},
	}
	for i, cfg := range rejected {
		_, err := store.ApplyProjection(ctx, changed, []byte(fmt.Sprintf(`{"rejected":%d}`, i)), base.Add(time.Duration(5+i)*time.Second), cfg)
		if !errors.Is(err, ErrFixtureLeaseLost) {
			t.Fatalf("rejected apply %d: got %v, want ErrFixtureLeaseLost", i, err)
		}
	}

	assertAuthoritativeCounts(t, ctx, db, want)
	assertApplyGeneration(t, ctx, store.fixtures, fixtureID, 1)
	assertCommittedProjection(t, ctx, db, result, projection)

	var target FixtureTarget
	if err := store.fixtures.FindOne(ctx, bson.M{"_id": fixtureID}).Decode(&target); err != nil {
		t.Fatalf("read claimed fixture: %v", err)
	}
	if target.LeaseOwner != secondOwner || target.LeaseToken != secondToken {
		t.Fatalf("rejected apply changed active lease: owner=%q token=%q", target.LeaseOwner, target.LeaseToken)
	}

	// Raw payload diagnostics intentionally survive failed transactions, but
	// only the latest payload is retained for each fixture/mode pair.
	assertCollectionCount(t, ctx, db.Collection("provider_payloads"), 1)
}

func integrationProjection(fixtureID int64) reconcile.Projection {
	start := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	return reconcile.Projection{
		FixtureID: fixtureID, LeagueID: 77, SeasonID: 88,
		LocalTeamID: 101, VisitorTeamID: 202,
		LocalTeamName: "Alpha", VisitorTeamName: "Beta",
		StartTime: start, Format: "T20", ScheduledBalls: 120,
		ProviderStatus: "1st Innings", Status: matches.StatusLive,
		CurrentInnings: 1, BattingTeamID: 101,
		CurrentScore: 4, Wickets: 0, LegalBalls: 1,
		Innings: []reconcile.Innings{{
			Number: 1, BattingTeamID: 101, Runs: 4, Wickets: 0,
			LegalBalls: 1, ScheduledBalls: 120, SnapshotHash: "innings-v1",
		}},
		Deliveries: []reconcile.Delivery{{
			ProviderEventID: "ball-1", ProviderScoreID: 4, ProviderBall: "0.1",
			Innings: 1, Sequence: 1, TeamID: 101, BatterID: 301, BowlerID: 401,
			TeamRuns: 4, BatterRuns: 4, LegalBall: true, PayloadHash: "ball-1-v1",
		}},
		SnapshotHash: "snapshot-v1",
	}
}

type authoritativeCounts struct {
	matches         int64
	events          int64
	revisions       int64
	markets         int64
	marketSnapshots int64
	outbox          int64
	gateJobs        int64
}

func assertAuthoritativeCounts(t *testing.T, ctx context.Context, db *mongo.Database, want authoritativeCounts) {
	t.Helper()
	collections := []struct {
		name string
		want int64
	}{
		{"matches", want.matches},
		{"match_events", want.events},
		{"match_event_revisions", want.revisions},
		{"markets", want.markets},
		{"market_snapshots", want.marketSnapshots},
		{"realtime_outbox", want.outbox},
		{"trading_gate_jobs", want.gateJobs},
	}
	for _, collection := range collections {
		assertCollectionCount(t, ctx, db.Collection(collection.name), collection.want)
	}
}

func assertCollectionCount(t *testing.T, ctx context.Context, collection *mongo.Collection, want int64) {
	t.Helper()
	got, err := collection.CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatalf("count %s: %v", collection.Name(), err)
	}
	if got != want {
		t.Fatalf("%s count=%d, want %d", collection.Name(), got, want)
	}
}

func assertApplyGeneration(t *testing.T, ctx context.Context, fixtures *mongo.Collection, fixtureID, want int64) {
	t.Helper()
	var row struct {
		ApplyGeneration int64 `bson:"applyGeneration"`
	}
	if err := fixtures.FindOne(ctx, bson.M{"_id": fixtureID}).Decode(&row); err != nil {
		t.Fatalf("read fixture apply generation: %v", err)
	}
	if row.ApplyGeneration != want {
		t.Fatalf("apply generation=%d, want %d", row.ApplyGeneration, want)
	}
}

func assertCommittedProjection(t *testing.T, ctx context.Context, db *mongo.Database, result ApplyResult, projection reconcile.Projection) {
	t.Helper()
	matchID, err := primitive.ObjectIDFromHex(result.MatchID)
	if err != nil {
		t.Fatalf("parse result match ID: %v", err)
	}
	var match matches.Match
	if err := db.Collection("matches").FindOne(ctx, bson.M{"_id": matchID}).Decode(&match); err != nil {
		t.Fatalf("read committed match: %v", err)
	}
	if match.StateVersion != result.StateVersion || match.LastSnapshotHash != projection.SnapshotHash || match.CurrentScore != projection.CurrentScore {
		t.Fatalf("committed match does not match projection: %+v", match)
	}

	var event matches.BallEvent
	if err := db.Collection("match_events").FindOne(ctx, bson.M{"providerEventId": "ball-1"}).Decode(&event); err != nil {
		t.Fatalf("read committed delivery: %v", err)
	}
	if event.MatchID != result.MatchID || event.Revision != 1 || event.PayloadHash != "ball-1-v1" {
		t.Fatalf("unexpected committed delivery: %+v", event)
	}

	var market markets.Market
	if err := db.Collection("markets").FindOne(ctx, bson.M{"matchId": result.MatchID}).Decode(&market); err != nil {
		t.Fatalf("read committed market: %v", err)
	}
	if market.Kind != markets.MarketKindInningsScore || market.Innings != 1 || market.FormulaVersion != markets.FormulaVersionInningsScoreV1 {
		t.Fatalf("unexpected committed market: %+v", market)
	}

	var snapshot MarketSnapshot
	if err := db.Collection("market_snapshots").FindOne(ctx, bson.M{"matchId": result.MatchID}).Decode(&snapshot); err != nil {
		t.Fatalf("read committed market snapshot: %v", err)
	}
	if snapshot.MarketID != market.ID.Hex() || snapshot.StateVersion != result.StateVersion || snapshot.ProviderSnapshotID != projection.SnapshotHash {
		t.Fatalf("unexpected market snapshot: %+v", snapshot)
	}

	matchOutbox, err := db.Collection("realtime_outbox").CountDocuments(ctx, bson.M{
		"matchId": result.MatchID, "type": "match.state", "stateVersion": result.StateVersion,
	})
	if err != nil || matchOutbox != 1 {
		t.Fatalf("match outbox count=%d err=%v", matchOutbox, err)
	}
	deliveryOutbox, err := db.Collection("realtime_outbox").CountDocuments(ctx, bson.M{
		"matchId": result.MatchID, "type": "match.delivery", "sequence": int64(1),
	})
	if err != nil || deliveryOutbox != 1 {
		t.Fatalf("delivery outbox count=%d err=%v", deliveryOutbox, err)
	}
}
