package executions

import (
	"context"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Repository interface {
	Create(ctx context.Context, exec Execution) (*Execution, error)
	GetByID(ctx context.Context, id primitive.ObjectID) (*Execution, error)
	List(ctx context.Context, filter Filter) []Execution
	ListWithError(ctx context.Context, filter Filter) ([]Execution, error)
	EnsureIndexes(ctx context.Context) error
}

type MemoryRepository struct {
	items []Execution
	mu    sync.RWMutex
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{}
}

func (r *MemoryRepository) Create(_ context.Context, exec Execution) (*Execution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if exec.ID.IsZero() {
		exec.ID = primitive.NewObjectID()
	}
	now := time.Now().UTC()
	if exec.CreatedAt.IsZero() {
		exec.CreatedAt = now
	}
	if exec.LiquiditySource == "" {
		exec.LiquiditySource = LiquiditySystemMarketMaker
	}
	r.items = append(r.items, exec)
	out := exec
	return &out, nil
}

func (r *MemoryRepository) GetByID(_ context.Context, id primitive.ObjectID) (*Execution, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := range r.items {
		if r.items[i].ID == id {
			out := r.items[i]
			return &out, nil
		}
	}
	return nil, nil
}

func (r *MemoryRepository) List(_ context.Context, filter Filter) []Execution {
	items, _ := r.ListWithError(context.Background(), filter)
	return items
}

func (r *MemoryRepository) ListWithError(_ context.Context, filter Filter) ([]Execution, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	limit := normalizedLimit(filter.Limit)
	out := make([]Execution, 0)
	for i := len(r.items) - 1; i >= 0; i-- {
		e := r.items[i]
		if !filter.UserID.IsZero() && e.UserID != filter.UserID {
			continue
		}
		if filter.MatchID != "" && e.MatchID != filter.MatchID {
			continue
		}
		if filter.MarketID != "" && e.MarketID != filter.MarketID {
			continue
		}
		if !filter.OrderID.IsZero() && e.OrderID != filter.OrderID {
			continue
		}
		if filter.ExcludeLiquiditySource != "" && e.LiquiditySource == filter.ExcludeLiquiditySource {
			continue
		}
		out = append(out, e)
		if int64(len(out)) >= limit {
			break
		}
	}
	return out, nil
}

func (r *MemoryRepository) EnsureIndexes(_ context.Context) error {
	return nil
}

type MongoRepository struct {
	col *mongo.Collection
}

func NewMongoRepository(db *mongo.Database) *MongoRepository {
	return &MongoRepository{col: db.Collection("executions")}
}

func (r *MongoRepository) EnsureIndexes(ctx context.Context) error {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	_, err := r.col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "userId", Value: 1}, {Key: "createdAt", Value: -1}}},
		{Keys: bson.D{{Key: "orderId", Value: 1}}},
		{Keys: bson.D{{Key: "matchId", Value: 1}, {Key: "marketId", Value: 1}, {Key: "createdAt", Value: -1}}},
	})
	return err
}

func (r *MongoRepository) Create(ctx context.Context, exec Execution) (*Execution, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	if exec.ID.IsZero() {
		exec.ID = primitive.NewObjectID()
	}
	if exec.CreatedAt.IsZero() {
		exec.CreatedAt = time.Now().UTC()
	}
	if exec.LiquiditySource == "" {
		exec.LiquiditySource = LiquiditySystemMarketMaker
	}
	if _, err := r.col.InsertOne(ctx, exec); err != nil {
		return nil, err
	}
	return &exec, nil
}

func (r *MongoRepository) GetByID(ctx context.Context, id primitive.ObjectID) (*Execution, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	var exec Execution
	err := r.col.FindOne(ctx, bson.M{"_id": id}).Decode(&exec)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return &exec, nil
}

func (r *MongoRepository) List(ctx context.Context, filter Filter) []Execution {
	items, _ := r.ListWithError(ctx, filter)
	return items
}

func (r *MongoRepository) ListWithError(ctx context.Context, filter Filter) ([]Execution, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	mongoFilter := bson.M{}
	if !filter.UserID.IsZero() {
		mongoFilter["userId"] = filter.UserID
	}
	if filter.MatchID != "" {
		mongoFilter["matchId"] = filter.MatchID
	}
	if filter.MarketID != "" {
		mongoFilter["marketId"] = filter.MarketID
	}
	if !filter.OrderID.IsZero() {
		mongoFilter["orderId"] = filter.OrderID
	}
	if filter.ExcludeLiquiditySource != "" {
		mongoFilter["liquiditySource"] = bson.M{"$ne": filter.ExcludeLiquiditySource}
	}

	cur, err := r.col.Find(
		ctx,
		mongoFilter,
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(normalizedLimit(filter.Limit)),
	)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var out []Execution
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func timeoutCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 5*time.Second)
}

func normalizedLimit(limit int64) int64 {
	if limit <= 0 {
		return 100
	}
	if limit > 10000 {
		return 10000
	}
	return limit
}
