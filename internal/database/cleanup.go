package database

import (
	"context"
	"log"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// dropEphemeralCollections are fully dropped on purge so WiredTiger frees Atlas
// storage (DeleteMany leaves allocated space on free-tier clusters).
// Never includes users, wallet_*, orders, matches, markets, executions, trades,
// or realtime_outbox (dropping outbox breaks the live ball change-stream).
var dropEphemeralCollections = []string{
	"provider_payloads",
	"market_snapshots",
	"reconciliation_reports",
	"shadow_projections",
}

// PurgeEphemeralStorage frees Atlas space used by Sportmonks debug/churn data.
// It never touches users, wallet_*, orders, matches, markets, executions, or
// position_projections.
func PurgeEphemeralStorage(ctx context.Context, db *mongo.Database) (int64, error) {
	if db == nil {
		return 0, nil
	}
	var deleted int64
	for _, name := range dropEphemeralCollections {
		count, _ := db.Collection(name).EstimatedDocumentCount(ctx)
		if err := db.Collection(name).Drop(ctx); err != nil {
			msg := strings.ToLower(err.Error())
			if isSpaceQuotaError(err) {
				log.Printf("mongo drop %s blocked by space quota: %v", name, err)
				continue
			}
			if strings.Contains(msg, "ns not found") || strings.Contains(msg, "namespace not found") {
				continue
			}
			return deleted, err
		}
		log.Printf("mongo dropped ephemeral collection %s (was ~%d docs) to reclaim storage", name, count)
		deleted += count
	}

	filters := []struct {
		name   string
		filter bson.M
	}{
		{name: "provider_incidents", filter: bson.M{
			"createdAt": bson.M{"$lt": time.Now().UTC().Add(-7 * 24 * time.Hour)},
		}},
		{name: "provider_request_quota", filter: bson.M{
			"expiresAt": bson.M{"$lt": time.Now().UTC()},
		}},
		{name: "settlement_jobs", filter: bson.M{
			"status":    bson.M{"$in": bson.A{"complete", "failed"}},
			"updatedAt": bson.M{"$lt": time.Now().UTC().Add(-7 * 24 * time.Hour)},
		}},
		{name: "trading_gate_jobs", filter: bson.M{
			"status":    bson.M{"$in": bson.A{"complete", "failed"}},
			"updatedAt": bson.M{"$lt": time.Now().UTC().Add(-7 * 24 * time.Hour)},
		}},
		{name: "realtime_outbox", filter: bson.M{
			"createdAt": bson.M{"$lt": time.Now().UTC().Add(-2 * time.Hour)},
		}},
	}
	for _, target := range filters {
		result, err := db.Collection(target.name).DeleteMany(ctx, target.filter)
		if err != nil {
			if isSpaceQuotaError(err) {
				log.Printf("mongo purge %s blocked by space quota: %v", target.name, err)
				continue
			}
			return deleted, err
		}
		if result.DeletedCount > 0 {
			log.Printf("mongo purged %s: deleted %d documents", target.name, result.DeletedCount)
			deleted += result.DeletedCount
		}
	}
	return deleted, nil
}

// EnsureEphemeralTTLs installs TTL indexes on high-churn Sportmonks collections
// so long ODI polls cannot permanently fill a 512MB free cluster.
func EnsureEphemeralTTLs(ctx context.Context, db *mongo.Database) error {
	if db == nil {
		return nil
	}
	specs := []struct {
		collection string
		model      mongo.IndexModel
	}{
		{
			collection: "market_snapshots",
			model: mongo.IndexModel{
				Keys:    bson.D{{Key: "createdAt", Value: 1}},
				Options: options.Index().SetName("createdAt_ttl_7d").SetExpireAfterSeconds(7 * 24 * 60 * 60),
			},
		},
		{
			collection: "provider_incidents",
			model: mongo.IndexModel{
				Keys:    bson.D{{Key: "createdAt", Value: 1}},
				Options: options.Index().SetName("createdAt_ttl_7d").SetExpireAfterSeconds(7 * 24 * 60 * 60),
			},
		},
		{
			collection: "reconciliation_reports",
			model: mongo.IndexModel{
				Keys:    bson.D{{Key: "receivedAt", Value: 1}},
				Options: options.Index().SetName("receivedAt_ttl_7d").SetExpireAfterSeconds(7 * 24 * 60 * 60),
			},
		},
		{
			collection: "realtime_outbox",
			// Short TTL: ball events must stream live, then expire to save Atlas space.
			// Do not Drop this collection — that kills the change stream mid-match.
			model: mongo.IndexModel{
				Keys:    bson.D{{Key: "createdAt", Value: 1}},
				Options: options.Index().SetName("createdAt_1").SetExpireAfterSeconds(2 * 60 * 60),
			},
		},
		{
			collection: "provider_payloads",
			model: mongo.IndexModel{
				Keys:    bson.D{{Key: "expiresAt", Value: 1}},
				Options: options.Index().SetName("expiresAt_1").SetExpireAfterSeconds(0),
			},
		},
	}
	for _, spec := range specs {
		if _, err := db.Collection(spec.collection).Indexes().CreateOne(ctx, spec.model); err != nil {
			if isSpaceQuotaError(err) || isIndexConflict(err) {
				// TTL already installed under another name — safe to ignore.
				continue
			}
			return err
		}
	}
	return nil
}

func isSpaceQuotaError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "space quota") || strings.Contains(msg, "storage quota")
}

func isIndexConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "indexoptionsconflict") ||
		strings.Contains(msg, "index key pattern")
}
