package markets

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

type Repository interface {
	GetAll(ctx context.Context) []Market
	GetByMatchID(ctx context.Context, matchID string) []Market
	ListByMatchID(ctx context.Context, matchID string) ([]Market, error)
	GetProviderSettlementMarket(ctx context.Context, matchID string, innings int, finalRevision int64) (*Market, error)
	GetByID(ctx context.Context, id primitive.ObjectID) (*Market, error)
	GetByIDs(ctx context.Context, ids []primitive.ObjectID) (map[primitive.ObjectID]Market, error)
	Create(ctx context.Context, market Market) (*Market, error)
	UpdateStatus(ctx context.Context, id primitive.ObjectID, status string) (*Market, error)
	UpsertProviderInningsMarket(ctx context.Context, market Market) error
	UpdateProviderMarketGate(ctx context.Context, matchID string, innings int, lifecycle string, blockers []string, finalScore *int, finalRevision int64) error
	SetProviderManualBlocker(ctx context.Context, id primitive.ObjectID, blocked bool) (*Market, error)
	ClaimProviderSettlement(ctx context.Context, matchID string, innings int, finalRevision int64) (bool, error)
	VerifyProviderMarketGate(ctx context.Context, id primitive.ObjectID, stateVersion, tradingVersion int64) (*Market, bool, error)
	EnsureIndexes(ctx context.Context) error
}

type MemoryRepository struct {
	markets []Market
	mu      sync.RWMutex
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		markets: getSampleMarkets(),
	}
}

func (r *MemoryRepository) GetByMatchID(ctx context.Context, matchID string) []Market {
	markets, _ := r.ListByMatchID(ctx, matchID)
	return markets
}

func (r *MemoryRepository) ListByMatchID(_ context.Context, matchID string) ([]Market, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []Market
	for i := range r.markets {
		if r.markets[i].MatchID == matchID {
			result = append(result, r.markets[i])
		}
	}
	return result, nil
}

func (r *MemoryRepository) GetProviderSettlementMarket(_ context.Context, matchID string, innings int, finalRevision int64) (*Market, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for i := range r.markets {
		market := r.markets[i]
		if market.MatchID == matchID && market.Kind == MarketKindInningsScore &&
			market.Innings == innings && market.FormulaVersion == FormulaVersionInningsScoreV1 &&
			market.FinalRevision == finalRevision {
			return &market, nil
		}
	}
	return nil, nil
}

func (r *MemoryRepository) GetAll(ctx context.Context) []Market {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Market, len(r.markets))
	copy(out, r.markets)
	return out
}

func (r *MemoryRepository) GetByID(ctx context.Context, id primitive.ObjectID) (*Market, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for i := range r.markets {
		if r.markets[i].ID == id {
			return &r.markets[i], nil
		}
	}
	return nil, nil
}

func (r *MemoryRepository) GetByIDs(ctx context.Context, ids []primitive.ObjectID) (map[primitive.ObjectID]Market, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	wanted := make(map[primitive.ObjectID]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	out := make(map[primitive.ObjectID]Market, len(ids))
	for i := range r.markets {
		if _, ok := wanted[r.markets[i].ID]; ok {
			out[r.markets[i].ID] = r.markets[i]
		}
	}
	return out, nil
}

func (r *MemoryRepository) Create(ctx context.Context, market Market) (*Market, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if market.ID.IsZero() {
		market.ID = primitive.NewObjectID()
	}
	now := time.Now().UTC()
	if market.CreatedAt.IsZero() {
		market.CreatedAt = now
	}
	market.UpdatedAt = now

	r.markets = append(r.markets, market)
	return &market, nil
}

func (r *MemoryRepository) UpdateStatus(ctx context.Context, id primitive.ObjectID, status string) (*Market, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i := range r.markets {
		if r.markets[i].ID == id {
			r.markets[i].Status = status
			r.markets[i].UpdatedAt = time.Now().UTC()
			return &r.markets[i], nil
		}
	}
	return nil, nil
}

func (r *MemoryRepository) UpsertProviderInningsMarket(_ context.Context, market Market) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.markets {
		current := &r.markets[i]
		if current.MatchID == market.MatchID && current.Kind == market.Kind && current.Innings == market.Innings && current.FormulaVersion == market.FormulaVersion {
			current.MatchStateVersion = market.MatchStateVersion
			current.TradingVersion = market.TradingVersion
			current.UpdatedAt = time.Now().UTC()
			return nil
		}
	}
	if market.ID.IsZero() {
		market.ID = primitive.NewObjectID()
	}
	now := time.Now().UTC()
	market.CreatedAt = now
	market.UpdatedAt = now
	r.markets = append(r.markets, market)
	return nil
}

func (r *MemoryRepository) UpdateProviderMarketGate(_ context.Context, matchID string, innings int, lifecycle string, blockers []string, finalScore *int, finalRevision int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.markets {
		market := &r.markets[i]
		if market.MatchID != matchID || market.Kind != MarketKindInningsScore || market.Innings != innings || market.FormulaVersion != FormulaVersionInningsScoreV1 {
			continue
		}
		if market.Lifecycle == MarketLifecycleSettled || market.Lifecycle == MarketLifecycleVoid {
			if finalScore != nil && (market.FinalRevision != finalRevision || market.FinalScore != *finalScore) {
				return ErrFinalRevisionConflict
			}
			return nil
		}
		if market.SettlementRevision > 0 && finalScore != nil && market.SettlementRevision != finalRevision {
			return ErrFinalRevisionConflict
		}
		if market.Lifecycle == MarketLifecycleSettling && lifecycle == MarketLifecycleOpen {
			if market.SettlementRevision > 0 {
				return ErrFinalRevisionConflict
			}
		} else if !validLifecycleTransition(market.Lifecycle, lifecycle) {
			return errInvalidStatus
		}
		if finalScore != nil {
			switch {
			case finalRevision < market.FinalRevision:
				if lifecycle == MarketLifecycleSettled || lifecycle == MarketLifecycleVoid {
					return ErrFinalRevisionConflict
				}
				return nil
			case finalRevision == market.FinalRevision && market.FinalRevision > 0 && market.FinalScore != *finalScore:
				return ErrFinalRevisionConflict
			}
		}
		blockers = preserveProviderMarketBlockers(market.Blockers, blockers)
		market.Lifecycle = lifecycle
		market.Blockers = append([]string(nil), blockers...)
		market.Status = compatibilityStatus(lifecycle, blockers)
		if (lifecycle == MarketLifecycleOpen || lifecycle == MarketLifecycleVoid) && market.SettlementRevision == 0 && finalScore == nil {
			market.FinalScore = 0
			market.FinalRevision = 0
		}
		if finalScore != nil && finalRevision >= market.FinalRevision {
			market.FinalScore = *finalScore
			market.FinalRevision = finalRevision
		}
		market.UpdatedAt = time.Now().UTC()
		return nil
	}
	return errMarketNotFound
}

func (r *MemoryRepository) SetProviderManualBlocker(_ context.Context, id primitive.ObjectID, blocked bool) (*Market, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.markets {
		market := &r.markets[i]
		if market.ID != id || market.Kind != MarketKindInningsScore {
			continue
		}
		market.Blockers = setManualBlocker(market.Blockers, blocked)
		market.Status = compatibilityStatus(market.Lifecycle, market.Blockers)
		market.UpdatedAt = time.Now().UTC()
		copy := *market
		return &copy, nil
	}
	return nil, nil
}

func (r *MemoryRepository) ClaimProviderSettlement(_ context.Context, matchID string, innings int, finalRevision int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.markets {
		market := &r.markets[i]
		if market.MatchID != matchID || market.Kind != MarketKindInningsScore || market.Innings != innings ||
			market.FormulaVersion != FormulaVersionInningsScoreV1 || market.Lifecycle != MarketLifecycleSettling ||
			market.FinalRevision != finalRevision {
			continue
		}
		if market.SettlementRevision != 0 && market.SettlementRevision != finalRevision {
			return false, nil
		}
		now := time.Now().UTC()
		market.SettlementRevision = finalRevision
		market.SettlementStartedAt = &now
		return true, nil
	}
	return false, nil
}

func (r *MemoryRepository) VerifyProviderMarketGate(_ context.Context, id primitive.ObjectID, stateVersion, tradingVersion int64) (*Market, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.markets {
		market := &r.markets[i]
		if market.ID != id {
			continue
		}
		// Match-level gate owns version fencing; market gate only requires
		// open+active with no hard blocker. Soft sync markers must pass, or the
		// order 409s for the whole duration of a feed sync.
		valid := market.Kind == MarketKindInningsScore &&
			market.Lifecycle == MarketLifecycleOpen &&
			market.Status == MarketStatusActive &&
			!matches.HasHardTradingBlockers(market.Blockers)
		if !valid {
			return nil, false, nil
		}
		now := time.Now().UTC()
		market.TradingGateCheckedAt = &now
		market.MatchStateVersion = stateVersion
		market.TradingVersion = tradingVersion
		market.GateCheckSeq++
		out := *market
		return &out, true, nil
	}
	return nil, false, nil
}

func validLifecycleTransition(from, to string) bool {
	if from == "" || from == to {
		return true
	}
	switch from {
	case MarketLifecyclePending:
		return to == MarketLifecycleOpen || to == MarketLifecycleSettling || to == MarketLifecycleVoid
	case MarketLifecycleOpen:
		return to == MarketLifecycleSettling || to == MarketLifecycleVoid
	case MarketLifecycleSettling:
		return to == MarketLifecycleSettled || to == MarketLifecycleVoid
	case MarketLifecycleSettled, MarketLifecycleVoid:
		return false
	default:
		return false
	}
}

func (r *MemoryRepository) EnsureIndexes(ctx context.Context) error {
	return nil
}

type MongoRepository struct {
	col *mongo.Collection
}

func NewMongoRepository(db *mongo.Database) *MongoRepository {
	return &MongoRepository{col: db.Collection("markets")}
}

func (r *MongoRepository) EnsureIndexes(ctx context.Context) error {
	indexes := []mongo.IndexModel{
		{Keys: bson.D{{Key: "matchId", Value: 1}}},
		{
			Keys: bson.D{
				{Key: "matchId", Value: 1},
				{Key: "kind", Value: 1},
				{Key: "innings", Value: 1},
				{Key: "formulaVersion", Value: 1},
			},
			Options: options.Index().SetName("provider_market_contract_unique").SetUnique(true).SetPartialFilterExpression(bson.M{
				"kind": MarketKindInningsScore,
			}),
		},
	}
	_, err := r.col.Indexes().CreateMany(ctx, indexes)
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

	samples := getSampleMarkets()
	docs := make([]any, 0, len(samples))
	for _, market := range samples {
		docs = append(docs, market)
	}
	if len(docs) == 0 {
		return 0, nil
	}
	if _, err := r.col.InsertMany(ctx, docs); err != nil {
		return 0, err
	}
	return len(docs), nil
}

// EnsureDefaultMarkets upserts built-in sample markets so ODI (and other) markets
// appear even when the collection was seeded before they existed.
func (r *MongoRepository) EnsureDefaultMarkets(ctx context.Context) error {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	now := time.Now().UTC()
	for _, market := range getSampleMarkets() {
		filter := bson.M{"_id": market.ID}
		update := bson.M{
			"$setOnInsert": bson.M{
				"_id":            market.ID,
				"matchId":        market.MatchID,
				"title":          market.Title,
				"type":           market.Type,
				"status":         market.Status,
				"buyerPrice":     market.BuyerPrice,
				"sellerPrice":    market.SellerPrice,
				"ltp":            market.LTP,
				"open":           market.Open,
				"high":           market.High,
				"low":            market.Low,
				"quantityLadder": market.QuantityLadder,
				"createdAt":      market.CreatedAt,
			},
			"$set": bson.M{
				"updatedAt": now,
			},
		}
		if _, err := r.col.UpdateOne(ctx, filter, update, options.Update().SetUpsert(true)); err != nil {
			return err
		}
	}
	return nil
}

func (r *MongoRepository) GetAll(ctx context.Context) []Market {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	cur, err := r.col.Find(ctx, bson.M{})
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)

	var out []Market
	if err := cur.All(ctx, &out); err != nil {
		return nil
	}
	return out
}

func (r *MongoRepository) GetByMatchID(ctx context.Context, matchID string) []Market {
	markets, _ := r.ListByMatchID(ctx, matchID)
	return markets
}

func (r *MongoRepository) ListByMatchID(ctx context.Context, matchID string) ([]Market, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	cur, err := r.col.Find(ctx, bson.M{"matchId": matchID})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var out []Market
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *MongoRepository) GetProviderSettlementMarket(ctx context.Context, matchID string, innings int, finalRevision int64) (*Market, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	var market Market
	err := r.col.FindOne(ctx, bson.M{
		"matchId": matchID, "kind": MarketKindInningsScore, "innings": innings,
		"formulaVersion": FormulaVersionInningsScoreV1, "finalRevision": finalRevision,
	}).Decode(&market)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &market, nil
}

func (r *MongoRepository) GetByID(ctx context.Context, id primitive.ObjectID) (*Market, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	var market Market
	err := r.col.FindOne(ctx, bson.M{"_id": id}).Decode(&market)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return &market, nil
}

func (r *MongoRepository) GetByIDs(ctx context.Context, ids []primitive.ObjectID) (map[primitive.ObjectID]Market, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	out := make(map[primitive.ObjectID]Market, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	cur, err := r.col.Find(ctx, bson.M{"_id": bson.M{"$in": ids}})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var market Market
		if err := cur.Decode(&market); err != nil {
			return nil, err
		}
		out[market.ID] = market
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *MongoRepository) Create(ctx context.Context, market Market) (*Market, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	if market.ID.IsZero() {
		market.ID = primitive.NewObjectID()
	}
	now := time.Now().UTC()
	if market.CreatedAt.IsZero() {
		market.CreatedAt = now
	}
	market.UpdatedAt = now

	if _, err := r.col.InsertOne(ctx, market); err != nil {
		return nil, err
	}
	return &market, nil
}

func (r *MongoRepository) UpdateStatus(ctx context.Context, id primitive.ObjectID, status string) (*Market, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	res := r.col.FindOneAndUpdate(
		ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"status": status, "updatedAt": time.Now().UTC()}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	if err := res.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	var market Market
	if err := res.Decode(&market); err != nil {
		return nil, err
	}
	return &market, nil
}

func (r *MongoRepository) UpsertProviderInningsMarket(ctx context.Context, market Market) error {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()
	now := time.Now().UTC()
	filter := bson.M{
		"matchId":        market.MatchID,
		"kind":           MarketKindInningsScore,
		"innings":        market.Innings,
		"formulaVersion": FormulaVersionInningsScoreV1,
	}
	update := bson.M{
		"$setOnInsert": bson.M{
			"_id":            primitive.NewObjectID(),
			"matchId":        market.MatchID,
			"title":          market.Title,
			"type":           market.Type,
			"kind":           market.Kind,
			"innings":        market.Innings,
			"format":         market.Format,
			"scheduledBalls": market.ScheduledBalls,
			"strikeMin":      market.StrikeMin,
			"strikeMax":      market.StrikeMax,
			"strikeStep":     market.StrikeStep,
			"formulaVersion": market.FormulaVersion,
			"status":         market.Status,
			"lifecycle":      market.Lifecycle,
			"blockers":       market.Blockers,
			"quantityLadder": []LadderEntry{},
			"createdAt":      now,
		},
		"$set": bson.M{
			"matchStateVersion": market.MatchStateVersion,
			"tradingVersion":    market.TradingVersion,
			"updatedAt":         now,
		},
	}
	_, err := r.col.UpdateOne(ctx, filter, update, options.Update().SetUpsert(true))
	return err
}

func (r *MongoRepository) UpdateProviderMarketGate(ctx context.Context, matchID string, innings int, lifecycle string, blockers []string, finalScore *int, finalRevision int64) error {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()
	filter := bson.M{
		"matchId":        matchID,
		"kind":           MarketKindInningsScore,
		"innings":        innings,
		"formulaVersion": FormulaVersionInningsScoreV1,
	}
	if lifecycle == MarketLifecycleOpen {
		filter["$or"] = bson.A{
			bson.M{"lifecycle": bson.M{"$in": []string{"", MarketLifecyclePending, MarketLifecycleOpen}}},
			bson.M{"lifecycle": MarketLifecycleSettling, "$or": bson.A{
				bson.M{"settlementRevision": bson.M{"$exists": false}},
				bson.M{"settlementRevision": 0},
			}},
		}
	} else {
		filter["lifecycle"] = bson.M{"$in": allowedLifecycleSources(lifecycle)}
	}
	mergedBlockers := providerMarketBlockerExpression(blockers)
	set := bson.M{
		"lifecycle": lifecycle,
		"blockers":  mergedBlockers,
		"status":    providerMarketStatusExpression(lifecycle, mergedBlockers),
		"updatedAt": time.Now().UTC(),
	}
	if finalScore != nil {
		filter["$and"] = []bson.M{{"$or": []bson.M{
			{"settlementRevision": bson.M{"$exists": false}},
			{"settlementRevision": 0},
			{"settlementRevision": finalRevision},
		}}}
		filter["$or"] = []bson.M{
			{"finalRevision": bson.M{"$exists": false}},
			{"finalRevision": bson.M{"$lt": finalRevision}},
			{"finalRevision": finalRevision, "finalScore": *finalScore},
		}
		set["finalScore"] = *finalScore
		set["finalRevision"] = finalRevision
	}
	pipeline := mongo.Pipeline{bson.D{{Key: "$set", Value: set}}}
	if (lifecycle == MarketLifecycleOpen || lifecycle == MarketLifecycleVoid) && finalScore == nil {
		pipeline = append(pipeline, bson.D{{Key: "$unset", Value: bson.A{"finalScore", "finalRevision"}}})
	}
	res, err := r.col.UpdateOne(ctx, filter, pipeline)
	if err != nil {
		return err
	}
	if res.MatchedCount != 0 {
		return nil
	}

	var existing Market
	identity := bson.M{"matchId": matchID, "kind": MarketKindInningsScore, "innings": innings, "formulaVersion": FormulaVersionInningsScoreV1}
	if err := r.col.FindOne(ctx, identity).Decode(&existing); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return errMarketNotFound
		}
		return err
	}
	if existing.Lifecycle == MarketLifecycleSettled || existing.Lifecycle == MarketLifecycleVoid {
		if finalScore != nil && (existing.FinalRevision != finalRevision || existing.FinalScore != *finalScore) {
			return ErrFinalRevisionConflict
		}
		return nil
	}
	if existing.SettlementRevision > 0 && finalScore != nil && existing.SettlementRevision != finalRevision {
		return ErrFinalRevisionConflict
	}
	if existing.Lifecycle == MarketLifecycleSettling && lifecycle == MarketLifecycleOpen && existing.SettlementRevision > 0 {
		return ErrFinalRevisionConflict
	}
	if finalScore != nil && existing.FinalRevision > finalRevision {
		if lifecycle == MarketLifecycleSettled || lifecycle == MarketLifecycleVoid {
			return ErrFinalRevisionConflict
		}
		return nil
	}
	if finalScore != nil && existing.FinalRevision == finalRevision && existing.FinalScore != *finalScore {
		return ErrFinalRevisionConflict
	}
	if !validLifecycleTransition(existing.Lifecycle, lifecycle) {
		return errInvalidStatus
	}
	return nil
}

func (r *MongoRepository) SetProviderManualBlocker(ctx context.Context, id primitive.ObjectID, blocked bool) (*Market, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()
	current := bson.M{"$ifNull": bson.A{"$blockers", bson.A{}}}
	withoutManual := bson.M{"$filter": bson.M{
		"input": current, "as": "blocker", "cond": bson.M{"$ne": bson.A{"$$blocker", "manual"}},
	}}
	nextBlockers := any(withoutManual)
	if blocked {
		nextBlockers = bson.M{"$setUnion": bson.A{withoutManual, bson.A{"manual"}}}
	}
	status := bson.M{"$cond": bson.A{
		bson.M{"$in": bson.A{"$lifecycle", bson.A{MarketLifecycleSettled, MarketLifecycleVoid}}},
		MarketStatusClosed,
		bson.M{"$cond": bson.A{
			bson.M{"$and": bson.A{
				bson.M{"$eq": bson.A{"$lifecycle", MarketLifecycleOpen}},
				// Soft sync markers must not suspend here either, or an admin
				// toggle landing mid-sync would suspend a healthy contract.
				bson.M{"$eq": bson.A{bson.M{"$size": hardBlockerSubsetExpression(nextBlockers)}, 0}},
			}},
			MarketStatusActive,
			MarketStatusSuspended,
		}},
	}}
	result := r.col.FindOneAndUpdate(ctx, bson.M{
		"_id": id, "kind": MarketKindInningsScore,
	}, mongo.Pipeline{bson.D{{Key: "$set", Value: bson.M{
		"blockers": nextBlockers, "status": status, "updatedAt": time.Now().UTC(),
	}}}}, options.FindOneAndUpdate().SetReturnDocument(options.After))
	if err := result.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	var market Market
	if err := result.Decode(&market); err != nil {
		return nil, err
	}
	return &market, nil
}

func providerMarketBlockerExpression(desired []string) bson.M {
	values := bson.A{}
	for _, blocker := range desired {
		values = append(values, blocker)
	}
	return bson.M{"$setUnion": bson.A{
		values,
		bson.M{"$filter": bson.M{
			"input": bson.M{"$ifNull": bson.A{"$blockers", bson.A{}}},
			"as":    "blocker",
			"cond":  bson.M{"$eq": bson.A{"$$blocker", "manual"}},
		}},
	}}
}

// providerMarketStatusExpression is the Mongo-side twin of compatibilityStatus.
// Only hard blockers suspend: soft sync markers must leave the market ACTIVE so
// the terminal keeps Buy/Sell enabled while the feed reconciles.
func providerMarketStatusExpression(lifecycle string, blockers any) any {
	if lifecycle == MarketLifecycleSettled || lifecycle == MarketLifecycleVoid {
		return MarketStatusClosed
	}
	if lifecycle != MarketLifecycleOpen {
		return MarketStatusSuspended
	}
	return bson.M{"$cond": bson.A{
		bson.M{"$eq": bson.A{bson.M{"$size": hardBlockerSubsetExpression(blockers)}, 0}},
		MarketStatusActive,
		MarketStatusSuspended,
	}}
}

// hardBlockerSubsetExpression drops the soft sync markers from a blockers
// expression, leaving only blockers that must suspend trading.
func hardBlockerSubsetExpression(blockers any) bson.M {
	// $not takes a one-element array in the aggregation expression language.
	return bson.M{"$filter": bson.M{
		"input": blockers,
		"as":    "blocker",
		"cond":  bson.M{"$not": bson.A{bson.M{"$in": bson.A{"$$blocker", softBlockerValues()}}}},
	}}
}

// softBlockerValues is matches.SoftSyncTradingBlockers in a stable order so the
// generated pipeline stays deterministic across runs.
func softBlockerValues() bson.A {
	names := make([]string, 0, len(matches.SoftSyncTradingBlockers))
	for blocker := range matches.SoftSyncTradingBlockers {
		names = append(names, blocker)
	}
	sort.Strings(names)
	values := make(bson.A, 0, len(names))
	for _, name := range names {
		values = append(values, name)
	}
	return values
}

func preserveProviderMarketBlockers(existing, desired []string) []string {
	next := normalizeBlockers(desired)
	for _, blocker := range normalizeBlockers(existing) {
		if blocker == "manual" {
			next = appendUniqueBlocker(next, blocker)
		}
	}
	return next
}

func setManualBlocker(existing []string, blocked bool) []string {
	next := make([]string, 0, len(existing)+1)
	for _, blocker := range normalizeBlockers(existing) {
		if blocker != "manual" {
			next = append(next, blocker)
		}
	}
	if blocked {
		next = append(next, "manual")
	}
	return next
}

func appendUniqueBlocker(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}

func (r *MongoRepository) ClaimProviderSettlement(ctx context.Context, matchID string, innings int, finalRevision int64) (bool, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()
	now := time.Now().UTC()
	result, err := r.col.UpdateOne(ctx, bson.M{
		"matchId": matchID, "kind": MarketKindInningsScore, "innings": innings,
		"formulaVersion": FormulaVersionInningsScoreV1, "lifecycle": MarketLifecycleSettling,
		"finalRevision": finalRevision,
		"$or": []bson.M{
			{"settlementRevision": bson.M{"$exists": false}},
			{"settlementRevision": 0},
			{"settlementRevision": finalRevision},
		},
	}, bson.M{"$set": bson.M{
		"settlementRevision": finalRevision, "settlementStartedAt": now, "updatedAt": now,
	}})
	return result != nil && result.MatchedCount == 1, err
}

func (r *MongoRepository) VerifyProviderMarketGate(ctx context.Context, id primitive.ObjectID, stateVersion, tradingVersion int64) (*Market, bool, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()
	now := time.Now().UTC()
	// Match-level VerifyTradingGate already fences state/trading versions.
	// Requiring the same versions on the market doc races with every Sportmonks
	// score tick (Ensure updates market versions slightly out of band) and breaks sells.
	_ = stateVersion
	_ = tradingVersion
	result := r.col.FindOneAndUpdate(
		ctx,
		bson.M{
			"_id":       id,
			"kind":      MarketKindInningsScore,
			"lifecycle": MarketLifecycleOpen,
			"status":    MarketStatusActive,
			// Soft sync markers ride on blockers during normal feed churn.
			// Demanding a literally empty array made this the strictest gate in
			// the system: IsTradable would pass moments earlier and the order
			// still 409'd here for the whole duration of a sync. Reject only
			// when a blocker outside the soft set is present.
			"blockers": bson.M{"$not": bson.M{"$elemMatch": bson.M{"$nin": softBlockerValues()}}},
		},
		bson.M{
			// blockers are deliberately not written here — the feed owns them.
			// Clearing them would stomp the soft sync marker the badge reads.
			"$set": bson.M{
				"tradingGateCheckedAt": now,
				"matchStateVersion":    stateVersion,
				"tradingVersion":       tradingVersion,
			},
			"$inc": bson.M{"gateCheckSeq": 1},
		},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	if err := result.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var market Market
	if err := result.Decode(&market); err != nil {
		return nil, false, err
	}
	return &market, true, nil
}

func allowedLifecycleSources(target string) []string {
	switch target {
	case MarketLifecyclePending:
		return []string{"", MarketLifecyclePending}
	case MarketLifecycleOpen:
		return []string{"", MarketLifecyclePending, MarketLifecycleOpen}
	case MarketLifecycleSettling:
		return []string{"", MarketLifecyclePending, MarketLifecycleOpen, MarketLifecycleSettling}
	case MarketLifecycleSettled:
		return []string{MarketLifecycleSettling, MarketLifecycleSettled}
	case MarketLifecycleVoid:
		return []string{"", MarketLifecyclePending, MarketLifecycleOpen, MarketLifecycleSettling, MarketLifecycleVoid}
	default:
		return nil
	}
}

func timeoutCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 5*time.Second)
}

func getSampleMarkets() []Market {
	now := time.Now().UTC()
	mk := func(hex, matchID, title, mtype string, ltp, open, high, low, buyer, seller float64, ladder []LadderEntry) Market {
		id, _ := primitive.ObjectIDFromHex(hex)
		return Market{
			ID:             id,
			MatchID:        matchID,
			Title:          title,
			Type:           mtype,
			Status:         "active",
			BuyerPrice:     buyer,
			SellerPrice:    seller,
			LTP:            ltp,
			Open:           open,
			High:           high,
			Low:            low,
			QuantityLadder: ladder,
			CreatedAt:      now.Add(-1 * time.Hour),
			UpdatedAt:      now,
		}
	}

	closedID, _ := primitive.ObjectIDFromHex("0000000000000000000000d5")

	return []Market{
		mk("0000000000000000000000d1", "1", "CSK vs MI Match Depth", "match_depth", 156, 124, 160, 124, 155, 157, []LadderEntry{
			{BuyerQty: 570, BuyerPrice: 155, SellerPrice: 155.5, SellerQty: 400},
			{BuyerQty: 320, BuyerPrice: 156, SellerPrice: 156.5, SellerQty: 250},
			{BuyerQty: 150, BuyerPrice: 157, SellerPrice: 157.5, SellerQty: 180},
		}),
		mk("0000000000000000000000d2", "1", "CSK vs MI - 1st Innings Score", "future", 161, 150, 170, 140, 160, 162, []LadderEntry{
			{BuyerQty: 200, BuyerPrice: 160, SellerPrice: 161, SellerQty: 150},
			{BuyerQty: 100, BuyerPrice: 161, SellerPrice: 162, SellerQty: 80},
		}),
		mk("0000000000000000000000d3", "1", "CSK vs MI - Wicket Fall", "technical", 46, 40, 55, 35, 45, 47, []LadderEntry{
			{BuyerQty: 300, BuyerPrice: 45, SellerPrice: 46, SellerQty: 200},
			{BuyerQty: 150, BuyerPrice: 46, SellerPrice: 47, SellerQty: 100},
		}),
		mk("0000000000000000000000d4", "2", "RCB vs KKR Match Depth", "match_depth", 101, 98, 105, 95, 100, 102, []LadderEntry{
			{BuyerQty: 400, BuyerPrice: 100, SellerPrice: 101, SellerQty: 300},
			{BuyerQty: 200, BuyerPrice: 101, SellerPrice: 102, SellerQty: 150},
		}),
		mk("0000000000000000000000d6", "2", "RCB vs KKR - 1st Innings Score", "future", 168, 158, 178, 150, 167, 169, []LadderEntry{
			{BuyerQty: 220, BuyerPrice: 167, SellerPrice: 168, SellerQty: 160},
			{BuyerQty: 120, BuyerPrice: 168, SellerPrice: 169, SellerQty: 90},
		}),
		mk("0000000000000000000000d7", "2", "RCB vs KKR - Wicket Fall", "technical", 52, 44, 60, 40, 51, 53, []LadderEntry{
			{BuyerQty: 320, BuyerPrice: 51, SellerPrice: 52, SellerQty: 210},
			{BuyerQty: 160, BuyerPrice: 52, SellerPrice: 53, SellerQty: 110},
		}),
		mk("0000000000000000000000d8", "4", "IND vs AUS ODI Match Depth", "match_depth", 278, 265, 290, 250, 277, 279, []LadderEntry{
			{BuyerQty: 480, BuyerPrice: 277, SellerPrice: 278, SellerQty: 360},
			{BuyerQty: 240, BuyerPrice: 278, SellerPrice: 279, SellerQty: 180},
		}),
		mk("0000000000000000000000d9", "4", "IND vs AUS - 1st Innings Score", "future", 285, 270, 300, 255, 284, 286, []LadderEntry{
			{BuyerQty: 260, BuyerPrice: 284, SellerPrice: 285, SellerQty: 190},
			{BuyerQty: 140, BuyerPrice: 285, SellerPrice: 286, SellerQty: 100},
		}),
		mk("0000000000000000000000da", "4", "IND vs AUS - Wicket Fall", "technical", 48, 40, 58, 35, 47, 49, []LadderEntry{
			{BuyerQty: 300, BuyerPrice: 47, SellerPrice: 48, SellerQty: 210},
			{BuyerQty: 150, BuyerPrice: 48, SellerPrice: 49, SellerQty: 100},
		}),
		{
			ID:          closedID,
			MatchID:     "3",
			Title:       "DC vs SRH Match Depth",
			Type:        "match_depth",
			Status:      "closed",
			BuyerPrice:  180,
			SellerPrice: 182,
			LTP:         181,
			Open:        165,
			High:        190,
			Low:         160,
			QuantityLadder: []LadderEntry{
				{BuyerQty: 100, BuyerPrice: 180, SellerPrice: 181, SellerQty: 50},
			},
			CreatedAt: now.Add(-10 * time.Hour),
			UpdatedAt: now,
		},
	}
}
