package matches

import (
	"context"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Ball-event extras. nil/"" means a legal delivery.
const (
	ExtraWide    = "wide"
	ExtraNoBall  = "noball"
	ExtraBye     = "bye"
	ExtraLegBye  = "legbye"
	ExtraPenalty = "penalty"
)

type DeliveryExtras struct {
	Wides     int `json:"wides" bson:"wides"`
	NoBalls   int `json:"noBalls" bson:"noBalls"`
	Byes      int `json:"byes" bson:"byes"`
	LegByes   int `json:"legByes" bson:"legByes"`
	Penalties int `json:"penalties" bson:"penalties"`
}

func (e DeliveryExtras) Total() int {
	return e.Wides + e.NoBalls + e.Byes + e.LegByes + e.Penalties
}

type Dismissal struct {
	Kind         string `json:"kind,omitempty" bson:"kind,omitempty"`
	PlayerID     int64  `json:"playerId,omitempty" bson:"playerId,omitempty"`
	FielderID    int64  `json:"fielderId,omitempty" bson:"fielderId,omitempty"`
	BowlerCredit bool   `json:"bowlerCredit" bson:"bowlerCredit"`
}

// BallEvent is one persisted delivery for a match, used to reconstruct the
// "This over" view for clients that join after balls were already bowled.
type BallEvent struct {
	ID          primitive.ObjectID `json:"_id" bson:"_id,omitempty"`
	MatchID     string             `json:"matchId" bson:"matchId"`
	Innings     int                `json:"innings" bson:"innings"`
	Over        int                `json:"over" bson:"over"`
	Ball        int                `json:"ball" bson:"ball"`
	LegalBall   bool               `json:"legalBall" bson:"legalBall"`
	Runs        int                `json:"runs" bson:"runs"`
	IsWicket    bool               `json:"isWicket" bson:"isWicket"`
	Extra       *string            `json:"extra" bson:"extra"`
	StrikerName string             `json:"strikerName,omitempty" bson:"strikerName,omitempty"`
	BowlerName  string             `json:"bowlerName,omitempty" bson:"bowlerName,omitempty"`
	Commentary  string             `json:"commentary,omitempty" bson:"commentary,omitempty"`

	Provider           string         `json:"provider,omitempty" bson:"provider,omitempty"`
	ProviderFixtureID  int64          `json:"providerFixtureId,omitempty" bson:"providerFixtureId,omitempty"`
	ProviderEventID    string         `json:"providerEventId,omitempty" bson:"providerEventId,omitempty"`
	ProviderScoreID    int64          `json:"providerScoreId,omitempty" bson:"providerScoreId,omitempty"`
	ProviderBall       string         `json:"providerBall,omitempty" bson:"providerBall,omitempty"`
	Sequence           int64          `json:"sequence,omitempty" bson:"sequence,omitempty"`
	Revision           int64          `json:"revision,omitempty" bson:"revision,omitempty"`
	PayloadHash        string         `json:"-" bson:"payloadHash,omitempty"`
	TeamRuns           int            `json:"teamRuns,omitempty" bson:"teamRuns,omitempty"`
	BatterRuns         int            `json:"batterRuns,omitempty" bson:"batterRuns,omitempty"`
	Extras             DeliveryExtras `json:"extras,omitempty" bson:"extras,omitempty"`
	Dismissal          *Dismissal     `json:"dismissal,omitempty" bson:"dismissal,omitempty"`
	Active             bool           `json:"active,omitempty" bson:"active,omitempty"`
	Tombstoned         bool           `json:"tombstoned,omitempty" bson:"tombstoned,omitempty"`
	SupersededRevision int64          `json:"supersededRevision,omitempty" bson:"supersededRevision,omitempty"`
	MissingPolls       int            `json:"-" bson:"missingPolls,omitempty"`
	ProviderUpdatedAt  *time.Time     `json:"providerUpdatedAt,omitempty" bson:"providerUpdatedAt,omitempty"`
	ReceivedAt         *time.Time     `json:"receivedAt,omitempty" bson:"receivedAt,omitempty"`
	CreatedAt          time.Time      `json:"createdAt" bson:"createdAt"`
}

// EventRepository persists and reads per-ball events. AppendEvent must preserve
// insertion order; RecentEvents returns events oldest → newest.
type EventRepository interface {
	AppendEvent(ctx context.Context, event BallEvent) error
	LegalBallCount(ctx context.Context, matchID string, innings int) (int, error)
	EventCount(ctx context.Context, matchID string, innings int) (int, error)
	RecentEvents(ctx context.Context, matchID string, innings, limit int) ([]BallEvent, error)
	InningsEvents(ctx context.Context, matchID string, innings, limit int) ([]BallEvent, error)
	DeleteByMatchID(ctx context.Context, matchID string) error
	EnsureIndexes(ctx context.Context) error
}

type CursorEventRepository interface {
	InningsEventsAfter(ctx context.Context, matchID string, innings int, afterSequence int64, limit int) ([]BallEvent, error)
}

// recentFromDescending takes events ordered newest → oldest and returns the
// last `limit` legal deliveries plus any extras interleaved among them, in
// chronological (oldest → newest) order.
func recentFromDescending(desc []BallEvent, limit int) []BallEvent {
	if limit <= 0 {
		limit = 6
	}
	collected := make([]BallEvent, 0, limit*2)
	legal := 0
	for _, e := range desc {
		collected = append(collected, e)
		if e.LegalBall {
			legal++
			if legal >= limit {
				break
			}
		}
	}
	// Reverse to chronological order.
	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	return collected
}

// MemoryEventRepository is an in-memory EventRepository for tests/dev.
type MemoryEventRepository struct {
	mu     sync.RWMutex
	events []BallEvent
	seq    int64
}

func NewMemoryEventRepository() *MemoryEventRepository {
	return &MemoryEventRepository{}
}

func (r *MemoryEventRepository) AppendEvent(_ context.Context, event BallEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	r.seq++
	if event.ID.IsZero() {
		event.ID = primitive.NewObjectID()
	}
	r.events = append(r.events, event)
	return nil
}

func (r *MemoryEventRepository) LegalBallCount(_ context.Context, matchID string, innings int) (int, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, e := range r.events {
		if e.MatchID == matchID && e.Innings == innings && visibleEvent(e) && e.LegalBall {
			count++
		}
	}
	return count, nil
}

func (r *MemoryEventRepository) EventCount(_ context.Context, matchID string, innings int) (int, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, e := range r.events {
		if e.MatchID == matchID && e.Innings == innings && visibleEvent(e) {
			count++
		}
	}
	return count, nil
}

func (r *MemoryEventRepository) RecentEvents(_ context.Context, matchID string, innings, limit int) ([]BallEvent, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var filtered []BallEvent
	for _, e := range r.events {
		if e.MatchID == matchID && e.Innings == innings && visibleEvent(e) {
			filtered = append(filtered, e)
		}
	}
	// Insertion order is chronological; build a descending copy.
	desc := make([]BallEvent, len(filtered))
	for i := range filtered {
		desc[len(filtered)-1-i] = filtered[i]
	}
	return recentFromDescending(desc, limit), nil
}

func (r *MemoryEventRepository) InningsEvents(_ context.Context, matchID string, innings, limit int) ([]BallEvent, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if limit <= 0 {
		limit = len(r.events)
	}

	filtered := make([]BallEvent, 0, limit)
	for _, e := range r.events {
		if e.MatchID != matchID || e.Innings != innings || !visibleEvent(e) {
			continue
		}
		filtered = append(filtered, e)
		if len(filtered) >= limit {
			break
		}
	}
	return filtered, nil
}

func (r *MemoryEventRepository) InningsEventsAfter(_ context.Context, matchID string, innings int, afterSequence int64, limit int) ([]BallEvent, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if limit <= 0 {
		limit = len(r.events)
	}
	filtered := make([]BallEvent, 0, limit)
	for _, event := range r.events {
		if event.MatchID != matchID || event.Innings != innings || event.Sequence <= afterSequence || !visibleEvent(event) {
			continue
		}
		filtered = append(filtered, event)
		if len(filtered) == limit {
			break
		}
	}
	return filtered, nil
}

func (r *MemoryEventRepository) DeleteByMatchID(_ context.Context, matchID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	keep := r.events[:0]
	for _, e := range r.events {
		if e.MatchID != matchID {
			keep = append(keep, e)
		}
	}
	r.events = keep
	return nil
}

func (r *MemoryEventRepository) EnsureIndexes(_ context.Context) error { return nil }

// MongoEventRepository persists ball events in the match_events collection.
type MongoEventRepository struct {
	col *mongo.Collection
}

func NewMongoEventRepository(db *mongo.Database) *MongoEventRepository {
	return &MongoEventRepository{col: db.Collection("match_events")}
}

func (r *MongoEventRepository) EnsureIndexes(ctx context.Context) error {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()
	_, err := r.col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "matchId", Value: 1}, {Key: "innings", Value: 1}, {Key: "_id", Value: 1}}},
		{Keys: bson.D{{Key: "matchId", Value: 1}, {Key: "innings", Value: 1}, {Key: "createdAt", Value: 1}, {Key: "_id", Value: 1}}},
		{
			Keys: bson.D{{Key: "provider", Value: 1}, {Key: "providerFixtureId", Value: 1}, {Key: "providerEventId", Value: 1}},
			Options: options.Index().SetUnique(true).SetPartialFilterExpression(bson.M{
				"provider":          bson.M{"$type": "string"},
				"providerFixtureId": bson.M{"$type": "long"},
				"providerEventId":   bson.M{"$type": "string"},
			}),
		},
		{Keys: bson.D{{Key: "matchId", Value: 1}, {Key: "innings", Value: 1}, {Key: "sequence", Value: 1}}},
	})
	return err
}

func (r *MongoEventRepository) AppendEvent(ctx context.Context, event BallEvent) error {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()
	if event.ID.IsZero() {
		event.ID = primitive.NewObjectID()
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	_, err := r.col.InsertOne(ctx, event)
	return err
}

func (r *MongoEventRepository) LegalBallCount(ctx context.Context, matchID string, innings int) (int, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()
	filter := visibleEventFilter(matchID, innings)
	filter["legalBall"] = true
	count, err := r.col.CountDocuments(ctx, filter)
	if err != nil {
		return 0, err
	}
	return int(count), nil
}

func (r *MongoEventRepository) EventCount(ctx context.Context, matchID string, innings int) (int, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()
	count, err := r.col.CountDocuments(ctx, visibleEventFilter(matchID, innings))
	if err != nil {
		return 0, err
	}
	return int(count), nil
}

func (r *MongoEventRepository) DeleteByMatchID(ctx context.Context, matchID string) error {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()
	_, err := r.col.DeleteMany(ctx, bson.M{"matchId": matchID})
	return err
}

func (r *MongoEventRepository) RecentEvents(ctx context.Context, matchID string, innings, limit int) ([]BallEvent, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	if limit <= 0 {
		limit = 6
	}
	// Over-fetch generously to be sure we capture `limit` legal balls plus any
	// extras; an over rarely exceeds a handful of extras.
	fetch := int64(limit*3 + 12)

	cur, err := r.col.Find(
		ctx,
		visibleEventFilter(matchID, innings),
		options.Find().SetSort(bson.D{{Key: "sequence", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(fetch),
	)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var desc []BallEvent
	if err := cur.All(ctx, &desc); err != nil {
		return nil, err
	}
	return recentFromDescending(desc, limit), nil
}

func (r *MongoEventRepository) InningsEvents(ctx context.Context, matchID string, innings, limit int) ([]BallEvent, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	if limit <= 0 {
		limit = 240
	}

	cur, err := r.col.Find(
		ctx,
		visibleEventFilter(matchID, innings),
		options.Find().
			SetSort(bson.D{{Key: "sequence", Value: 1}, {Key: "createdAt", Value: 1}, {Key: "_id", Value: 1}}).
			SetLimit(int64(limit)),
	)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var events []BallEvent
	if err := cur.All(ctx, &events); err != nil {
		return nil, err
	}
	return events, nil
}

func (r *MongoEventRepository) InningsEventsAfter(ctx context.Context, matchID string, innings int, afterSequence int64, limit int) ([]BallEvent, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()
	if limit <= 0 {
		limit = 100
	}
	filter := visibleEventFilter(matchID, innings)
	filter["sequence"] = bson.M{"$gt": afterSequence}
	cur, err := r.col.Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "sequence", Value: 1}, {Key: "_id", Value: 1}}).
		SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var events []BallEvent
	if err := cur.All(ctx, &events); err != nil {
		return nil, err
	}
	return events, nil
}

func visibleEvent(event BallEvent) bool {
	return event.Provider == "" || (event.Active && !event.Tombstoned)
}

func visibleEventFilter(matchID string, innings int) bson.M {
	return bson.M{
		"matchId": matchID,
		"innings": innings,
		"$or": bson.A{
			bson.M{"provider": bson.M{"$exists": false}},
			bson.M{"provider": ""},
			bson.M{"active": true, "tombstoned": bson.M{"$ne": true}},
		},
	}
}
