package simulator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var ErrLockHeld = errors.New("simulator lock held by another instance")

// LockStore coordinates simulator ownership across API instances.
type LockStore interface {
	EnsureIndexes(ctx context.Context) error
	Acquire(ctx context.Context, matchID, ownerID, token string, ttl time.Duration) (bool, error)
	Renew(ctx context.Context, matchID, ownerID, token string, ttl time.Duration) (bool, error)
	Release(ctx context.Context, matchID, ownerID, token string) error
}

type LockRecord struct {
	MatchID   string    `bson:"_id"`
	OwnerID   string    `bson:"ownerId"`
	ExpiresAt time.Time `bson:"expiresAt"`
	UpdatedAt time.Time `bson:"updatedAt"`
}

type LockLease struct {
	store   LockStore
	matchID string
	ownerID string
	token   string
	ttl     time.Duration
}

func (l *LockLease) Renew(ctx context.Context) error {
	if l == nil || l.store == nil {
		return nil
	}
	ok, err := l.store.Renew(ctx, l.matchID, l.ownerID, l.token, l.ttl)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w for match %s", ErrLockHeld, l.matchID)
	}
	return nil
}

func (l *LockLease) Release(ctx context.Context) {
	if l == nil || l.store == nil {
		return
	}
	if err := l.store.Release(ctx, l.matchID, l.ownerID, l.token); err != nil {
		log.Printf("simulator[%s]: release lock: %v", l.matchID, err)
	}
}

// MongoLockStore stores one expiring lock document per simulator match.
type MongoLockStore struct {
	col *mongo.Collection
}

func NewMongoLockStore(db *mongo.Database) *MongoLockStore {
	return &MongoLockStore{col: db.Collection("simulator_locks")}
}

func (s *MongoLockStore) EnsureIndexes(ctx context.Context) error {
	ctx, cancel := lockTimeout(ctx)
	defer cancel()
	_, err := s.col.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "expiresAt", Value: 1}},
		Options: options.Index().SetName("expiresAt_ttl").SetExpireAfterSeconds(0),
	})
	return err
}

func (s *MongoLockStore) Acquire(ctx context.Context, matchID, ownerID, token string, ttl time.Duration) (bool, error) {
	if err := validateLockArgs(matchID, ownerID, token, ttl); err != nil {
		return false, err
	}
	ctx, cancel := lockTimeout(ctx)
	defer cancel()

	now := time.Now().UTC()
	filter := bson.M{
		"_id": matchID,
		"$or": []bson.M{
			{"ownerId": ownerID},
			{"expiresAt": bson.M{"$lte": now}},
		},
	}
	update := bson.M{
		"$set": bson.M{
			"ownerId":   ownerID,
			"token":     token,
			"expiresAt": now.Add(ttl),
			"updatedAt": now,
		},
		"$setOnInsert": bson.M{
			"createdAt": now,
		},
	}
	res, err := s.col.UpdateOne(ctx, filter, update, options.Update().SetUpsert(true))
	if mongo.IsDuplicateKeyError(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0 || res.UpsertedCount > 0, nil
}

func (s *MongoLockStore) Renew(ctx context.Context, matchID, ownerID, token string, ttl time.Duration) (bool, error) {
	if err := validateLockArgs(matchID, ownerID, token, ttl); err != nil {
		return false, err
	}
	ctx, cancel := lockTimeout(ctx)
	defer cancel()

	now := time.Now().UTC()
	res, err := s.col.UpdateOne(
		ctx,
		bson.M{
			"_id":       matchID,
			"ownerId":   ownerID,
			"token":     token,
			"expiresAt": bson.M{"$gt": now},
		},
		bson.M{"$set": bson.M{
			"expiresAt": now.Add(ttl),
			"updatedAt": now,
		}},
	)
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

func (s *MongoLockStore) Release(ctx context.Context, matchID, ownerID, token string) error {
	ctx, cancel := lockTimeout(ctx)
	defer cancel()
	_, err := s.col.DeleteOne(ctx, bson.M{
		"_id":     matchID,
		"ownerId": ownerID,
		"token":   token,
	})
	return err
}

func (s *MongoLockStore) Get(ctx context.Context, matchID string) (*LockRecord, bool, error) {
	ctx, cancel := lockTimeout(ctx)
	defer cancel()

	var record LockRecord
	err := s.col.FindOne(ctx, bson.M{"_id": matchID}).Decode(&record)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &record, true, nil
}

func validateLockArgs(matchID, ownerID, token string, ttl time.Duration) error {
	if strings.TrimSpace(matchID) == "" {
		return errors.New("matchID is required")
	}
	if strings.TrimSpace(ownerID) == "" {
		return errors.New("ownerID is required")
	}
	if strings.TrimSpace(token) == "" {
		return errors.New("token is required")
	}
	if ttl <= 0 {
		return errors.New("ttl must be positive")
	}
	return nil
}

func newLeaseToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func lockTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 5*time.Second)
}
