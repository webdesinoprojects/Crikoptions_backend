package challenges

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
)

type CloseEvent struct {
	FillID      string                `bson:"_id"`
	UserID      primitive.ObjectID    `bson:"userId"`
	DateUTC     string                `bson:"dateUTC"`
	RealizedPnL float64               `bson:"realizedPnL"`
	Open        executions.MatchClock `bson:"open"`
	Close       executions.MatchClock `bson:"close"`
	CreatedAt   time.Time             `bson:"createdAt"`
}

type DailyProgress struct {
	UserID      primitive.ObjectID `bson:"userId"`
	ChallengeID string             `bson:"challengeId"`
	DateUTC     string             `bson:"dateUTC"`
	Progress    int                `bson:"progress"`
	Claimed     bool               `bson:"claimed"`
	FillIDs     []string           `bson:"fillIds,omitempty"`
	UpdatedAt   time.Time          `bson:"updatedAt"`
}

type DailyStore interface {
	EnsureIndexes(ctx context.Context) error
	RecordClose(ctx context.Context, ev CloseEvent) error
	Increment(ctx context.Context, userID primitive.ObjectID, challengeID, dateUTC, fillID string, target int) (DailyProgress, error)
	Get(ctx context.Context, userID primitive.ObjectID, challengeID, dateUTC string) (DailyProgress, error)
	ListForDate(ctx context.Context, userID primitive.ObjectID, dateUTC string) ([]DailyProgress, error)
	MarkClaimed(ctx context.Context, userID primitive.ObjectID, challengeID, dateUTC string) error
}

type MemoryDailyStore struct {
	mu       sync.Mutex
	events   map[string]CloseEvent
	progress map[string]DailyProgress
}

func NewMemoryDailyStore() *MemoryDailyStore {
	return &MemoryDailyStore{
		events:   make(map[string]CloseEvent),
		progress: make(map[string]DailyProgress),
	}
}

func (s *MemoryDailyStore) EnsureIndexes(context.Context) error { return nil }

func (s *MemoryDailyStore) RecordClose(_ context.Context, ev CloseEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.FillID == "" {
		return errors.New("missing fill id")
	}
	if _, exists := s.events[ev.FillID]; exists {
		return nil
	}
	s.events[ev.FillID] = ev
	return nil
}

func (s *MemoryDailyStore) Increment(_ context.Context, userID primitive.ObjectID, challengeID, dateUTC, fillID string, target int) (DailyProgress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := dailyKey(userID, challengeID, dateUTC)
	row := s.progress[key]
	if row.ChallengeID == "" {
		row = DailyProgress{
			UserID:      userID,
			ChallengeID: challengeID,
			DateUTC:     dateUTC,
		}
	}
	if containsString(row.FillIDs, fillID) {
		s.progress[key] = row
		return row, nil
	}
	row.FillIDs = append(row.FillIDs, fillID)
	if target <= 0 {
		target = row.Progress + 1
	}
	if row.Progress < target {
		row.Progress++
	}
	row.UpdatedAt = time.Now().UTC()
	s.progress[key] = row
	return row, nil
}

func (s *MemoryDailyStore) Get(_ context.Context, userID primitive.ObjectID, challengeID, dateUTC string) (DailyProgress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.progress[dailyKey(userID, challengeID, dateUTC)], nil
}

func (s *MemoryDailyStore) ListForDate(_ context.Context, userID primitive.ObjectID, dateUTC string) ([]DailyProgress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DailyProgress, 0, 4)
	for _, row := range s.progress {
		if row.UserID == userID && row.DateUTC == dateUTC {
			out = append(out, row)
		}
	}
	return out, nil
}

func (s *MemoryDailyStore) MarkClaimed(_ context.Context, userID primitive.ObjectID, challengeID, dateUTC string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := dailyKey(userID, challengeID, dateUTC)
	row := s.progress[key]
	if row.ChallengeID == "" {
		row = DailyProgress{UserID: userID, ChallengeID: challengeID, DateUTC: dateUTC}
	}
	row.Claimed = true
	row.UpdatedAt = time.Now().UTC()
	s.progress[key] = row
	return nil
}

func dailyKey(userID primitive.ObjectID, challengeID, dateUTC string) string {
	return userID.Hex() + "|" + challengeID + "|" + dateUTC
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

type MongoDailyStore struct {
	events   *mongo.Collection
	progress *mongo.Collection
}

func NewMongoDailyStore(db *mongo.Database) *MongoDailyStore {
	return &MongoDailyStore{
		events:   db.Collection("challenge_close_events"),
		progress: db.Collection("challenge_daily_progress"),
	}
}

func (s *MongoDailyStore) EnsureIndexes(ctx context.Context) error {
	ctx, cancel := dailyTimeoutCtx(ctx)
	defer cancel()
	if _, err := s.events.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "userId", Value: 1}, {Key: "dateUTC", Value: 1}},
	}); err != nil {
		return err
	}
	_, err := s.progress.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{
			{Key: "userId", Value: 1},
			{Key: "challengeId", Value: 1},
			{Key: "dateUTC", Value: 1},
		},
		Options: options.Index().SetUnique(true).SetName("unique_daily_challenge_progress"),
	})
	return err
}

func (s *MongoDailyStore) RecordClose(ctx context.Context, ev CloseEvent) error {
	ctx, cancel := dailyTimeoutCtx(ctx)
	defer cancel()
	if ev.FillID == "" {
		return errors.New("missing fill id")
	}
	_, err := s.events.InsertOne(ctx, ev)
	if mongo.IsDuplicateKeyError(err) {
		return nil
	}
	return err
}

func (s *MongoDailyStore) Increment(ctx context.Context, userID primitive.ObjectID, challengeID, dateUTC, fillID string, target int) (DailyProgress, error) {
	ctx, cancel := dailyTimeoutCtx(ctx)
	defer cancel()
	if target <= 0 {
		target = 1
	}
	now := time.Now().UTC()
	filter := bson.M{
		"userId":      userID,
		"challengeId": challengeID,
		"dateUTC":     dateUTC,
		"fillIds":     bson.M{"$ne": fillID},
	}
	update := bson.M{
		"$inc":      bson.M{"progress": 1},
		"$addToSet": bson.M{"fillIds": fillID},
		"$set":      bson.M{"updatedAt": now},
		"$setOnInsert": bson.M{
			"userId":      userID,
			"challengeId": challengeID,
			"dateUTC":     dateUTC,
			"claimed":     false,
		},
	}
	opts := options.FindOneAndUpdate().
		SetUpsert(true).
		SetReturnDocument(options.After)
	var row DailyProgress
	err := s.progress.FindOneAndUpdate(ctx, filter, update, opts).Decode(&row)
	if err == nil {
		if row.Progress > target {
			_, _ = s.progress.UpdateOne(ctx, bson.M{
				"userId": userID, "challengeId": challengeID, "dateUTC": dateUTC,
			}, bson.M{"$min": bson.M{"progress": target}, "$set": bson.M{"updatedAt": now}})
			row.Progress = target
		}
		return row, nil
	}
	if mongo.IsDuplicateKeyError(err) || errors.Is(err, mongo.ErrNoDocuments) {
		return s.Get(ctx, userID, challengeID, dateUTC)
	}
	return DailyProgress{}, err
}

func (s *MongoDailyStore) Get(ctx context.Context, userID primitive.ObjectID, challengeID, dateUTC string) (DailyProgress, error) {
	ctx, cancel := dailyTimeoutCtx(ctx)
	defer cancel()
	var row DailyProgress
	err := s.progress.FindOne(ctx, bson.M{
		"userId":      userID,
		"challengeId": challengeID,
		"dateUTC":     dateUTC,
	}).Decode(&row)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return DailyProgress{UserID: userID, ChallengeID: challengeID, DateUTC: dateUTC}, nil
	}
	if err != nil {
		return DailyProgress{}, err
	}
	return row, nil
}

func (s *MongoDailyStore) ListForDate(ctx context.Context, userID primitive.ObjectID, dateUTC string) ([]DailyProgress, error) {
	ctx, cancel := dailyTimeoutCtx(ctx)
	defer cancel()
	cur, err := s.progress.Find(ctx, bson.M{"userId": userID, "dateUTC": dateUTC})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []DailyProgress
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *MongoDailyStore) MarkClaimed(ctx context.Context, userID primitive.ObjectID, challengeID, dateUTC string) error {
	ctx, cancel := dailyTimeoutCtx(ctx)
	defer cancel()
	now := time.Now().UTC()
	_, err := s.progress.UpdateOne(ctx,
		bson.M{"userId": userID, "challengeId": challengeID, "dateUTC": dateUTC},
		bson.M{
			"$set": bson.M{"claimed": true, "updatedAt": now},
			"$setOnInsert": bson.M{
				"userId":      userID,
				"challengeId": challengeID,
				"dateUTC":     dateUTC,
				"progress":    0,
			},
		},
		options.Update().SetUpsert(true),
	)
	return err
}

func dailyTimeoutCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 5*time.Second)
}
