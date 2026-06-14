package wallet

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var errAccountNotFound = errors.New("wallet account not found")

type Repository interface {
	EnsureAccount(ctx context.Context, userID primitive.ObjectID) (*Account, error)
	ApplyAdjustment(ctx context.Context, adjustment Adjustment) (*AdjustmentResult, error)
	ListLedger(ctx context.Context, filter LedgerFilter) ([]LedgerEntry, error)
	EnsureIndexes(ctx context.Context) error
}

type MemoryRepository struct {
	accounts map[primitive.ObjectID]Account
	ledger   []LedgerEntry
	mu       sync.Mutex
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		accounts: make(map[primitive.ObjectID]Account),
	}
}

func (r *MemoryRepository) EnsureAccount(_ context.Context, userID primitive.ObjectID) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	account := r.ensureAccountLocked(userID, time.Now().UTC())
	return &account, nil
}

func (r *MemoryRepository) ApplyAdjustment(_ context.Context, adjustment Adjustment) (*AdjustmentResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	before := r.ensureAccountLocked(adjustment.UserID, now)
	if adjustment.Delta < 0 && before.AvailableBalance < adjustment.Amount {
		return nil, errInsufficientFunds
	}
	after := before
	after.CashBalance = round2(after.CashBalance + adjustment.Delta)
	after.AvailableBalance = round2(after.CashBalance - after.ReservedBalance)
	after.UpdatedAt = now
	r.accounts[adjustment.UserID] = after

	entry := LedgerEntry{
		ID:             primitive.NewObjectID(),
		WalletID:       after.ID,
		UserID:         adjustment.UserID,
		Type:           adjustment.Type,
		Amount:         round2(adjustment.Amount),
		BalanceBefore:  before.CashBalance,
		BalanceAfter:   after.CashBalance,
		ReservedBefore: before.ReservedBalance,
		ReservedAfter:  after.ReservedBalance,
		ReferenceType:  adjustment.ReferenceType,
		ReferenceID:    adjustment.ReferenceID,
		Description:    adjustment.Description,
		CreatedBy:      adjustment.CreatedBy,
		CreatedAt:      now,
	}
	r.ledger = append(r.ledger, entry)

	return &AdjustmentResult{Account: after, LedgerEntry: entry}, nil
}

func (r *MemoryRepository) ListLedger(_ context.Context, filter LedgerFilter) ([]LedgerEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	limit := normalizedLimit(filter.Limit)
	out := make([]LedgerEntry, 0, len(r.ledger))
	for i := len(r.ledger) - 1; i >= 0; i-- {
		entry := r.ledger[i]
		if !filter.UserID.IsZero() && entry.UserID != filter.UserID {
			continue
		}
		out = append(out, entry)
		if int64(len(out)) >= limit {
			break
		}
	}
	return out, nil
}

func (r *MemoryRepository) EnsureIndexes(_ context.Context) error {
	return nil
}

func (r *MemoryRepository) ensureAccountLocked(userID primitive.ObjectID, now time.Time) Account {
	if account, ok := r.accounts[userID]; ok {
		return account
	}
	account := Account{
		ID:               primitive.NewObjectID(),
		UserID:           userID,
		Currency:         CurrencyPaperINR,
		CashBalance:      0,
		ReservedBalance:  0,
		AvailableBalance: 0,
		Status:           AccountActive,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	r.accounts[userID] = account
	return account
}

type MongoRepository struct {
	accounts *mongo.Collection
	ledger   *mongo.Collection
}

func NewMongoRepository(db *mongo.Database) *MongoRepository {
	return &MongoRepository{
		accounts: db.Collection("wallet_accounts"),
		ledger:   db.Collection("wallet_ledger_entries"),
	}
}

func (r *MongoRepository) EnsureIndexes(ctx context.Context) error {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	if _, err := r.accounts.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "userId", Value: 1}, {Key: "currency", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		return err
	}

	_, err := r.ledger.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "userId", Value: 1}, {Key: "createdAt", Value: -1}}},
		{Keys: bson.D{{Key: "walletId", Value: 1}, {Key: "createdAt", Value: -1}}},
	})
	return err
}

func (r *MongoRepository) EnsureAccount(ctx context.Context, userID primitive.ObjectID) (*Account, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	now := time.Now().UTC()
	_, err := r.accounts.UpdateOne(
		ctx,
		bson.M{"userId": userID, "currency": CurrencyPaperINR},
		bson.M{"$setOnInsert": newAccount(userID, now)},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return nil, err
	}

	var account Account
	err = r.accounts.FindOne(ctx, bson.M{"userId": userID, "currency": CurrencyPaperINR}).Decode(&account)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, errAccountNotFound
		}
		return nil, err
	}
	return &account, nil
}

func (r *MongoRepository) ApplyAdjustment(ctx context.Context, adjustment Adjustment) (*AdjustmentResult, error) {
	account, err := r.EnsureAccount(ctx, adjustment.UserID)
	if err != nil {
		return nil, err
	}

	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	afterUpdate := bson.M{
		"$inc": bson.M{
			"cashBalance":      adjustment.Delta,
			"availableBalance": adjustment.Delta,
		},
		"$set": bson.M{"updatedAt": time.Now().UTC()},
	}
	filter := bson.M{"_id": account.ID}
	if adjustment.Delta < 0 {
		filter["availableBalance"] = bson.M{"$gte": adjustment.Amount}
	}

	res := r.accounts.FindOneAndUpdate(
		ctx,
		filter,
		afterUpdate,
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	if err := res.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			if adjustment.Delta < 0 {
				return nil, errInsufficientFunds
			}
			return nil, errAccountNotFound
		}
		return nil, err
	}

	var updated Account
	if err := res.Decode(&updated); err != nil {
		return nil, err
	}
	updated.CashBalance = round2(updated.CashBalance)
	updated.AvailableBalance = round2(updated.AvailableBalance)

	entry := LedgerEntry{
		ID:             primitive.NewObjectID(),
		WalletID:       updated.ID,
		UserID:         updated.UserID,
		Type:           adjustment.Type,
		Amount:         round2(adjustment.Amount),
		BalanceBefore:  round2(account.CashBalance),
		BalanceAfter:   updated.CashBalance,
		ReservedBefore: round2(account.ReservedBalance),
		ReservedAfter:  round2(updated.ReservedBalance),
		ReferenceType:  adjustment.ReferenceType,
		ReferenceID:    adjustment.ReferenceID,
		Description:    adjustment.Description,
		CreatedBy:      adjustment.CreatedBy,
		CreatedAt:      time.Now().UTC(),
	}
	if _, err := r.ledger.InsertOne(ctx, entry); err != nil {
		return nil, err
	}

	return &AdjustmentResult{Account: updated, LedgerEntry: entry}, nil
}

func (r *MongoRepository) ListLedger(ctx context.Context, filter LedgerFilter) ([]LedgerEntry, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	mongoFilter := bson.M{}
	if !filter.UserID.IsZero() {
		mongoFilter["userId"] = filter.UserID
	}

	cur, err := r.ledger.Find(
		ctx,
		mongoFilter,
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(normalizedLimit(filter.Limit)),
	)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var out []LedgerEntry
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func newAccount(userID primitive.ObjectID, now time.Time) Account {
	return Account{
		ID:               primitive.NewObjectID(),
		UserID:           userID,
		Currency:         CurrencyPaperINR,
		CashBalance:      0,
		ReservedBalance:  0,
		AvailableBalance: 0,
		Status:           AccountActive,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

func timeoutCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 5*time.Second)
}

func normalizedLimit(limit int64) int64 {
	if limit <= 0 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}
