package positions

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
)

type ProjectionFilter struct {
	UserID   primitive.ObjectID
	MatchID  string
	MarketID string
	Status   string
}

type ProjectionRepository interface {
	ApplyExecution(ctx context.Context, exec executions.Execution) error
	List(ctx context.Context, filter ProjectionFilter) ([]PositionProjection, error)
	GetByID(ctx context.Context, userID primitive.ObjectID, id string) (*PositionProjection, error)
	GetOpenByKey(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) (*PositionProjection, error)
	Clear(ctx context.Context) error
	EnsureIndexes(ctx context.Context) error
}

type PositionProjection struct {
	ID           string             `bson:"_id"`
	UserID       primitive.ObjectID `bson:"userId"`
	MatchID      string             `bson:"matchId"`
	MarketID     string             `bson:"marketId"`
	Strike       float64            `bson:"strike"`
	Status       string             `bson:"status"`
	Lots         int                `bson:"lots"`
	BuyLots      int                `bson:"buyLots"`
	BuyNotional  float64            `bson:"buyNotional"`
	SellLots     int                `bson:"sellLots"`
	SellNotional float64            `bson:"sellNotional"`
	BuyPrice     float64            `bson:"buyPrice"`
	SellPrice    float64            `bson:"sellPrice"`
	MatchedLots  int                `bson:"matchedLots"`
	RealizedPnL  float64            `bson:"realizedPnl"`
	CreatedAt    time.Time          `bson:"createdAt"`
	UpdatedAt    time.Time          `bson:"updatedAt"`
}

func (p PositionProjection) ToPosition(ltp float64) Position {
	position := Position{
		ID:          p.ID,
		UserID:      p.UserID,
		MatchID:     p.MatchID,
		MarketID:    p.MarketID,
		Strike:      p.Strike,
		Status:      p.Status,
		Lots:        p.Lots,
		BuyPrice:    round2(p.BuyPrice),
		SellPrice:   round2(p.SellPrice),
		LTP:         round2(ltp),
		RealizedPnL: round2(p.RealizedPnL),
		MatchedLots: p.MatchedLots,
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
	}
	position.PnL = computePnL(position, position.MatchedLots)
	if position.Status == "closed" && position.RealizedPnL != 0 {
		position.PnL = position.RealizedPnL
	}
	return position
}

func (p *PositionProjection) apply(exec executions.Execution) {
	createdAt := exec.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	if p.ID == "" {
		p.ID = derivePositionID(exec.UserID, exec.MatchID, exec.MarketID, exec.Strike, createdAt)
		p.UserID = exec.UserID
		p.MatchID = exec.MatchID
		p.MarketID = exec.MarketID
		p.Strike = exec.Strike
		p.CreatedAt = createdAt
	}

	switch strings.ToLower(strings.TrimSpace(exec.Side)) {
	case "buy":
		p.BuyLots += exec.Quantity
		p.BuyNotional += exec.Price * float64(exec.Quantity)
	case "sell":
		p.SellLots += exec.Quantity
		p.SellNotional += exec.Price * float64(exec.Quantity)
	}

	p.Lots = p.BuyLots - p.SellLots
	if p.Lots == 0 {
		p.Status = "closed"
	} else {
		p.Status = "open"
	}
	if p.BuyLots > 0 {
		p.BuyPrice = round2(p.BuyNotional / float64(p.BuyLots))
	}
	if p.SellLots > 0 {
		p.SellPrice = round2(p.SellNotional / float64(p.SellLots))
	}
	p.MatchedLots = minInt(p.BuyLots, p.SellLots)
	if p.MatchedLots > 0 && p.BuyLots > 0 && p.SellLots > 0 {
		avgBuy := p.BuyNotional / float64(p.BuyLots)
		avgSell := p.SellNotional / float64(p.SellLots)
		p.RealizedPnL = round2((avgSell - avgBuy) * float64(p.MatchedLots))
	}
	if p.UpdatedAt.IsZero() || createdAt.After(p.UpdatedAt) {
		p.UpdatedAt = createdAt
	}
}

type MemoryProjectionRepository struct {
	items map[string]PositionProjection
	mu    sync.RWMutex
}

func NewMemoryProjectionRepository() *MemoryProjectionRepository {
	return &MemoryProjectionRepository{items: make(map[string]PositionProjection)}
}

func (r *MemoryProjectionRepository) ApplyExecution(_ context.Context, exec executions.Execution) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var projection PositionProjection
	var found bool
	for _, item := range r.items {
		if projectionMatchesOpenKey(item, exec.UserID, exec.MatchID, exec.MarketID, exec.Strike) {
			projection = item
			found = true
			break
		}
	}
	projection.apply(exec)
	r.items[projection.ID] = projection
	if !found && len(r.items) == 0 {
		r.items[projection.ID] = projection
	}
	return nil
}

func (r *MemoryProjectionRepository) List(_ context.Context, filter ProjectionFilter) ([]PositionProjection, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]PositionProjection, 0, len(r.items))
	for _, item := range r.items {
		if !projectionMatchesFilter(item, filter) {
			continue
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

func (r *MemoryProjectionRepository) GetByID(_ context.Context, userID primitive.ObjectID, id string) (*PositionProjection, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	item, ok := r.items[id]
	if !ok || (!userID.IsZero() && item.UserID != userID) {
		return nil, nil
	}
	out := item
	return &out, nil
}

func (r *MemoryProjectionRepository) GetOpenByKey(_ context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) (*PositionProjection, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, item := range r.items {
		if projectionMatchesOpenKey(item, userID, matchID, marketID, strike) {
			out := item
			return &out, nil
		}
	}
	return nil, nil
}

func (r *MemoryProjectionRepository) Clear(_ context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = make(map[string]PositionProjection)
	return nil
}

func (r *MemoryProjectionRepository) EnsureIndexes(_ context.Context) error {
	return nil
}

type MongoProjectionRepository struct {
	col *mongo.Collection
}

func NewMongoProjectionRepository(db *mongo.Database) *MongoProjectionRepository {
	return &MongoProjectionRepository{col: db.Collection("position_projections")}
}

func (r *MongoProjectionRepository) EnsureIndexes(ctx context.Context) error {
	ctx, cancel := projectionTimeoutCtx(ctx)
	defer cancel()

	_, err := r.col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys: bson.D{{Key: "userId", Value: 1}, {Key: "status", Value: 1}, {Key: "updatedAt", Value: -1}},
		},
		{
			Keys: bson.D{{Key: "userId", Value: 1}, {Key: "matchId", Value: 1}, {Key: "marketId", Value: 1}, {Key: "status", Value: 1}},
		},
		{
			Keys: bson.D{{Key: "userId", Value: 1}, {Key: "matchId", Value: 1}, {Key: "marketId", Value: 1}, {Key: "strike", Value: 1}, {Key: "status", Value: 1}},
		},
	})
	return err
}

func (r *MongoProjectionRepository) ApplyExecution(ctx context.Context, exec executions.Execution) error {
	ctx, cancel := projectionTimeoutCtx(ctx)
	defer cancel()

	filter := bson.M{
		"userId":   exec.UserID,
		"matchId":  exec.MatchID,
		"marketId": exec.MarketID,
		"strike":   exec.Strike,
		"status":   "open",
	}

	var projection PositionProjection
	err := r.col.FindOne(ctx, filter).Decode(&projection)
	if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		return err
	}
	projection.apply(exec)

	_, err = r.col.ReplaceOne(
		ctx,
		bson.M{"_id": projection.ID},
		projection,
		options.Replace().SetUpsert(true),
	)
	return err
}

func (r *MongoProjectionRepository) List(ctx context.Context, filter ProjectionFilter) ([]PositionProjection, error) {
	ctx, cancel := projectionTimeoutCtx(ctx)
	defer cancel()

	cur, err := r.col.Find(
		ctx,
		projectionMongoFilter(filter),
		options.Find().SetSort(bson.D{{Key: "updatedAt", Value: -1}}),
	)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var out []PositionProjection
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *MongoProjectionRepository) GetByID(ctx context.Context, userID primitive.ObjectID, id string) (*PositionProjection, error) {
	ctx, cancel := projectionTimeoutCtx(ctx)
	defer cancel()

	filter := bson.M{"_id": strings.TrimSpace(id)}
	if !userID.IsZero() {
		filter["userId"] = userID
	}
	var projection PositionProjection
	err := r.col.FindOne(ctx, filter).Decode(&projection)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &projection, nil
}

func (r *MongoProjectionRepository) GetOpenByKey(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) (*PositionProjection, error) {
	ctx, cancel := projectionTimeoutCtx(ctx)
	defer cancel()

	var projection PositionProjection
	err := r.col.FindOne(ctx, bson.M{
		"userId":   userID,
		"matchId":  matchID,
		"marketId": marketID,
		"strike":   strike,
		"status":   "open",
	}).Decode(&projection)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &projection, nil
}

func (r *MongoProjectionRepository) Clear(ctx context.Context) error {
	ctx, cancel := projectionTimeoutCtx(ctx)
	defer cancel()
	_, err := r.col.DeleteMany(ctx, bson.M{})
	return err
}

func projectionMongoFilter(filter ProjectionFilter) bson.M {
	out := bson.M{}
	if !filter.UserID.IsZero() {
		out["userId"] = filter.UserID
	}
	if filter.MatchID != "" {
		out["matchId"] = filter.MatchID
	}
	if filter.MarketID != "" {
		out["marketId"] = filter.MarketID
	}
	if filter.Status != "" {
		out["status"] = filter.Status
	}
	return out
}

func projectionMatchesFilter(item PositionProjection, filter ProjectionFilter) bool {
	if !filter.UserID.IsZero() && item.UserID != filter.UserID {
		return false
	}
	if filter.MatchID != "" && item.MatchID != filter.MatchID {
		return false
	}
	if filter.MarketID != "" && item.MarketID != filter.MarketID {
		return false
	}
	if filter.Status != "" && item.Status != filter.Status {
		return false
	}
	return true
}

func projectionMatchesOpenKey(item PositionProjection, userID primitive.ObjectID, matchID, marketID string, strike float64) bool {
	return item.Status == "open" &&
		item.UserID == userID &&
		item.MatchID == matchID &&
		item.MarketID == marketID &&
		item.Strike == strike
}

func projectionTimeoutCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 5*time.Second)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
