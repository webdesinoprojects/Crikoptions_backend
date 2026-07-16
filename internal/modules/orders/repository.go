package orders

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

type OrderFilter struct {
	UserID   primitive.ObjectID
	MatchID  string
	MarketID string
	Side     string
	Status   string
}

type Repository interface {
	GetByUserID(ctx context.Context, userID primitive.ObjectID, status, matchID string) []Order
	GetByID(ctx context.Context, id primitive.ObjectID) (*Order, error)
	FindByClientOrderID(ctx context.Context, userID primitive.ObjectID, clientOrderID string) (*Order, error)
	Create(ctx context.Context, order Order) (*Order, error)
	UpdateFill(ctx context.Context, id primitive.ObjectID, update FillUpdate) (*Order, error)
	Cancel(ctx context.Context, id primitive.ObjectID, userID primitive.ObjectID) (*Order, error)
	GetAll(ctx context.Context) []Order
	List(ctx context.Context, filter OrderFilter) []Order
	ListWithError(ctx context.Context, filter OrderFilter) ([]Order, error)
	EnsureIndexes(ctx context.Context) error
	DoTx(ctx context.Context, fn func(ctx context.Context) error) error
	FreezeProviderVoidCompensation(ctx context.Context, compensation ProviderVoidCompensation) (*ProviderVoidCompensation, error)
}

type ProviderVoidCompensation struct {
	ID            primitive.ObjectID `bson:"_id,omitempty"`
	MarketID      string             `bson:"marketId"`
	UserID        primitive.ObjectID `bson:"userId"`
	CashDelta     float64            `bson:"cashDelta"`
	ReservedDelta float64            `bson:"reservedDelta"`
	ExecutionHash string             `bson:"executionHash"`
	CreatedAt     time.Time          `bson:"createdAt"`
}

type FillUpdate struct {
	ExpectedFilledQuantity    int
	ExpectedRemainingQuantity int
	ExpectedStatus            string
	FilledQuantity            int
	RemainingQuantity         int
	AverageFillPrice          float64
	OutstandingReserve        float64
	ReserveReconciled         bool
	Status                    string
}

type MemoryRepository struct {
	orders            []Order
	voidCompensations map[string]ProviderVoidCompensation
	mu                sync.RWMutex
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		orders:            getSampleOrders(),
		voidCompensations: make(map[string]ProviderVoidCompensation),
	}
}

func (r *MemoryRepository) FreezeProviderVoidCompensation(_ context.Context, candidate ProviderVoidCompensation) (*ProviderVoidCompensation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := validateProviderVoidCompensation(candidate); err != nil {
		return nil, err
	}
	if r.voidCompensations == nil {
		r.voidCompensations = make(map[string]ProviderVoidCompensation)
	}
	key := candidate.MarketID + "|" + candidate.UserID.Hex()
	if existing, ok := r.voidCompensations[key]; ok {
		if existing.ExecutionHash != candidate.ExecutionHash || existing.CashDelta != candidate.CashDelta {
			return nil, errors.New("provider void compensation changed after it was frozen")
		}
		out := existing
		return &out, nil
	}
	if candidate.ID.IsZero() {
		candidate.ID = primitive.NewObjectID()
	}
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = time.Now().UTC()
	}
	r.voidCompensations[key] = candidate
	out := candidate
	return &out, nil
}

func (r *MemoryRepository) GetByUserID(ctx context.Context, userID primitive.ObjectID, status, matchID string) []Order {
	r.mu.RLock()
	defer r.mu.RUnlock()

	uid := userID.Hex()
	var result []Order
	for i := range r.orders {
		order := r.orders[i]
		if order.UserID.Hex() != uid {
			continue
		}
		if status != "" && order.Status != status {
			continue
		}
		if matchID != "" && order.MatchID != matchID {
			continue
		}
		result = append(result, order)
	}
	return result
}

func (r *MemoryRepository) GetByID(ctx context.Context, id primitive.ObjectID) (*Order, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for i := range r.orders {
		if r.orders[i].ID == id {
			return &r.orders[i], nil
		}
	}
	return nil, nil
}

func (r *MemoryRepository) Create(ctx context.Context, order Order) (*Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if order.ID.IsZero() {
		order.ID = primitive.NewObjectID()
	}
	order.Status = StatusOpen
	if order.Type == "" {
		order.Type = OrderTypeLimit
	}
	if order.RemainingQuantity == 0 {
		order.RemainingQuantity = order.Quantity
	}
	order.CreatedAt = time.Now().UTC()
	order.UpdatedAt = order.CreatedAt

	r.orders = append(r.orders, order)
	return &order, nil
}

func (r *MemoryRepository) Cancel(ctx context.Context, id primitive.ObjectID, userID primitive.ObjectID) (*Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	uid := userID.Hex()
	for i := range r.orders {
		if r.orders[i].ID == id {
			if r.orders[i].UserID.Hex() != uid {
				return nil, nil
			}
			if r.orders[i].Status != StatusOpen && r.orders[i].Status != StatusPartiallyFilled {
				return nil, nil
			}
			r.orders[i].Status = StatusCancelled
			r.orders[i].UpdatedAt = time.Now().UTC()
			return &r.orders[i], nil
		}
	}
	return nil, nil
}

func (r *MemoryRepository) GetAll(ctx context.Context) []Order {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.orders
}

func (r *MemoryRepository) List(ctx context.Context, f OrderFilter) []Order {
	orders, _ := r.ListWithError(ctx, f)
	return orders
}

func (r *MemoryRepository) ListWithError(_ context.Context, f OrderFilter) ([]Order, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []Order
	for i := range r.orders {
		o := r.orders[i]
		if !f.UserID.IsZero() && o.UserID != f.UserID {
			continue
		}
		if f.MatchID != "" && o.MatchID != f.MatchID {
			continue
		}
		if f.MarketID != "" && o.MarketID != f.MarketID {
			continue
		}
		if f.Side != "" && o.Side != f.Side {
			continue
		}
		if f.Status != "" && o.Status != f.Status {
			continue
		}
		out = append(out, o)
	}
	return out, nil
}

func (r *MemoryRepository) FindByClientOrderID(_ context.Context, userID primitive.ObjectID, clientOrderID string) (*Order, error) {
	if clientOrderID == "" {
		return nil, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := range r.orders {
		if r.orders[i].UserID == userID && r.orders[i].ClientOrderID == clientOrderID {
			out := r.orders[i]
			return &out, nil
		}
	}
	return nil, nil
}

func (r *MemoryRepository) UpdateFill(_ context.Context, id primitive.ObjectID, update FillUpdate) (*Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.orders {
		if r.orders[i].ID == id {
			if r.orders[i].FilledQuantity != update.ExpectedFilledQuantity ||
				r.orders[i].RemainingQuantity != update.ExpectedRemainingQuantity ||
				r.orders[i].Status != update.ExpectedStatus {
				return nil, nil
			}
			r.orders[i].FilledQuantity = update.FilledQuantity
			r.orders[i].RemainingQuantity = update.RemainingQuantity
			r.orders[i].AverageFillPrice = update.AverageFillPrice
			r.orders[i].OutstandingReserve = update.OutstandingReserve
			r.orders[i].ReserveReconciled = update.ReserveReconciled
			r.orders[i].Status = update.Status
			r.orders[i].UpdatedAt = time.Now().UTC()
			out := r.orders[i]
			return &out, nil
		}
	}
	return nil, nil
}

func (r *MemoryRepository) EnsureIndexes(ctx context.Context) error {
	return nil
}

func (r *MemoryRepository) DoTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

type MongoRepository struct {
	col               *mongo.Collection
	voidCompensations *mongo.Collection
}

func NewMongoRepository(db *mongo.Database) *MongoRepository {
	return &MongoRepository{
		col:               db.Collection("orders"),
		voidCompensations: db.Collection("provider_void_compensations"),
	}
}

func (r *MongoRepository) EnsureIndexes(ctx context.Context) error {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	// Legacy seeded orders share userId with null/missing clientOrderId. Unset those
	// fields so a partial unique index can be applied safely.
	_, _ = r.col.UpdateMany(ctx, bson.M{
		"$or": []bson.M{
			{"clientOrderId": nil},
			{"clientOrderId": ""},
			{"clientOrderId": bson.M{"$exists": false}},
		},
	}, bson.M{"$unset": bson.M{"clientOrderId": ""}})

	// Drop a previously attempted sparse unique index (may fail on legacy null rows).
	_, _ = r.col.Indexes().DropOne(ctx, "userId_1_clientOrderId_1")

	indexes := []mongo.IndexModel{
		{
			Keys: bson.D{{Key: "userId", Value: 1}, {Key: "status", Value: 1}, {Key: "matchId", Value: 1}},
		},
		{
			Keys: bson.D{{Key: "userId", Value: 1}, {Key: "clientOrderId", Value: 1}},
			Options: options.Index().SetUnique(true).SetPartialFilterExpression(bson.M{
				"clientOrderId": bson.M{"$exists": true, "$type": "string", "$gt": ""},
			}),
		},
	}
	if _, err := r.col.Indexes().CreateMany(ctx, indexes); err != nil {
		return err
	}
	_, err := r.voidCompensations.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "marketId", Value: 1}, {Key: "userId", Value: 1}},
		Options: options.Index().SetName("uniq_provider_void_compensation").SetUnique(true),
	})
	return err
}

func (r *MongoRepository) FreezeProviderVoidCompensation(ctx context.Context, candidate ProviderVoidCompensation) (*ProviderVoidCompensation, error) {
	if err := validateProviderVoidCompensation(candidate); err != nil {
		return nil, err
	}
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()
	if candidate.ID.IsZero() {
		candidate.ID = primitive.NewObjectID()
	}
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = time.Now().UTC()
	}
	filter := bson.M{"marketId": candidate.MarketID, "userId": candidate.UserID}
	res := r.voidCompensations.FindOneAndUpdate(
		ctx,
		filter,
		bson.M{"$setOnInsert": candidate},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	)
	var frozen ProviderVoidCompensation
	if err := res.Decode(&frozen); err != nil {
		if !mongo.IsDuplicateKeyError(err) {
			return nil, err
		}
		if err := r.voidCompensations.FindOne(ctx, filter).Decode(&frozen); err != nil {
			return nil, err
		}
	}
	if frozen.ExecutionHash != candidate.ExecutionHash || frozen.CashDelta != candidate.CashDelta {
		return nil, errors.New("provider void compensation changed after it was frozen")
	}
	return &frozen, nil
}

func validateProviderVoidCompensation(candidate ProviderVoidCompensation) error {
	if candidate.MarketID == "" || candidate.UserID.IsZero() || candidate.ExecutionHash == "" {
		return errors.New("invalid provider void compensation")
	}
	return nil
}

func (r *MongoRepository) DoTx(ctx context.Context, fn func(ctx context.Context) error) error {
	session, err := r.col.Database().Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)

	_, err = session.WithTransaction(ctx, func(sessCtx mongo.SessionContext) (interface{}, error) {
		return nil, fn(sessCtx)
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()))
	return err
}

func (r *MongoRepository) SeedDefaults(ctx context.Context) (int, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	count, err := r.col.CountDocuments(ctx, bson.M{})
	if err != nil {
		return 0, err
	}
	if count > 0 {
		return 0, nil
	}

	samples := getSampleOrders()
	docs := make([]any, 0, len(samples))
	for _, order := range samples {
		docs = append(docs, order)
	}
	if len(docs) == 0 {
		return 0, nil
	}
	if _, err := r.col.InsertMany(ctx, docs); err != nil {
		return 0, err
	}
	return len(docs), nil
}

func (r *MongoRepository) GetByUserID(ctx context.Context, userID primitive.ObjectID, status, matchID string) []Order {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	filter := bson.M{"userId": userID}
	if status != "" {
		filter["status"] = status
	}
	if matchID != "" {
		filter["matchId"] = matchID
	}

	cur, err := r.col.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}))
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)

	var out []Order
	if err := cur.All(ctx, &out); err != nil {
		return nil
	}
	return out
}

func (r *MongoRepository) GetByID(ctx context.Context, id primitive.ObjectID) (*Order, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	var order Order
	err := r.col.FindOne(ctx, bson.M{"_id": id}).Decode(&order)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &order, nil
}

func (r *MongoRepository) Create(ctx context.Context, order Order) (*Order, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	if order.ID.IsZero() {
		order.ID = primitive.NewObjectID()
	}
	order.Status = StatusOpen
	if order.Type == "" {
		order.Type = OrderTypeLimit
	}
	if order.RemainingQuantity == 0 {
		order.RemainingQuantity = order.Quantity
	}
	now := time.Now().UTC()
	order.CreatedAt = now
	order.UpdatedAt = now

	if _, err := r.col.InsertOne(ctx, order); err != nil {
		return nil, err
	}
	return &order, nil
}

func (r *MongoRepository) FindByClientOrderID(ctx context.Context, userID primitive.ObjectID, clientOrderID string) (*Order, error) {
	if clientOrderID == "" {
		return nil, nil
	}
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	var order Order
	err := r.col.FindOne(ctx, bson.M{"userId": userID, "clientOrderId": clientOrderID}).Decode(&order)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &order, nil
}

func (r *MongoRepository) UpdateFill(ctx context.Context, id primitive.ObjectID, update FillUpdate) (*Order, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	res := r.col.FindOneAndUpdate(
		ctx,
		bson.M{
			"_id":               id,
			"filledQuantity":    update.ExpectedFilledQuantity,
			"remainingQuantity": update.ExpectedRemainingQuantity,
			"status":            update.ExpectedStatus,
		},
		bson.M{"$set": bson.M{
			"filledQuantity":     update.FilledQuantity,
			"remainingQuantity":  update.RemainingQuantity,
			"averageFillPrice":   update.AverageFillPrice,
			"outstandingReserve": update.OutstandingReserve,
			"reserveReconciled":  update.ReserveReconciled,
			"status":             update.Status,
			"updatedAt":          time.Now().UTC(),
		}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	if err := res.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	var order Order
	if err := res.Decode(&order); err != nil {
		return nil, err
	}
	return &order, nil
}

func (r *MongoRepository) Cancel(ctx context.Context, id primitive.ObjectID, userID primitive.ObjectID) (*Order, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	res := r.col.FindOneAndUpdate(
		ctx,
		bson.M{"_id": id, "userId": userID, "status": bson.M{"$in": []string{StatusOpen, StatusPartiallyFilled}}},
		bson.M{"$set": bson.M{"status": StatusCancelled, "updatedAt": time.Now().UTC()}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	if err := res.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}

	var order Order
	if err := res.Decode(&order); err != nil {
		return nil, err
	}
	return &order, nil
}

func (r *MongoRepository) GetAll(ctx context.Context) []Order {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	cur, err := r.col.Find(ctx, bson.M{})
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)

	var out []Order
	if err := cur.All(ctx, &out); err != nil {
		return nil
	}
	return out
}

func (r *MongoRepository) List(ctx context.Context, f OrderFilter) []Order {
	orders, _ := r.ListWithError(ctx, f)
	return orders
}

func (r *MongoRepository) ListWithError(ctx context.Context, f OrderFilter) ([]Order, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	filter := bson.M{}
	if !f.UserID.IsZero() {
		filter["userId"] = f.UserID
	}
	if f.MatchID != "" {
		filter["matchId"] = f.MatchID
	}
	if f.MarketID != "" {
		filter["marketId"] = f.MarketID
	}
	if f.Side != "" {
		filter["side"] = f.Side
	}
	if f.Status != "" {
		filter["status"] = f.Status
	}

	cur, err := r.col.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var out []Order
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func timeoutCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 5*time.Second)
}

func getSampleOrders() []Order {
	now := time.Now().UTC()
	return []Order{
		{
			ID:        primitive.NewObjectID(),
			UserID:    sampleUserID,
			MatchID:   "1",
			MarketID:  "market-1",
			Side:      "buy",
			Quantity:  50,
			Price:     155,
			Status:    "executed",
			CreatedAt: now.Add(-1 * time.Hour),
			UpdatedAt: now.Add(-30 * time.Minute),
		},
		{
			ID:        primitive.NewObjectID(),
			UserID:    sampleUserID,
			MatchID:   "1",
			MarketID:  "market-2",
			Side:      "sell",
			Quantity:  30,
			Price:     160,
			Status:    "open",
			CreatedAt: now.Add(-15 * time.Minute),
			UpdatedAt: now.Add(-15 * time.Minute),
		},
	}
}
