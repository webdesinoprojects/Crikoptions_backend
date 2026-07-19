package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/reconcile"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

type Store struct {
	db          *mongo.Database
	matches     *mongo.Collection
	events      *mongo.Collection
	revisions   *mongo.Collection
	outbox      *mongo.Collection
	leagues     *mongo.Collection
	fixtures    *mongo.Collection
	catalog     *mongo.Collection
	payloads    *mongo.Collection
	settlements *mongo.Collection
	gateJobs    *mongo.Collection
	controls    *mongo.Collection
	shadow      *mongo.Collection
	reports     *mongo.Collection
	marketSnaps *mongo.Collection
	quota       *mongo.Collection
	schedules   *mongo.Collection
	incidents   *mongo.Collection
	markets     MarketProjector
}

func New(db *mongo.Database, marketProjector MarketProjector) *Store {
	return &Store{
		db: db, matches: db.Collection("matches"), events: db.Collection("match_events"),
		revisions: db.Collection("match_event_revisions"), outbox: db.Collection("realtime_outbox"),
		leagues: db.Collection("provider_leagues"), fixtures: db.Collection("provider_fixtures"),
		catalog: db.Collection("provider_score_catalog"), payloads: db.Collection("provider_payloads"),
		settlements: db.Collection("settlement_jobs"),
		gateJobs:    db.Collection("trading_gate_jobs"),
		controls:    db.Collection("provider_controls"),
		shadow:      db.Collection("shadow_projections"),
		reports:     db.Collection("reconciliation_reports"),
		marketSnaps: db.Collection("market_snapshots"),
		quota:       db.Collection("provider_request_quota"),
		schedules:   db.Collection("provider_scheduler_leases"),
		incidents:   db.Collection("provider_incidents"),
		markets:     marketProjector,
	}
}

func (s *Store) EnsureIndexes(ctx context.Context) error {
	sets := []struct {
		collection *mongo.Collection
		indexes    []mongo.IndexModel
	}{
		{s.matches, []mongo.IndexModel{{
			Keys: bson.D{{Key: "provider", Value: 1}, {Key: "providerFixtureId", Value: 1}},
			Options: options.Index().SetUnique(true).SetPartialFilterExpression(bson.M{
				"provider": bson.M{"$type": "string"}, "providerFixtureId": bson.M{"$type": "long"},
			}),
		}}},
		{s.events, []mongo.IndexModel{
			{
				Keys: bson.D{{Key: "provider", Value: 1}, {Key: "providerFixtureId", Value: 1}, {Key: "providerEventId", Value: 1}},
				Options: options.Index().SetUnique(true).SetPartialFilterExpression(bson.M{
					"provider": bson.M{"$type": "string"}, "providerFixtureId": bson.M{"$type": "long"},
					"providerEventId": bson.M{"$type": "string"},
				}),
			},
			{Keys: bson.D{{Key: "matchId", Value: 1}, {Key: "innings", Value: 1}, {Key: "sequence", Value: 1}}},
		}},
		{s.revisions, []mongo.IndexModel{{
			Keys:    bson.D{{Key: "provider", Value: 1}, {Key: "providerFixtureId", Value: 1}, {Key: "providerEventId", Value: 1}, {Key: "revision", Value: 1}},
			Options: options.Index().SetUnique(true),
		}}},
		{s.outbox, []mongo.IndexModel{
			{Keys: bson.D{{Key: "eventId", Value: 1}}, Options: options.Index().SetUnique(true)},
			{Keys: bson.D{{Key: "createdAt", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(24 * 60 * 60)},
		}},
		{s.leagues, []mongo.IndexModel{{Keys: bson.D{{Key: "enabled", Value: 1}, {Key: "entitled", Value: 1}}}}},
		{s.fixtures, []mongo.IndexModel{
			{Keys: bson.D{{Key: "eligible", Value: 1}, {Key: "nextPollAt", Value: 1}, {Key: "startTime", Value: 1}}},
			{Keys: bson.D{{Key: "leagueId", Value: 1}, {Key: "startTime", Value: 1}}},
		}},
		{s.payloads, []mongo.IndexModel{{Keys: bson.D{{Key: "expiresAt", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(0)}}},
		{s.settlements, []mongo.IndexModel{{Keys: bson.D{{Key: "status", Value: 1}, {Key: "nextAttemptAt", Value: 1}, {Key: "leaseUntil", Value: 1}, {Key: "createdAt", Value: 1}}}}},
		{s.gateJobs, []mongo.IndexModel{{Keys: bson.D{{Key: "status", Value: 1}, {Key: "nextAttemptAt", Value: 1}, {Key: "leaseUntil", Value: 1}, {Key: "createdAt", Value: 1}}}}},
		{s.reports, []mongo.IndexModel{{Keys: bson.D{{Key: "fixtureId", Value: 1}, {Key: "receivedAt", Value: -1}}}}},
		{s.marketSnaps, []mongo.IndexModel{{Keys: bson.D{{Key: "matchId", Value: 1}, {Key: "stateVersion", Value: 1}, {Key: "tradingVersion", Value: 1}}}}},
		{s.quota, []mongo.IndexModel{{Keys: bson.D{{Key: "expiresAt", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(0)}}},
		{s.schedules, []mongo.IndexModel{{Keys: bson.D{{Key: "leaseUntil", Value: 1}}}}},
		{s.incidents, []mongo.IndexModel{{Keys: bson.D{{Key: "status", Value: 1}, {Key: "createdAt", Value: -1}}}}},
	}
	for _, set := range sets {
		if _, err := set.collection.Indexes().CreateMany(ctx, set.indexes); err != nil {
			return fmt.Errorf("ensure %s indexes: %w", set.collection.Name(), err)
		}
	}
	return nil
}

func (s *Store) ConsumeRequestQuota(ctx context.Context, endpoint string, now time.Time, hourlyLimit, reservePercent int) (bool, error) {
	if strings.TrimSpace(endpoint) == "" || hourlyLimit <= 0 || reservePercent < 0 || reservePercent >= 100 {
		return false, errors.New("invalid provider quota request")
	}
	usable := hourlyLimit * (100 - reservePercent) / 100
	if usable < 1 {
		usable = 1
	}
	bucket := now.UTC().Truncate(time.Hour)
	id := endpoint + ":" + bucket.Format("2006010215")
	result := s.quota.FindOneAndUpdate(ctx, bson.M{
		"_id": id, "count": bson.M{"$lt": usable},
	}, bson.M{
		"$inc": bson.M{"count": 1},
		"$setOnInsert": bson.M{
			"endpoint": endpoint, "bucketStart": bucket,
			"expiresAt": bucket.Add(2 * time.Hour),
		},
	}, options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After))
	var row struct{ Count int }
	if err := result.Decode(&row); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return false, nil
		}
		return false, err
	}
	return row.Count <= usable, nil
}

func (s *Store) ClaimSchedule(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(owner) == "" || ttl <= 0 {
		return false, errors.New("invalid provider scheduler lease")
	}
	result := s.schedules.FindOneAndUpdate(ctx, bson.M{
		"_id": name,
		"$or": bson.A{
			bson.M{"leaseUntil": bson.M{"$exists": false}},
			bson.M{"leaseUntil": bson.M{"$lte": now.UTC()}},
			bson.M{"leaseOwner": owner},
		},
	}, bson.M{"$set": bson.M{
		"leaseOwner": owner, "leaseUntil": now.UTC().Add(ttl), "updatedAt": now.UTC(),
	}}, options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After))
	var row bson.M
	if err := result.Decode(&row); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// RequireLiveCapabilities fails startup before live mode can write if Mongo is
// not a replica set/sharded deployment with logical sessions. Transactions and
// change streams are safety requirements, not optional fallbacks.
func (s *Store) RequireLiveCapabilities(ctx context.Context) error {
	var hello struct {
		SetName                      string `bson:"setName"`
		Message                      string `bson:"msg"`
		LogicalSessionTimeoutMinutes *int64 `bson:"logicalSessionTimeoutMinutes"`
	}
	if err := s.db.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		return fmt.Errorf("Mongo hello: %w", err)
	}
	if hello.LogicalSessionTimeoutMinutes == nil {
		return errors.New("live Sportmonks mode requires Mongo logical sessions")
	}
	if hello.SetName == "" && hello.Message != "isdbgrid" {
		return errors.New("live Sportmonks mode requires a Mongo replica set or sharded cluster")
	}
	return nil
}

func (s *Store) SyncLeagues(ctx context.Context, leagues []client.League, now time.Time, gatePublicMatches bool) error {
	now = now.UTC()
	writes := make([]mongo.WriteModel, 0, len(leagues))
	seen := make([]int64, 0, len(leagues))
	for _, league := range leagues {
		if league.ID <= 0 {
			continue
		}
		seen = append(seen, league.ID)
		writes = append(writes, mongo.NewUpdateOneModel().SetFilter(bson.M{"_id": league.ID}).SetUpsert(true).SetUpdate(bson.M{
			"$set": bson.M{
				"name": league.Name, "code": league.Code, "entitled": true,
				"lastSeenAt": now, "updatedAt": now,
			},
			"$setOnInsert": bson.M{"enabled": false},
		}))
	}
	session, err := s.db.Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sessionContext mongo.SessionContext) (any, error) {
		if len(writes) > 0 {
			if _, err := s.leagues.BulkWrite(sessionContext, writes, options.BulkWrite().SetOrdered(false)); err != nil {
				return nil, err
			}
		}
		missing := bson.M{}
		if len(seen) > 0 {
			missing = bson.M{"_id": bson.M{"$nin": seen}}
		}
		cursor, err := s.leagues.Find(sessionContext, missing, options.Find().SetProjection(bson.M{"_id": 1}))
		if err != nil {
			return nil, err
		}
		var revoked []struct {
			ID int64 `bson:"_id"`
		}
		if err := cursor.All(sessionContext, &revoked); err != nil {
			cursor.Close(sessionContext)
			return nil, err
		}
		cursor.Close(sessionContext)
		if _, err := s.leagues.UpdateMany(sessionContext, missing, bson.M{"$set": bson.M{
			"entitled": false, "enabled": false, "updatedAt": now,
		}}); err != nil {
			return nil, err
		}
		for _, league := range revoked {
			if err := s.disableLeague(sessionContext, league.ID, now, gatePublicMatches); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()))
	return err
}

func (s *Store) SetLeagueEnabled(ctx context.Context, leagueID int64, enabled bool) (bool, error) {
	if leagueID <= 0 {
		return false, nil
	}
	now := time.Now().UTC()
	session, err := s.db.Client().StartSession()
	if err != nil {
		return false, err
	}
	defer session.EndSession(ctx)
	value, err := session.WithTransaction(ctx, func(sessionContext mongo.SessionContext) (any, error) {
		filter := bson.M{"_id": leagueID}
		if enabled {
			filter["entitled"] = true
		}
		result, err := s.leagues.UpdateOne(sessionContext, filter, bson.M{"$set": bson.M{"enabled": enabled, "updatedAt": now}})
		if err != nil || result.MatchedCount == 0 {
			return false, err
		}
		if enabled {
			if _, err := s.fixtures.UpdateMany(sessionContext, bson.M{
				"leagueId": leagueID, "supported": true,
			}, bson.M{"$set": bson.M{"eligible": true, "nextPollAt": now, "updatedAt": now}}); err != nil {
				return false, err
			}
			_, err = s.matches.UpdateMany(sessionContext, bson.M{
				"provider": ProviderName, "providerLeagueId": leagueID, "status": matches.StatusUpcoming,
			}, bson.M{"$set": bson.M{"hidden": false, "updatedAt": now}})
			return true, err
		}

		return true, s.disableLeague(sessionContext, leagueID, now, true)
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()))
	if err != nil {
		return false, err
	}
	updated, _ := value.(bool)
	return updated, nil
}

func (s *Store) disableLeague(ctx context.Context, leagueID int64, now time.Time, gatePublicMatches bool) error {
	if _, err := s.fixtures.UpdateMany(ctx, bson.M{"leagueId": leagueID}, bson.M{"$set": bson.M{
		"eligible": false, "updatedAt": now,
	}}); err != nil {
		return err
	}
	if !gatePublicMatches {
		return nil
	}
	cursor, err := s.matches.Find(ctx, bson.M{"provider": ProviderName, "providerLeagueId": leagueID})
	if err != nil {
		return err
	}
	var providerMatches []matches.Match
	if err := cursor.All(ctx, &providerMatches); err != nil {
		cursor.Close(ctx)
		return err
	}
	cursor.Close(ctx)
	for _, match := range providerMatches {
		status := matches.NormalizeStatus(match.Status)
		if status == matches.StatusUpcoming {
			if _, err := s.matches.UpdateOne(ctx, bson.M{"_id": match.ID}, bson.M{"$set": bson.M{
				"hidden": true, "updatedAt": now,
			}}); err != nil {
				return err
			}
			continue
		}
		if status == matches.StatusCompleted || status == matches.StatusAbandoned || match.FeedState == matches.FeedStateTerminal || containsValue(match.TradingBlockers, "league_disabled") {
			continue
		}
		previousVersion := match.StateVersion
		if err := s.resetPendingFinalization(ctx, &match, now); err != nil {
			return err
		}
		match.StateVersion++
		match.TradingVersion++
		match.FeedState = matches.FeedStateUnsupported
		match.HealthySnapshotCount = 0
		match.TradingState = "blocked"
		match.TradingBlockers = appendUnique(providerBlockers(match.TradingBlockers, "league_disabled"), "cancellation_pending")
		match.UpdatedAt = now
		result, err := s.matches.ReplaceOne(ctx, bson.M{
			"_id": match.ID, "stateVersion": previousVersion,
		}, match)
		if err != nil || result.ModifiedCount != 1 {
			if err == nil {
				err = ErrConcurrentApply
			}
			return err
		}
		if err := s.projectMarkets(ctx, match, reconcile.Projection{CurrentInnings: match.Innings}); err != nil {
			return err
		}
		if err := s.enqueueTradingGateJob(ctx, match, now); err != nil {
			return err
		}
		if err := s.insertMarketSnapshots(ctx, match, now); err != nil {
			return err
		}
		if err := s.insertMatchOutbox(ctx, match, "match.league_disabled", now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) EnabledLeagueIDs(ctx context.Context) ([]int64, error) {
	cursor, err := s.leagues.Find(ctx, bson.M{"enabled": true, "entitled": true}, options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var rows []struct {
		ID int64 `bson:"_id"`
	}
	if err := cursor.All(ctx, &rows); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids, nil
}

func (s *Store) ListLeagues(ctx context.Context) ([]League, error) {
	cursor, err := s.leagues.Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "enabled", Value: -1}, {Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var leagues []League
	if err := cursor.All(ctx, &leagues); err != nil {
		return nil, err
	}
	return leagues, nil
}

func (s *Store) ListIncidents(ctx context.Context, limit int64) ([]Incident, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	cursor, err := s.incidents.Find(ctx, bson.M{}, options.Find().
		SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(limit))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var incidents []Incident
	if err := cursor.All(ctx, &incidents); err != nil {
		return nil, err
	}
	return incidents, nil
}

func (s *Store) RequestFixtureResync(ctx context.Context, fixtureID int64, now time.Time) (bool, error) {
	result, err := s.fixtures.UpdateOne(ctx, bson.M{"_id": fixtureID}, bson.M{
		"$set": bson.M{
			"eligible": true, "nextPollAt": now.UTC(),
			"lastError": "manual_resync", "updatedAt": now.UTC(),
		},
		"$unset": bson.M{"leaseOwner": "", "leaseToken": "", "leaseUntil": ""},
	}, options.Update().SetUpsert(true))
	if err != nil {
		return false, err
	}
	return result.MatchedCount > 0 || result.UpsertedCount > 0, nil
}

func (s *Store) AdminStatus(ctx context.Context, now time.Time) (Status, error) {
	var status Status
	status.FeedStates = make(map[string]int64)
	status.RequestsThisHour = make(map[string]int)
	var err error
	if status.EnabledLeagues, err = s.leagues.CountDocuments(ctx, bson.M{"enabled": true, "entitled": true}); err != nil {
		return Status{}, err
	}
	if status.EligibleFixtures, err = s.fixtures.CountDocuments(ctx, bson.M{"eligible": true}); err != nil {
		return Status{}, err
	}
	if status.LeasedFixtures, err = s.fixtures.CountDocuments(ctx, bson.M{"leaseUntil": bson.M{"$gt": now.UTC()}}); err != nil {
		return Status{}, err
	}
	if status.PendingSettlements, err = s.settlements.CountDocuments(ctx, bson.M{"status": bson.M{"$in": []string{"pending", "processing", "failed"}}}); err != nil {
		return Status{}, err
	}
	if status.PendingCancellations, err = s.gateJobs.CountDocuments(ctx, bson.M{"status": bson.M{"$in": []string{"pending", "processing", "failed"}}}); err != nil {
		return Status{}, err
	}
	cursor, err := s.matches.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"provider": ProviderName}}},
		{{Key: "$group", Value: bson.M{"_id": "$feedState", "count": bson.M{"$sum": 1}, "last": bson.M{"$max": "$lastSuccessfulPollAt"}}}},
	})
	if err != nil {
		return Status{}, err
	}
	defer cursor.Close(ctx)
	var rows []struct {
		State string     `bson:"_id"`
		Count int64      `bson:"count"`
		Last  *time.Time `bson:"last"`
	}
	if err := cursor.All(ctx, &rows); err != nil {
		return Status{}, err
	}
	for _, row := range rows {
		status.FeedStates[row.State] = row.Count
		if row.Last != nil && (status.LastSuccessfulPoll == nil || row.Last.After(*status.LastSuccessfulPoll)) {
			copy := row.Last.UTC()
			status.LastSuccessfulPoll = &copy
		}
	}
	status.GlobalTradingKilled, err = s.GlobalTradingKilled(ctx)
	if err != nil {
		return Status{}, err
	}
	quotaCursor, err := s.quota.Find(ctx, bson.M{"bucketStart": now.UTC().Truncate(time.Hour)}, options.Find().SetProjection(bson.M{"endpoint": 1, "count": 1}))
	if err != nil {
		return Status{}, err
	}
	defer quotaCursor.Close(ctx)
	var quotaRows []struct {
		Endpoint string `bson:"endpoint"`
		Count    int    `bson:"count"`
	}
	if err := quotaCursor.All(ctx, &quotaRows); err != nil {
		return Status{}, err
	}
	for _, row := range quotaRows {
		status.RequestsThisHour[row.Endpoint] = row.Count
	}
	return status, nil
}

func (s *Store) FixtureDiagnostics(ctx context.Context, fixtureID int64, includeRaw bool) (FixtureDiagnostics, bool, error) {
	if fixtureID <= 0 {
		return FixtureDiagnostics{}, false, nil
	}
	var diagnostics FixtureDiagnostics
	if err := s.fixtures.FindOne(ctx, bson.M{"_id": fixtureID}).Decode(&diagnostics.Target); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return FixtureDiagnostics{}, false, nil
		}
		return FixtureDiagnostics{}, false, err
	}
	var match matches.Match
	if err := s.matches.FindOne(ctx, bson.M{"provider": ProviderName, "providerFixtureId": fixtureID}).Decode(&match); err == nil {
		diagnostics.Match = &match
	} else if !errors.Is(err, mongo.ErrNoDocuments) {
		return FixtureDiagnostics{}, false, err
	}
	var shadow ShadowProjection
	if err := s.shadow.FindOne(ctx, bson.M{"_id": fixtureID}).Decode(&shadow); err == nil {
		diagnostics.Shadow = &shadow
	} else if !errors.Is(err, mongo.ErrNoDocuments) {
		return FixtureDiagnostics{}, false, err
	}
	reportCursor, err := s.reports.Find(ctx, bson.M{"fixtureId": fixtureID}, options.Find().
		SetSort(bson.D{{Key: "receivedAt", Value: -1}}).SetLimit(20))
	if err != nil {
		return FixtureDiagnostics{}, false, err
	}
	if err := reportCursor.All(ctx, &diagnostics.Reports); err != nil {
		reportCursor.Close(ctx)
		return FixtureDiagnostics{}, false, err
	}
	reportCursor.Close(ctx)
	projection := bson.M{"valid": 1, "error": 1, "receivedAt": 1}
	if includeRaw {
		projection["raw"] = 1
	}
	var payload PayloadDiagnostic
	if err := s.payloads.FindOne(ctx, bson.M{"fixtureId": fixtureID}, options.FindOne().
		SetSort(bson.D{{Key: "receivedAt", Value: -1}}).SetProjection(projection)).Decode(&payload); err == nil {
		diagnostics.LatestPayload = &payload
	} else if !errors.Is(err, mongo.ErrNoDocuments) {
		return FixtureDiagnostics{}, false, err
	}
	if diagnostics.Reports == nil {
		diagnostics.Reports = []ReconciliationReport{}
	}
	return diagnostics, true, nil
}

func (s *Store) GlobalTradingKilled(ctx context.Context) (bool, error) {
	var control TradingControl
	err := s.controls.FindOne(ctx, bson.M{"_id": "global"}).Decode(&control)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return false, nil
	}
	return control.Killed, err
}

func (s *Store) SetGlobalTradingKill(ctx context.Context, killed bool, now time.Time) error {
	session, err := s.db.Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sessionContext mongo.SessionContext) (any, error) {
		if _, err := s.controls.UpdateOne(sessionContext, bson.M{"_id": "global"}, bson.M{"$set": bson.M{
			"killed": killed, "updatedAt": now.UTC(),
		}}, options.Update().SetUpsert(true)); err != nil {
			return nil, err
		}
		cursor, err := s.matches.Find(sessionContext, bson.M{
			"provider":  ProviderName,
			"feedState": bson.M{"$ne": matches.FeedStateTerminal},
		})
		if err != nil {
			return nil, err
		}
		var providerMatches []matches.Match
		if err := cursor.All(sessionContext, &providerMatches); err != nil {
			cursor.Close(sessionContext)
			return nil, err
		}
		cursor.Close(sessionContext)
		for _, match := range providerMatches {
			hasKill := containsValue(match.TradingBlockers, "global_kill")
			if hasKill == killed {
				continue
			}
			match.StateVersion++
			match.TradingVersion++
			match.TradingState = "blocked"
			if killed {
				match.TradingBlockers = appendUnique(match.TradingBlockers, "global_kill")
				match.TradingBlockers = appendUnique(match.TradingBlockers, "cancellation_pending")
			} else {
				match.TradingBlockers = removeValue(match.TradingBlockers, "global_kill")
				if len(match.TradingBlockers) == 0 {
					match.TradingBlockers = []string{"warming"}
				}
			}
			match.UpdatedAt = now.UTC()
			result, err := s.matches.ReplaceOne(sessionContext, bson.M{
				"_id": match.ID, "stateVersion": match.StateVersion - 1,
			}, match)
			if err != nil || result.ModifiedCount != 1 {
				if err == nil {
					err = ErrConcurrentApply
				}
				return nil, err
			}
			if err := s.projectMarkets(sessionContext, match, reconcile.Projection{CurrentInnings: match.Innings}); err != nil {
				return nil, err
			}
			if err := s.insertMarketSnapshots(sessionContext, match, now.UTC()); err != nil {
				return nil, err
			}
			if killed {
				if err := s.enqueueTradingGateJob(sessionContext, match, now.UTC()); err != nil {
					return nil, err
				}
			}
			if err := s.insertMatchOutbox(sessionContext, match, "match.kill_switch", now.UTC()); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()))
	return err
}

func (s *Store) SetProviderManualGate(ctx context.Context, matchID string, marketID primitive.ObjectID, blocked bool) (*markets.Market, error) {
	objectID, err := primitive.ObjectIDFromHex(strings.TrimSpace(matchID))
	if err != nil || marketID.IsZero() {
		return nil, errors.New("invalid provider market gate identity")
	}
	now := time.Now().UTC()
	session, err := s.db.Client().StartSession()
	if err != nil {
		return nil, err
	}
	defer session.EndSession(ctx)
	value, err := session.WithTransaction(ctx, func(sessionContext mongo.SessionContext) (any, error) {
		var match matches.Match
		if err := s.matches.FindOne(sessionContext, bson.M{"_id": objectID, "provider": ProviderName}).Decode(&match); err != nil {
			return nil, err
		}
		marketList, err := s.markets.ListMarketsByMatchID(sessionContext, match.ID.Hex())
		if err != nil {
			return nil, fmt.Errorf("list provider markets: %w", err)
		}
		var selected *markets.Market
		for _, market := range marketList {
			if market.ID == marketID {
				copy := market
				selected = &copy
				break
			}
		}
		if selected == nil || selected.Innings != match.Innings ||
			selected.Lifecycle == markets.MarketLifecycleSettled || selected.Lifecycle == markets.MarketLifecycleVoid {
			return nil, errors.New("provider market is not the current mutable innings contract")
		}
		matchBlocked := containsValue(match.TradingBlockers, "manual")
		marketBlocked := containsValue(selected.Blockers, "manual")
		updated, err := s.markets.SetProviderManualBlocker(sessionContext, marketID, blocked)
		if err != nil || updated == nil {
			if err == nil {
				err = errors.New("provider market was not found")
			}
			return nil, err
		}
		if matchBlocked == blocked && marketBlocked == blocked {
			return updated, nil
		}

		previousTradingVersion := match.TradingVersion
		match.TradingVersion++
		match.TradingState = "blocked"
		if blocked {
			match.TradingBlockers = appendUnique(match.TradingBlockers, "manual")
			match.TradingBlockers = appendUnique(match.TradingBlockers, "cancellation_pending")
		} else {
			match.TradingBlockers = removeValue(match.TradingBlockers, "manual")
			if len(match.TradingBlockers) == 0 {
				match.TradingBlockers = []string{"warming"}
			}
		}
		match.UpdatedAt = now
		result, err := s.matches.ReplaceOne(sessionContext, bson.M{
			"_id": match.ID, "stateVersion": match.StateVersion,
			"tradingVersion": previousTradingVersion,
		}, match)
		if err != nil {
			return nil, err
		}
		if result.ModifiedCount != 1 {
			return nil, ErrConcurrentApply
		}
		if err := s.projectMarkets(sessionContext, match, reconcile.Projection{CurrentInnings: match.Innings}); err != nil {
			return nil, err
		}
		if blocked {
			if err := s.enqueueTradingGateJob(sessionContext, match, now); err != nil {
				return nil, err
			}
		}
		if err := s.insertMarketSnapshots(sessionContext, match, now); err != nil {
			return nil, err
		}
		if err := s.insertMatchOutbox(sessionContext, match, "match.manual_gate", now); err != nil {
			return nil, err
		}
		marketList, err = s.markets.ListMarketsByMatchID(sessionContext, match.ID.Hex())
		if err != nil {
			return nil, fmt.Errorf("reload provider markets: %w", err)
		}
		for _, market := range marketList {
			if market.ID == marketID {
				copy := market
				return &copy, nil
			}
		}
		return nil, errors.New("provider market disappeared during gate update")
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()))
	if err != nil {
		return nil, err
	}
	market, ok := value.(*markets.Market)
	if !ok || market == nil {
		return nil, errors.New("invalid provider market gate result")
	}
	return market, nil
}

func (s *Store) ExpireStaleFeeds(ctx context.Context, now time.Time, limit int64) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	cursor, err := s.matches.Find(ctx, bson.M{
		"provider": ProviderName, "feedState": bson.M{"$in": []string{
			matches.FeedStateWarming, matches.FeedStateHealthy, matches.FeedStateFinalizing,
		}},
		"feedValidUntil": bson.M{"$lte": now.UTC()},
	}, options.Find().SetProjection(bson.M{
		"providerFixtureId": 1, "lastSuccessfulPollAt": 1,
	}).SetLimit(limit))
	if err != nil {
		return 0, err
	}
	defer cursor.Close(ctx)
	var rows []struct {
		FixtureID   int64      `bson:"providerFixtureId"`
		LastSuccess *time.Time `bson:"lastSuccessfulPollAt"`
	}
	if err := cursor.All(ctx, &rows); err != nil {
		return 0, err
	}
	expired := 0
	for _, row := range rows {
		if row.FixtureID <= 0 || row.LastSuccess == nil {
			continue
		}
		if err := s.MarkFeedUnavailable(ctx, row.FixtureID, matches.FeedStateStale, "feed_stale", now.UTC(), row.LastSuccess); err != nil {
			return expired, err
		}
		expired++
	}
	return expired, nil
}

func (s *Store) SaveCatalog(ctx context.Context, scores []client.Score, now time.Time) (reconcile.Catalog, error) {
	entries := make([]json.RawMessage, 0, len(scores))
	for _, score := range scores {
		raw := append(json.RawMessage(nil), score.Raw...)
		if len(raw) == 0 {
			encoded, err := json.Marshal(score)
			if err != nil {
				return nil, err
			}
			raw = encoded
		}
		entries = append(entries, raw)
	}
	raw, err := json.Marshal(struct {
		Data []json.RawMessage `json:"data"`
	}{Data: entries})
	if err != nil {
		return nil, err
	}
	catalog, err := reconcile.CatalogFromJSON(raw)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(raw)
	hashString := hex.EncodeToString(hash[:])
	if _, err = s.catalog.UpdateOne(ctx, bson.M{"_id": hashString}, bson.M{"$setOnInsert": bson.M{
		"raw": raw, "hash": hashString, "createdAt": now.UTC(),
	}}, options.Update().SetUpsert(true)); err != nil {
		return nil, err
	}
	_, err = s.catalog.UpdateOne(ctx, bson.M{"_id": "current"}, bson.M{"$set": bson.M{
		"raw": raw, "hash": hashString, "updatedAt": now.UTC(),
	}}, options.Update().SetUpsert(true))
	return catalog, err
}

func (s *Store) LoadCatalog(ctx context.Context) (reconcile.Catalog, error) {
	var row struct {
		Raw []byte `bson:"raw"`
	}
	if err := s.catalog.FindOne(ctx, bson.M{"_id": "current"}).Decode(&row); err != nil {
		return nil, err
	}
	return reconcile.CatalogFromJSON(row.Raw)
}

func (s *Store) UpsertFixtureTargets(ctx context.Context, fixtures []client.Fixture, now time.Time, liveMode bool, allowMidMatchAdmission bool) error {
	now = now.UTC()
	enabledIDs, err := s.EnabledLeagueIDs(ctx)
	if err != nil {
		return err
	}
	enabled := make(map[int64]struct{}, len(enabledIDs))
	for _, id := range enabledIDs {
		enabled[id] = struct{}{}
	}
	admitted, err := s.admittedFixtureIDs(ctx, fixtures)
	if err != nil {
		return err
	}
	existing, err := s.fixtureTargetsByID(ctx, fixtures)
	if err != nil {
		return err
	}
	writes := make([]mongo.WriteModel, 0, len(fixtures))
	for _, fixture := range fixtures {
		if fixture.ID <= 0 || fixture.LeagueID <= 0 {
			continue
		}
		if _, allowed := enabled[fixture.LeagueID]; !allowed {
			continue
		}
		format, scheduled, formatErr := reconcile.ClassifyFormat(fixture.Type)
		identityValid := fixture.SeasonID > 0 && fixture.LocalTeamID > 0 && fixture.VisitorTeamID > 0 && fixture.LocalTeamID != fixture.VisitorTeamID
		eligible := identityValid && formatErr == nil && !fixture.SuperOver && !rawMeaningful(fixture.RPCOvers) && !rawMeaningful(fixture.RPCTarget)
		start, err := parseProviderTime(fixture.StartingAt)
		if err != nil {
			eligible = false
		}
		if liveMode {
			_, alreadyAdmitted := admitted[fixture.ID]
			if !alreadyAdmitted {
				eligible = eligible && (futureNotStartedFixture(fixture, start, now) ||
					liveAdmissionAllowed(fixture, allowMidMatchAdmission, existing[fixture.ID], now))
			}
		}
		set := bson.M{
			"leagueId": fixture.LeagueID, "seasonId": fixture.SeasonID,
			"localTeamId": fixture.LocalTeamID, "visitorTeamId": fixture.VisitorTeamID,
			"format": format, "scheduledBalls": scheduled, "providerStatus": fixture.Status,
			"startTime": start, "supported": eligible, "eligible": eligible, "updatedAt": now,
		}
		update := bson.M{
			"$set":         set,
			"$setOnInsert": bson.M{"createdAt": now},
		}
		if nextPoll, apply, replace := fixtureTargetNextPoll(existing[fixture.ID], fixture, start, now); apply {
			if replace {
				set["nextPollAt"] = nextPoll
			} else {
				update["$min"] = bson.M{"nextPollAt": nextPoll}
			}
		}
		writes = append(writes, mongo.NewUpdateOneModel().SetFilter(bson.M{"_id": fixture.ID}).SetUpsert(true).SetUpdate(update))
	}
	if len(writes) == 0 {
		return nil
	}
	_, err = s.fixtures.BulkWrite(ctx, writes, options.BulkWrite().SetOrdered(false))
	return err
}

func (s *Store) fixtureTargetsByID(ctx context.Context, fixtures []client.Fixture) (map[int64]*FixtureTarget, error) {
	ids := make([]int64, 0, len(fixtures))
	seen := make(map[int64]struct{}, len(fixtures))
	for _, fixture := range fixtures {
		if fixture.ID <= 0 {
			continue
		}
		if _, duplicate := seen[fixture.ID]; duplicate {
			continue
		}
		seen[fixture.ID] = struct{}{}
		ids = append(ids, fixture.ID)
	}
	result := make(map[int64]*FixtureTarget, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	cursor, err := s.fixtures.Find(ctx, bson.M{"_id": bson.M{"$in": ids}})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var rows []FixtureTarget
	if err := cursor.All(ctx, &rows); err != nil {
		return nil, err
	}
	for i := range rows {
		result[rows[i].ID] = &rows[i]
	}
	return result, nil
}

func fixtureTargetNextPoll(existing *FixtureTarget, fixture client.Fixture, start, now time.Time) (time.Time, bool, bool) {
	next := start.Add(-30 * time.Minute)
	if next.Before(now) {
		next = now
	}
	if existing == nil {
		return next, true, true
	}
	if !reconcile.IsNotStartedStatus(fixture.Status) ||
		(!reconcile.IsNotStartedStatus(existing.ProviderStatus) && existing.LastSuccessAt != nil) {
		return time.Time{}, false, false
	}
	return next, true, start.After(existing.StartTime)
}

func (s *Store) PublishFixtureMatches(ctx context.Context, fixtures []client.Fixture, now time.Time, allowMidMatchAdmission bool) error {
	enabledIDs, err := s.EnabledLeagueIDs(ctx)
	if err != nil {
		return err
	}
	enabled := make(map[int64]struct{}, len(enabledIDs))
	for _, id := range enabledIDs {
		enabled[id] = struct{}{}
	}
	admitted, err := s.admittedFixtureIDs(ctx, fixtures)
	if err != nil {
		return err
	}
	existingTargets, err := s.fixtureTargetsByID(ctx, fixtures)
	if err != nil {
		return err
	}
	for _, fixture := range fixtures {
		if fixture.ID <= 0 {
			continue
		}
		if _, ok := enabled[fixture.LeagueID]; !ok {
			continue
		}
		start, err := parseProviderTime(fixture.StartingAt)
		if err != nil {
			continue
		}
		_, alreadyAdmitted := admitted[fixture.ID]
		midMatchAdmitted := liveAdmissionAllowed(fixture, allowMidMatchAdmission, existingTargets[fixture.ID], now.UTC())
		if !alreadyAdmitted && !futureNotStartedFixture(fixture, start, now.UTC()) && !midMatchAdmitted {
			continue
		}
		format, scheduledBalls, formatErr := reconcile.ClassifyFixtureFormat(fixture.Raw, fixture.Type)
		feedState := matches.FeedStateWarming
		blockers := []string{"not_live"}
		identityValid := fixture.LeagueID > 0 && fixture.SeasonID > 0 && fixture.LocalTeamID > 0 && fixture.VisitorTeamID > 0 && fixture.LocalTeamID != fixture.VisitorTeamID
		supported := identityValid && formatErr == nil && !fixture.SuperOver && !rawMeaningful(fixture.RPCOvers) && !rawMeaningful(fixture.RPCTarget)
		if !supported {
			format = fixture.Type
			scheduledBalls = 0
			feedState = matches.FeedStateUnsupported
			blockers = []string{"unsupported"}
		}
		localName := fixtureTeamName(fixture.LocalTeam, fixture.LocalTeamID)
		visitorName := fixtureTeamName(fixture.VisitorTeam, fixture.VisitorTeamID)
		localLogo := fixtureTeamLogo(fixture.LocalTeam)
		visitorLogo := fixtureTeamLogo(fixture.VisitorTeam)
		setOnInsert := bson.M{
			"_id": primitive.NewObjectID(), "dataSource": ProviderName, "provider": ProviderName,
			"providerFixtureId": fixture.ID, "providerLeagueId": fixture.LeagueID,
			"providerSeasonId": fixture.SeasonID, "providerTeamAId": fixture.LocalTeamID,
			"providerTeamBId": fixture.VisitorTeamID,
			"tournamentId":    fmt.Sprintf("sportmonks:%d", fixture.LeagueID), "format": format,
			"teamAId": fmt.Sprintf("sportmonks:%d", fixture.LocalTeamID),
			"teamBId": fmt.Sprintf("sportmonks:%d", fixture.VisitorTeamID),
			"status":  matches.StatusUpcoming, "innings": 1,
			"scheduledBalls": scheduledBalls, "ballsLeft": scheduledBalls, "oversText": "0.0",
			"stateVersion": 1, "tradingVersion": 1, "feedState": feedState,
			"tradingState": "blocked", "tradingBlockers": blockers, "createdAt": now.UTC(),
		}
		setFields := bson.M{
			"startTime": start,
			"teamAName": localName, "teamBName": visitorName,
			"teamALogo": localLogo, "teamBLogo": visitorLogo,
			"hidden": false, "updatedAt": now.UTC(),
		}
		// Keep upcoming publications in sync so previously unsupported ODI fixtures
		// become visible once classification succeeds.
		if reconcile.IsNotStartedStatus(fixture.Status) {
			setFields["providerPhase"] = fixture.Status
			setFields["format"] = format
			setFields["scheduledBalls"] = scheduledBalls
			setFields["ballsLeft"] = scheduledBalls
			setFields["oversText"] = "0.0"
			setFields["feedState"] = feedState
			setFields["tradingState"] = "blocked"
			setFields["tradingBlockers"] = blockers
			if supported {
				setFields["status"] = matches.StatusUpcoming
			}
		}
		_, err = s.matches.UpdateOne(ctx, bson.M{
			"provider": ProviderName, "providerFixtureId": fixture.ID,
		}, bson.M{
			"$setOnInsert": setOnInsert,
			"$set":         setFields,
		}, options.Update().SetUpsert(true))
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) admittedFixtureIDs(ctx context.Context, fixtures []client.Fixture) (map[int64]struct{}, error) {
	ids := make([]int64, 0, len(fixtures))
	seen := make(map[int64]struct{}, len(fixtures))
	for _, fixture := range fixtures {
		if fixture.ID <= 0 {
			continue
		}
		if _, exists := seen[fixture.ID]; exists {
			continue
		}
		seen[fixture.ID] = struct{}{}
		ids = append(ids, fixture.ID)
	}
	admitted := make(map[int64]struct{})
	if len(ids) == 0 {
		return admitted, nil
	}
	cursor, err := s.matches.Find(ctx, bson.M{
		"provider": ProviderName, "providerFixtureId": bson.M{"$in": ids},
	}, options.Find().SetProjection(bson.M{"providerFixtureId": 1}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var rows []struct {
		FixtureID int64 `bson:"providerFixtureId"`
	}
	if err := cursor.All(ctx, &rows); err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.FixtureID > 0 {
			admitted[row.FixtureID] = struct{}{}
		}
	}
	return admitted, nil
}

func futureNotStartedFixture(fixture client.Fixture, start, now time.Time) bool {
	return !start.IsZero() && start.After(now.UTC()) &&
		reconcile.IsNotStartedStatus(fixture.Status)
}

func liveAdmissionAllowed(fixture client.Fixture, enabled bool, target *FixtureTarget, now time.Time) bool {
	if !enabled {
		return false
	}
	if recentlyShadowValidated(target, now) {
		return true
	}
	return providerLiveOrBreakStatus(fixture.Status)
}

func providerLiveOrBreakStatus(status string) bool {
	lower := strings.ToLower(strings.TrimSpace(status))
	if lower == "" {
		return false
	}
	if strings.Contains(lower, "finished") || strings.Contains(lower, "complete") ||
		strings.Contains(lower, "aban") || strings.Contains(lower, "cancl") ||
		strings.Contains(lower, "not started") || lower == "ns" ||
		strings.Contains(lower, "delayed") || strings.Contains(lower, "postp") {
		return false
	}
	return strings.Contains(lower, "innings") || strings.Contains(lower, "break") ||
		strings.Contains(lower, "lunch") || strings.Contains(lower, "tea") ||
		strings.Contains(lower, "dinner") || strings.Contains(lower, "int.")
}

func recentlyShadowValidated(target *FixtureTarget, now time.Time) bool {
	if target == nil || target.LastSuccessAt == nil {
		return false
	}
	promotionOnlyFailure := target.LastError == ErrMidMatchPromotion.Error()
	return target.LastSuccessMode == string(client.ModeShadow) &&
		(target.ConsecutiveFailures == 0 || promotionOnlyFailure) &&
		(target.LastError == "" || promotionOnlyFailure) &&
		!target.LastSuccessAt.Before(now.UTC().Add(-5*time.Minute))
}

func (s *Store) DueTargets(ctx context.Context, now time.Time, limit int64) ([]FixtureTarget, error) {
	if limit <= 0 {
		limit = 20
	}
	cursor, err := s.fixtures.Find(ctx, bson.M{
		"eligible": true, "nextPollAt": bson.M{"$lte": now.UTC()},
		"$or": bson.A{
			bson.M{"leaseUntil": bson.M{"$exists": false}},
			bson.M{"leaseUntil": bson.M{"$lte": now.UTC()}},
		},
	}, options.Find().SetSort(bson.D{{Key: "nextPollAt", Value: 1}, {Key: "startTime", Value: 1}}).SetLimit(limit))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var targets []FixtureTarget
	if err := cursor.All(ctx, &targets); err != nil {
		return nil, err
	}
	return targets, nil
}

func (s *Store) PollableTargetCount(ctx context.Context, now time.Time) (int64, error) {
	return s.fixtures.CountDocuments(ctx, bson.M{
		"eligible":       true,
		"startTime":      bson.M{"$lte": now.UTC().Add(30 * time.Minute)},
		"providerStatus": bson.M{"$nin": []string{"Finished", "Aban.", "Cancl."}},
	})
}

func (s *Store) OpenTargetCount(ctx context.Context, now time.Time, mode string) (int64, error) {
	return s.fixtures.CountDocuments(ctx, bson.M{
		"eligible":  true,
		"startTime": bson.M{"$lte": now.UTC().Add(30 * time.Minute)},
		"$and": bson.A{
			bson.M{"$or": bson.A{
				bson.M{"lastSuccessAt": bson.M{"$exists": true}, "lastSuccessMode": mode},
				bson.M{"leaseUntil": bson.M{"$gt": now.UTC()}},
			}},
			bson.M{"$or": bson.A{
				bson.M{"providerStatus": bson.M{"$nin": []string{"Finished", "Aban.", "Cancl."}}},
				bson.M{"providerStatus": "Finished", "nextPollAt": bson.M{"$lte": now.UTC().Add(2 * time.Minute)}},
			}},
		},
	})
}

func (s *Store) DeferTarget(ctx context.Context, fixtureID int64, next time.Time, reason string) error {
	_, err := s.fixtures.UpdateOne(ctx, bson.M{"_id": fixtureID}, bson.M{"$set": bson.M{
		"nextPollAt": next.UTC(), "lastError": reason, "updatedAt": time.Now().UTC(),
	}})
	return err
}

func (s *Store) ClaimTarget(ctx context.Context, fixtureID int64, owner string, now time.Time, ttl time.Duration) (string, bool, error) {
	until := now.UTC().Add(ttl)
	token := primitive.NewObjectID().Hex()
	result, err := s.fixtures.UpdateOne(ctx, bson.M{
		"_id": fixtureID, "eligible": true,
		"$or": bson.A{
			bson.M{"leaseUntil": bson.M{"$exists": false}},
			bson.M{"leaseUntil": bson.M{"$lte": now.UTC()}},
		},
	}, bson.M{"$set": bson.M{"leaseOwner": owner, "leaseToken": token, "leaseUntil": until, "updatedAt": now.UTC()}})
	return token, result != nil && result.ModifiedCount == 1, err
}

func (s *Store) CompleteTargetPoll(ctx context.Context, fixtureID int64, owner, token, mode, snapshotHash, status string, now, next time.Time) error {
	result, err := s.fixtures.UpdateOne(ctx, bson.M{"_id": fixtureID, "leaseOwner": owner, "leaseToken": token}, bson.M{
		"$set": bson.M{
			"providerStatus": status, "lastPollAt": now.UTC(), "lastSuccessAt": now.UTC(),
			"lastSuccessMode":  mode,
			"lastSnapshotHash": snapshotHash, "lastError": "", "consecutiveFailures": 0,
			"nextPollAt": next.UTC(), "updatedAt": now.UTC(),
		},
		"$unset": bson.M{"leaseOwner": "", "leaseToken": "", "leaseUntil": ""},
	})
	if err == nil && result.ModifiedCount != 1 {
		return errors.New("Sportmonks fixture lease was lost before poll completion")
	}
	return err
}

func (s *Store) FailTargetPoll(ctx context.Context, fixtureID int64, owner, token string, cause error, now, next time.Time) error {
	message := "poll failed"
	if cause != nil {
		message = cause.Error()
	}
	if len(message) > 500 {
		message = message[:500]
	}
	result, err := s.fixtures.UpdateOne(ctx, bson.M{"_id": fixtureID, "leaseOwner": owner, "leaseToken": token}, bson.M{
		"$set": bson.M{
			"lastPollAt": now.UTC(), "lastError": message, "nextPollAt": next.UTC(), "updatedAt": now.UTC(),
		},
		"$inc":   bson.M{"consecutiveFailures": 1},
		"$unset": bson.M{"leaseOwner": "", "leaseToken": "", "leaseUntil": ""},
	})
	if err == nil && result.ModifiedCount != 1 {
		return errors.New("Sportmonks fixture lease was lost before poll failure was recorded")
	}
	return err
}

func (s *Store) SavePayload(ctx context.Context, fixtureID int64, mode string, raw []byte, receivedAt time.Time, ttl time.Duration, valid bool, cause error) error {
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	message := ""
	if cause != nil {
		message = cause.Error()
		if len(message) > 1000 {
			message = message[:1000]
		}
	}
	_, err := s.payloads.UpdateOne(ctx,
		bson.M{"_id": fmt.Sprintf("%d:%s", fixtureID, mode)},
		bson.M{"$set": bson.M{
			"fixtureId": fixtureID, "mode": mode, "valid": valid, "error": message,
			"raw":        append(json.RawMessage(nil), raw...),
			"receivedAt": receivedAt.UTC(), "expiresAt": receivedAt.UTC().Add(ttl),
		}},
		options.Update().SetUpsert(true),
	)
	return err
}

func (s *Store) ClaimSettlementJob(ctx context.Context, owner string, now time.Time, ttl time.Duration) (*SettlementJob, error) {
	if strings.TrimSpace(owner) == "" || ttl <= 0 {
		return nil, errors.New("settlement claim requires owner and positive TTL")
	}
	until := now.UTC().Add(ttl)
	result := s.settlements.FindOneAndUpdate(ctx, bson.M{
		"$or": bson.A{
			retryableJobFilter(now),
			bson.M{"status": "processing", "leaseUntil": bson.M{"$lte": now.UTC()}},
		},
	}, bson.M{
		"$set": bson.M{"status": "processing", "leaseOwner": owner, "leaseUntil": until, "updatedAt": now.UTC()},
		"$inc": bson.M{"attempts": 1},
	}, options.FindOneAndUpdate().SetSort(bson.D{{Key: "nextAttemptAt", Value: 1}, {Key: "createdAt", Value: 1}}).SetReturnDocument(options.After))
	var job SettlementJob
	if err := result.Decode(&job); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &job, nil
}

func (s *Store) CompleteSettlementJob(ctx context.Context, id, owner string, now time.Time) error {
	result, err := s.settlements.UpdateOne(ctx, bson.M{"_id": id, "status": "processing", "leaseOwner": owner}, bson.M{
		"$set":   bson.M{"status": "complete", "updatedAt": now.UTC(), "lastError": ""},
		"$unset": bson.M{"leaseOwner": "", "leaseUntil": "", "nextAttemptAt": ""},
	})
	if err == nil && result.ModifiedCount != 1 {
		return errors.New("settlement job lease was lost")
	}
	return err
}

func (s *Store) RenewSettlementJob(ctx context.Context, id, owner string, now time.Time, ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("settlement lease TTL must be positive")
	}
	result, err := s.settlements.UpdateOne(ctx, bson.M{
		"_id": id, "status": "processing", "leaseOwner": owner,
	}, bson.M{"$set": bson.M{"leaseUntil": now.UTC().Add(ttl), "updatedAt": now.UTC()}})
	if err == nil && result.MatchedCount != 1 {
		return errors.New("settlement job lease was lost")
	}
	return err
}

func (s *Store) FailSettlementJob(ctx context.Context, id, owner string, cause error, now, retryAt time.Time) error {
	message := "settlement failed"
	if cause != nil {
		message = cause.Error()
	}
	if len(message) > 500 {
		message = message[:500]
	}
	retryAt = normalizedRetryAt(now, retryAt)
	result, err := s.settlements.UpdateOne(ctx, bson.M{"_id": id, "status": "processing", "leaseOwner": owner}, bson.M{
		"$set":   bson.M{"status": "failed", "lastError": message, "nextAttemptAt": retryAt, "updatedAt": now.UTC()},
		"$unset": bson.M{"leaseOwner": "", "leaseUntil": ""},
	})
	if err == nil && result.ModifiedCount != 1 {
		return errors.New("settlement job lease was lost")
	}
	return err
}

func (s *Store) ClaimTradingGateJob(ctx context.Context, owner string, now time.Time, ttl time.Duration) (*TradingGateJob, error) {
	if strings.TrimSpace(owner) == "" || ttl <= 0 {
		return nil, errors.New("trading gate claim requires owner and positive TTL")
	}
	result := s.gateJobs.FindOneAndUpdate(ctx, bson.M{
		"$or": bson.A{
			retryableJobFilter(now),
			bson.M{"status": "processing", "leaseUntil": bson.M{"$lte": now.UTC()}},
		},
	}, bson.M{
		"$set": bson.M{
			"status": "processing", "leaseOwner": owner,
			"leaseUntil": now.UTC().Add(ttl), "updatedAt": now.UTC(),
		},
		"$inc": bson.M{"attempts": 1},
	}, options.FindOneAndUpdate().SetSort(bson.D{{Key: "nextAttemptAt", Value: 1}, {Key: "createdAt", Value: 1}}).SetReturnDocument(options.After))
	var job TradingGateJob
	if err := result.Decode(&job); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &job, nil
}

func (s *Store) RenewTradingGateJob(ctx context.Context, id, owner string, now time.Time, ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("trading gate lease TTL must be positive")
	}
	result, err := s.gateJobs.UpdateOne(ctx, bson.M{
		"_id": id, "status": "processing", "leaseOwner": owner,
	}, bson.M{"$set": bson.M{"leaseUntil": now.UTC().Add(ttl), "updatedAt": now.UTC()}})
	if err == nil && result.MatchedCount != 1 {
		return errors.New("trading gate job lease was lost")
	}
	return err
}

func (s *Store) CompleteTradingGateJob(ctx context.Context, id, owner string, now time.Time) error {
	session, err := s.db.Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sessionContext mongo.SessionContext) (any, error) {
		var job TradingGateJob
		if err := s.gateJobs.FindOne(sessionContext, bson.M{
			"_id": id, "status": "processing", "leaseOwner": owner,
		}).Decode(&job); err != nil {
			return nil, err
		}
		result, err := s.gateJobs.UpdateOne(sessionContext, bson.M{
			"_id": id, "status": "processing", "leaseOwner": owner,
		}, bson.M{
			"$set":   bson.M{"status": "complete", "updatedAt": now.UTC(), "lastError": ""},
			"$unset": bson.M{"leaseOwner": "", "leaseUntil": "", "nextAttemptAt": ""},
		})
		if err != nil || result.ModifiedCount != 1 {
			if err == nil {
				err = errors.New("trading gate job lease was lost")
			}
			return nil, err
		}
		outstanding, err := s.gateJobs.CountDocuments(sessionContext, bson.M{
			"matchId": job.MatchID, "_id": bson.M{"$ne": id},
			"status": bson.M{"$in": []string{"pending", "processing", "failed"}},
		})
		if err != nil || outstanding > 0 {
			return nil, err
		}
		matchID, err := primitive.ObjectIDFromHex(job.MatchID)
		if err != nil {
			return nil, err
		}
		var match matches.Match
		if err := s.matches.FindOne(sessionContext, bson.M{"_id": matchID, "provider": ProviderName}).Decode(&match); err != nil {
			return nil, err
		}
		match.TradingBlockers = removeValue(match.TradingBlockers, "cancellation_pending")
		if match.FeedState == matches.FeedStateHealthy && len(match.TradingBlockers) == 0 {
			match.TradingState = "open"
		} else {
			match.TradingState = "blocked"
		}
		match.TradingVersion++
		match.UpdatedAt = now.UTC()
		replace, err := s.matches.ReplaceOne(sessionContext, bson.M{
			"_id": match.ID, "stateVersion": match.StateVersion,
		}, match)
		if err != nil || replace.ModifiedCount != 1 {
			if err == nil {
				err = ErrConcurrentApply
			}
			return nil, err
		}
		if err := s.projectMarkets(sessionContext, match, reconcile.Projection{CurrentInnings: match.Innings}); err != nil {
			return nil, err
		}
		if err := s.insertMarketSnapshots(sessionContext, match, now.UTC()); err != nil {
			return nil, err
		}
		return nil, s.insertMatchOutbox(sessionContext, match, "match.orders_cancelled", now.UTC())
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()))
	return err
}

func (s *Store) FailTradingGateJob(ctx context.Context, id, owner string, cause error, now, retryAt time.Time) error {
	message := "working-order cancellation failed"
	if cause != nil {
		message = cause.Error()
	}
	if len(message) > 500 {
		message = message[:500]
	}
	retryAt = normalizedRetryAt(now, retryAt)
	result, err := s.gateJobs.UpdateOne(ctx, bson.M{
		"_id": id, "status": "processing", "leaseOwner": owner,
	}, bson.M{
		"$set":   bson.M{"status": "failed", "lastError": message, "nextAttemptAt": retryAt, "updatedAt": now.UTC()},
		"$unset": bson.M{"leaseOwner": "", "leaseUntil": ""},
	})
	if err == nil && result.ModifiedCount != 1 {
		return errors.New("trading gate job lease was lost")
	}
	return err
}

func retryableJobFilter(now time.Time) bson.M {
	return bson.M{
		"status": bson.M{"$in": []string{"pending", "failed"}},
		"$or": bson.A{
			bson.M{"nextAttemptAt": bson.M{"$exists": false}},
			bson.M{"nextAttemptAt": bson.M{"$lte": now.UTC()}},
		},
	}
}

func normalizedRetryAt(now, retryAt time.Time) time.Time {
	now = now.UTC()
	if !retryAt.After(now) {
		return now.Add(5 * time.Second)
	}
	return retryAt.UTC()
}

func rawMeaningful(raw json.RawMessage) bool {
	clean := strings.TrimSpace(string(raw))
	return clean != "" && clean != "null" && clean != "0" && clean != `""`
}

func parseProviderTime(raw string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if value, err := time.Parse(layout, strings.TrimSpace(raw)); err == nil {
			return value.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid provider time %q", raw)
}

type providerTeam struct {
	Name      string `json:"name"`
	ImagePath string `json:"image_path"`
}

func fixtureTeamName(raw json.RawMessage, id int64) string {
	team, err := client.DecodeRelation[providerTeam](raw)
	if err == nil && strings.TrimSpace(team.Name) != "" {
		return strings.TrimSpace(team.Name)
	}
	return fmt.Sprintf("Team %d", id)
}

func fixtureTeamLogo(raw json.RawMessage) string {
	team, err := client.DecodeRelation[providerTeam](raw)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(team.ImagePath)
}

func isNoDocuments(err error) bool { return errors.Is(err, mongo.ErrNoDocuments) }

func removeValue(values []string, target string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != target {
			out = append(out, value)
		}
	}
	return out
}

func containsValue(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
