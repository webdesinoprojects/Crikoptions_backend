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
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/orders"
)

type ProjectionFilter struct {
	UserID   primitive.ObjectID
	MatchID  string
	MarketID string
	Status   string
}

type ProjectionRepository interface {
	ApplyExecution(ctx context.Context, exec executions.Execution, constraint ProjectionConstraint) (ProjectionTransition, error)
	List(ctx context.Context, filter ProjectionFilter) ([]PositionProjection, error)
	GetByID(ctx context.Context, userID primitive.ObjectID, id string) (*PositionProjection, error)
	GetOpenByKey(ctx context.Context, userID primitive.ObjectID, matchID, marketID string, strike float64) (*PositionProjection, error)
	Clear(ctx context.Context) error
	EnsureIndexes(ctx context.Context) error
}

type ProjectionTransition struct {
	Before                 PositionProjection
	After                  PositionProjection
	ShortCollateralRelease float64
}

var ErrProjectionConstraint = errors.New("position no longer satisfies order constraint")

type ProjectionConstraint struct {
	MinLots *int
	MaxLots *int
}

func (c ProjectionConstraint) accepts(lots int) bool {
	return (c.MinLots == nil || lots >= *c.MinLots) &&
		(c.MaxLots == nil || lots <= *c.MaxLots)
}

type PositionProjection struct {
	ID              string             `bson:"_id"`
	UserID          primitive.ObjectID `bson:"userId"`
	MatchID         string             `bson:"matchId"`
	MarketID        string             `bson:"marketId"`
	Strike          float64            `bson:"strike"`
	Status          string             `bson:"status"`
	Side            string             `bson:"side,omitempty"`
	Lots            int                `bson:"lots"`
	BuyLots         int                `bson:"buyLots"`
	BuyNotional     float64            `bson:"buyNotional"`
	SellLots        int                `bson:"sellLots"`
	SellNotional    float64            `bson:"sellNotional"`
	BuyPrice        float64            `bson:"buyPrice"`
	SellPrice       float64            `bson:"sellPrice"`
	MatchedLots     int                `bson:"matchedLots"`
	RealizedPnL     float64            `bson:"realizedPnl"`
	ShortCollateral float64            `bson:"shortCollateral,omitempty"`
	Revision        int64              `bson:"revision"`
	CreatedAt       time.Time          `bson:"createdAt"`
	UpdatedAt       time.Time          `bson:"updatedAt"`
}

func (p PositionProjection) ToPosition(ltp float64) Position {
	sellPrice := p.SellPrice
	if p.Lots < 0 && p.ShortCollateral > 0 {
		sellPrice = p.ShortCollateral / ((1 + orders.ShortInitialMarginRate) * float64(-p.Lots))
	}
	position := Position{
		ID:              p.ID,
		UserID:          p.UserID,
		MatchID:         p.MatchID,
		MarketID:        p.MarketID,
		Strike:          p.Strike,
		Status:          p.Status,
		Side:            normalizedPositionSide(p.Side, p.Lots),
		Lots:            p.Lots,
		BuyPrice:        round2(p.BuyPrice),
		SellPrice:       round2(sellPrice),
		LTP:             round2(ltp),
		RealizedPnL:     round2(p.RealizedPnL),
		MatchedLots:     p.MatchedLots,
		ShortCollateral: round2(p.ShortCollateral),
		CreatedAt:       p.CreatedAt,
		UpdatedAt:       p.UpdatedAt,
	}
	position.PnL = computePnL(position, position.MatchedLots)
	if position.Status == "closed" && position.RealizedPnL != 0 {
		position.PnL = position.RealizedPnL
	}
	return position
}

func (p *PositionProjection) apply(exec executions.Execution) float64 {
	createdAt := exec.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	if p.ID == "" {
		side := strings.ToLower(strings.TrimSpace(exec.Side))
		p.ID = derivePositionID(exec.UserID, exec.MatchID, exec.MarketID, exec.Strike, createdAt)
		p.UserID = exec.UserID
		p.MatchID = exec.MatchID
		p.MarketID = exec.MarketID
		p.Strike = exec.Strike
		p.Side = sideToPositionSide(side)
		p.CreatedAt = createdAt
	}

	beforeLots := p.Lots
	shortCollateralRelease := 0.0
	switch strings.ToLower(strings.TrimSpace(exec.Side)) {
	case "buy":
		if beforeLots < 0 {
			shortLots := -beforeLots
			coverQty := minInt(exec.Quantity, shortLots)
			if coverQty > 0 && p.ShortCollateral > 0 {
				shortCollateralRelease = p.ShortCollateral
				if coverQty < shortLots {
					shortCollateralRelease = round2(p.ShortCollateral * float64(coverQty) / float64(shortLots))
				}
				p.ShortCollateral = round2(p.ShortCollateral - shortCollateralRelease)
			}
		}
		p.BuyLots += exec.Quantity
		p.BuyNotional += exec.Price * float64(exec.Quantity)
	case "sell":
		closeLongQty := 0
		if beforeLots > 0 {
			closeLongQty = minInt(exec.Quantity, beforeLots)
		}
		openShortQty := exec.Quantity - closeLongQty
		if openShortQty > 0 {
			initialMargin := round2(exec.Price * float64(openShortQty) * orders.ShortInitialMarginRate)
			proceeds := round2(exec.Price * float64(openShortQty))
			p.ShortCollateral = round2(p.ShortCollateral + initialMargin + proceeds)
		}
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
		if avgBuy > 0 && avgSell >= 0 {
			p.RealizedPnL = round2((avgSell - avgBuy) * float64(p.MatchedLots))
		} else {
			p.RealizedPnL = 0
		}
	}
	if p.UpdatedAt.IsZero() || createdAt.After(p.UpdatedAt) {
		p.UpdatedAt = createdAt
	}
	return shortCollateralRelease
}

type MemoryProjectionRepository struct {
	items map[string]PositionProjection
	mu    sync.RWMutex
}

func NewMemoryProjectionRepository() *MemoryProjectionRepository {
	return &MemoryProjectionRepository{items: make(map[string]PositionProjection)}
}

func (r *MemoryProjectionRepository) ApplyExecution(_ context.Context, exec executions.Execution, constraint ProjectionConstraint) (ProjectionTransition, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var projection PositionProjection
	found := false
	for _, item := range r.items {
		if projectionMatchesOpenKey(item, exec.UserID, exec.MatchID, exec.MarketID, exec.Strike) {
			projection = item
			found = true
			break
		}
	}
	before := projection
	if !constraint.accepts(before.Lots) {
		return ProjectionTransition{}, ErrProjectionConstraint
	}
	shortCollateralRelease := projection.apply(exec)
	if !found {
		if closed, ok := r.items[projection.ID]; ok && closed.Status == "closed" {
			projection.Revision = closed.Revision + 1
		} else {
			projection.Revision = 1
		}
	} else {
		projection.Revision = before.Revision + 1
	}
	r.items[projection.ID] = projection
	return ProjectionTransition{Before: before, After: projection, ShortCollateralRelease: shortCollateralRelease}, nil
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
			Keys: bson.D{{Key: "userId", Value: 1}, {Key: "matchId", Value: 1}, {Key: "marketId", Value: 1}, {Key: "strike", Value: 1}},
			Options: options.Index().
				SetName("unique_open_position_projection").
				SetUnique(true).
				SetPartialFilterExpression(bson.M{"status": "open"}),
		},
	})
	return err
}

func (r *MongoProjectionRepository) ApplyExecution(ctx context.Context, exec executions.Execution, constraint ProjectionConstraint) (ProjectionTransition, error) {
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
		return ProjectionTransition{}, err
	}
	found := err == nil
	before := projection
	if !constraint.accepts(before.Lots) {
		return ProjectionTransition{}, ErrProjectionConstraint
	}
	shortCollateralRelease := projection.apply(exec)
	projection.Revision = before.Revision + 1

	if !found {
		var closed PositionProjection
		err = r.col.FindOne(ctx, bson.M{"_id": projection.ID, "status": "closed"}).Decode(&closed)
		if err == nil {
			projection.Revision = closed.Revision + 1
			result, replaceErr := r.col.ReplaceOne(ctx, projectionRevisionFilter(closed), projection)
			if mongo.IsDuplicateKeyError(replaceErr) {
				return ProjectionTransition{}, newProjectionConflict(replaceErr)
			}
			if replaceErr != nil {
				return ProjectionTransition{}, replaceErr
			}
			if result.MatchedCount != 1 {
				return ProjectionTransition{}, newProjectionConflict(nil)
			}
			return ProjectionTransition{Before: before, After: projection, ShortCollateralRelease: shortCollateralRelease}, nil
		}
		if !errors.Is(err, mongo.ErrNoDocuments) {
			return ProjectionTransition{}, err
		}

		_, err = r.col.InsertOne(ctx, projection)
		if mongo.IsDuplicateKeyError(err) {
			return ProjectionTransition{}, newProjectionConflict(err)
		}
		if err != nil {
			return ProjectionTransition{}, err
		}
		return ProjectionTransition{Before: before, After: projection, ShortCollateralRelease: shortCollateralRelease}, nil
	}

	result, err := r.col.ReplaceOne(ctx, projectionRevisionFilter(before), projection)
	if err != nil {
		return ProjectionTransition{}, err
	}
	if result.MatchedCount != 1 {
		return ProjectionTransition{}, newProjectionConflict(nil)
	}
	return ProjectionTransition{Before: before, After: projection, ShortCollateralRelease: shortCollateralRelease}, nil
}

func projectionRevisionFilter(projection PositionProjection) bson.M {
	filter := bson.M{"_id": projection.ID, "status": projection.Status}
	if projection.Revision == 0 {
		filter["$or"] = bson.A{
			bson.M{"revision": int64(0)},
			bson.M{"revision": bson.M{"$exists": false}},
		}
	} else {
		filter["revision"] = projection.Revision
	}
	return filter
}

type projectionConflictError struct {
	cause error
}

func newProjectionConflict(cause error) error {
	return &projectionConflictError{cause: cause}
}

func (e *projectionConflictError) Error() string {
	return "position projection changed concurrently"
}

func (e *projectionConflictError) Unwrap() error {
	return e.cause
}

func (e *projectionConflictError) HasErrorLabel(label string) bool {
	return label == "TransientTransactionError"
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

func sideToPositionSide(side string) string {
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "sell":
		return "SELL"
	default:
		return "BUY"
	}
}

func normalizedPositionSide(side string, lots int) string {
	if lots < 0 {
		return "SELL"
	}
	if lots > 0 {
		return "BUY"
	}
	side = strings.ToUpper(strings.TrimSpace(side))
	if side == "BUY" || side == "SELL" {
		return side
	}
	return "BUY"
}
