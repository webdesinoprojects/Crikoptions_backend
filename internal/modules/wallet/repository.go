package wallet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

var (
	ErrAccountNotFound     = errors.New("wallet account not found")
	ErrIdempotencyConflict = errors.New("wallet idempotency key was reused with different inputs")
	errInsufficientReserve = errors.New("insufficient reserved wallet balance")
	errInvalidAmount       = errors.New("amount must be positive")
)

type Repository interface {
	EnsureAccount(ctx context.Context, userID primitive.ObjectID) (*Account, error)
	ApplyAdjustment(ctx context.Context, adjustment Adjustment) (*AdjustmentResult, error)
	ReserveMargin(ctx context.Context, op OrderFundsOp) (*AdjustmentResult, error)
	ReleaseMargin(ctx context.Context, op OrderFundsOp) (*AdjustmentResult, error)
	SettleBuyFill(ctx context.Context, op BuyFillOp) (*AdjustmentResult, error)
	SettleSellFill(ctx context.Context, op SellFillOp) (*AdjustmentResult, error)
	SettleShortOpenFill(ctx context.Context, op ShortOpenFillOp) (*AdjustmentResult, error)
	SettleForcedBuyFill(ctx context.Context, op BuyFillOp) (*AdjustmentResult, error)
	ReverseProviderContract(ctx context.Context, op ProviderVoidOp) (*AdjustmentResult, error)
	ListLedger(ctx context.Context, filter LedgerFilter) ([]LedgerEntry, error)
	EnsureIndexes(ctx context.Context) error
}

type MemoryRepository struct {
	accounts   map[primitive.ObjectID]Account
	ledger     []LedgerEntry
	operations map[string]memoryOperation
	mu         sync.Mutex
}

type memoryOperation struct {
	hash   string
	result AdjustmentResult
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		accounts:   make(map[primitive.ObjectID]Account),
		operations: make(map[string]memoryOperation),
	}
}

func (r *MemoryRepository) EnsureAccount(_ context.Context, userID primitive.ObjectID) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	account := r.ensureAccountLocked(userID, time.Now().UTC())
	return &account, nil
}

func (r *MemoryRepository) ApplyAdjustment(ctx context.Context, adjustment Adjustment) (*AdjustmentResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := OperationKey(ctx)
	hash := adjustmentOperationHash(adjustment)
	if prior, err := r.priorOperationLocked(adjustment.UserID, key, hash); prior != nil || err != nil {
		return prior, err
	}

	now := time.Now().UTC()
	before := r.ensureAccountLocked(adjustment.UserID, now)
	if adjustment.Delta < 0 && before.AvailableBalance < adjustment.Amount {
		return nil, ErrInsufficientFunds
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
	return r.recordOperationLocked(after, entry, key, hash), nil
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

func (r *MemoryRepository) ReserveMargin(ctx context.Context, op OrderFundsOp) (*AdjustmentResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	amount := round2(op.Amount)
	if amount <= 0 {
		return nil, errInvalidAmount
	}
	key := OperationKey(ctx)
	hash := fundsOperationHash(LedgerOrderReserve, op, amount)
	if prior, err := r.priorOperationLocked(op.UserID, key, hash); prior != nil || err != nil {
		return prior, err
	}

	now := time.Now().UTC()
	before := r.ensureAccountLocked(op.UserID, now)
	if before.AvailableBalance < amount {
		return nil, ErrInsufficientFunds
	}

	after := before
	after.ReservedBalance = round2(after.ReservedBalance + amount)
	after.AvailableBalance = round2(after.CashBalance - after.ReservedBalance)
	after.UpdatedAt = now
	r.accounts[op.UserID] = after

	entry := ledgerEntryFromMutation(before, after, op.UserID, LedgerOrderReserve, amount, op)
	return r.recordOperationLocked(after, entry, key, hash), nil
}

func (r *MemoryRepository) ReleaseMargin(ctx context.Context, op OrderFundsOp) (*AdjustmentResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	amount := round2(op.Amount)
	if amount <= 0 {
		return nil, errInvalidAmount
	}
	key := OperationKey(ctx)
	hash := fundsOperationHash(LedgerOrderRelease, op, amount)
	if prior, err := r.priorOperationLocked(op.UserID, key, hash); prior != nil || err != nil {
		return prior, err
	}

	now := time.Now().UTC()
	before := r.ensureAccountLocked(op.UserID, now)
	if before.ReservedBalance < amount {
		return nil, errInsufficientReserve
	}

	after := before
	after.ReservedBalance = round2(after.ReservedBalance - amount)
	after.AvailableBalance = round2(after.CashBalance - after.ReservedBalance)
	after.UpdatedAt = now
	r.accounts[op.UserID] = after

	entry := ledgerEntryFromMutation(before, after, op.UserID, LedgerOrderRelease, amount, op)
	return r.recordOperationLocked(after, entry, key, hash), nil
}

func (r *MemoryRepository) SettleBuyFill(ctx context.Context, op BuyFillOp) (*AdjustmentResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fillCost := round2(op.FillCost)
	release := round2(op.ReserveRelease)
	if fillCost <= 0 || release < 0 {
		return nil, errInvalidAmount
	}
	key := OperationKey(ctx)
	hash := buyFillOperationHash(op, fillCost, release)
	if prior, err := r.priorOperationLocked(op.UserID, key, hash); prior != nil || err != nil {
		return prior, err
	}

	now := time.Now().UTC()
	before := r.ensureAccountLocked(op.UserID, now)
	if before.AvailableBalance+release < fillCost || before.ReservedBalance < release {
		return nil, ErrInsufficientFunds
	}

	after := before
	after.CashBalance = round2(after.CashBalance - fillCost)
	after.ReservedBalance = round2(after.ReservedBalance - release)
	after.AvailableBalance = round2(after.CashBalance - after.ReservedBalance)
	after.UpdatedAt = now
	r.accounts[op.UserID] = after

	entry := LedgerEntry{
		ID:             primitive.NewObjectID(),
		WalletID:       after.ID,
		UserID:         op.UserID,
		Type:           LedgerTradeDebit,
		Amount:         fillCost,
		BalanceBefore:  before.CashBalance,
		BalanceAfter:   after.CashBalance,
		ReservedBefore: before.ReservedBalance,
		ReservedAfter:  after.ReservedBalance,
		ReferenceType:  op.ReferenceType,
		ReferenceID:    op.ReferenceID,
		Description:    op.Description,
		CreatedBy:      op.CreatedBy,
		CreatedAt:      now,
	}
	return r.recordOperationLocked(after, entry, key, hash), nil
}

func (r *MemoryRepository) SettleSellFill(ctx context.Context, op SellFillOp) (*AdjustmentResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	proceeds := round2(op.Proceeds)
	if proceeds <= 0 {
		return nil, errInvalidAmount
	}
	key := OperationKey(ctx)
	hash := sellFillOperationHash(op, proceeds)
	if prior, err := r.priorOperationLocked(op.UserID, key, hash); prior != nil || err != nil {
		return prior, err
	}

	now := time.Now().UTC()
	before := r.ensureAccountLocked(op.UserID, now)
	after := before
	after.CashBalance = round2(after.CashBalance + proceeds)
	after.AvailableBalance = round2(after.CashBalance - after.ReservedBalance)
	after.UpdatedAt = now
	r.accounts[op.UserID] = after

	entry := LedgerEntry{
		ID:             primitive.NewObjectID(),
		WalletID:       after.ID,
		UserID:         op.UserID,
		Type:           LedgerTradeCredit,
		Amount:         proceeds,
		BalanceBefore:  before.CashBalance,
		BalanceAfter:   after.CashBalance,
		ReservedBefore: before.ReservedBalance,
		ReservedAfter:  after.ReservedBalance,
		ReferenceType:  op.ReferenceType,
		ReferenceID:    op.ReferenceID,
		Description:    op.Description,
		CreatedBy:      op.CreatedBy,
		CreatedAt:      now,
	}
	return r.recordOperationLocked(after, entry, key, hash), nil
}

func (r *MemoryRepository) SettleShortOpenFill(ctx context.Context, op ShortOpenFillOp) (*AdjustmentResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	proceeds := round2(op.Proceeds)
	if proceeds <= 0 {
		return nil, errInvalidAmount
	}
	key := OperationKey(ctx)
	hash := shortOpenOperationHash(op, proceeds)
	if prior, err := r.priorOperationLocked(op.UserID, key, hash); prior != nil || err != nil {
		return prior, err
	}

	now := time.Now().UTC()
	before := r.ensureAccountLocked(op.UserID, now)
	after := before
	after.CashBalance = round2(after.CashBalance + proceeds)
	after.ReservedBalance = round2(after.ReservedBalance + proceeds)
	after.AvailableBalance = round2(after.CashBalance - after.ReservedBalance)
	after.UpdatedAt = now
	r.accounts[op.UserID] = after

	entry := LedgerEntry{
		ID:             primitive.NewObjectID(),
		WalletID:       after.ID,
		UserID:         op.UserID,
		Type:           LedgerTradeCredit,
		Amount:         proceeds,
		BalanceBefore:  before.CashBalance,
		BalanceAfter:   after.CashBalance,
		ReservedBefore: before.ReservedBalance,
		ReservedAfter:  after.ReservedBalance,
		ReferenceType:  op.ReferenceType,
		ReferenceID:    op.ReferenceID,
		Description:    op.Description,
		CreatedBy:      op.CreatedBy,
		CreatedAt:      now,
	}
	return r.recordOperationLocked(after, entry, key, hash), nil
}

func (r *MemoryRepository) SettleForcedBuyFill(ctx context.Context, op BuyFillOp) (*AdjustmentResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fillCost := round2(op.FillCost)
	release := round2(op.ReserveRelease)
	if fillCost <= 0 || release < 0 {
		return nil, errInvalidAmount
	}
	key := OperationKey(ctx)
	hash := forcedBuyFillOperationHash(op, fillCost, release)
	if prior, err := r.priorOperationLocked(op.UserID, key, hash); prior != nil || err != nil {
		return prior, err
	}

	now := time.Now().UTC()
	before := r.ensureAccountLocked(op.UserID, now)
	if before.ReservedBalance < release {
		return nil, errInsufficientReserve
	}
	after := before
	after.CashBalance = round2(after.CashBalance - fillCost)
	after.ReservedBalance = round2(after.ReservedBalance - release)
	after.AvailableBalance = round2(after.CashBalance - after.ReservedBalance)
	after.UpdatedAt = now
	r.accounts[op.UserID] = after

	entry := LedgerEntry{
		ID: primitive.NewObjectID(), WalletID: after.ID, UserID: op.UserID,
		Type: LedgerTradeDebit, Amount: fillCost,
		BalanceBefore: before.CashBalance, BalanceAfter: after.CashBalance,
		ReservedBefore: before.ReservedBalance, ReservedAfter: after.ReservedBalance,
		ReferenceType: op.ReferenceType, ReferenceID: op.ReferenceID,
		Description: op.Description, CreatedBy: op.CreatedBy, CreatedAt: now,
	}
	return r.recordOperationLocked(after, entry, key, hash), nil
}

func (r *MemoryRepository) ReverseProviderContract(ctx context.Context, op ProviderVoidOp) (*AdjustmentResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cashDelta := round2(op.CashDelta)
	reservedDelta := round2(op.ReservedDelta)
	amount := round2(math.Abs(cashDelta) + math.Abs(reservedDelta))
	if amount <= 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return nil, errInvalidAmount
	}
	key := OperationKey(ctx)
	hash := providerVoidOperationHash(op, cashDelta, reservedDelta)
	if prior, err := r.priorOperationLocked(op.UserID, key, hash); prior != nil || err != nil {
		return prior, err
	}

	now := time.Now().UTC()
	before := r.ensureAccountLocked(op.UserID, now)
	if reservedDelta < 0 && before.ReservedBalance < -reservedDelta {
		return nil, errInsufficientReserve
	}
	after := before
	after.CashBalance = round2(after.CashBalance + cashDelta)
	after.ReservedBalance = round2(after.ReservedBalance + reservedDelta)
	after.AvailableBalance = round2(after.CashBalance - after.ReservedBalance)
	after.UpdatedAt = now
	r.accounts[op.UserID] = after

	entry := LedgerEntry{
		ID: primitive.NewObjectID(), WalletID: after.ID, UserID: op.UserID,
		Type: LedgerProviderVoid, Amount: amount,
		BalanceBefore: before.CashBalance, BalanceAfter: after.CashBalance,
		ReservedBefore: before.ReservedBalance, ReservedAfter: after.ReservedBalance,
		ReferenceType: op.ReferenceType, ReferenceID: op.ReferenceID,
		Description: op.Description, CreatedBy: op.CreatedBy, CreatedAt: now,
	}
	return r.recordOperationLocked(after, entry, key, hash), nil
}

func ledgerEntryFromMutation(before, after Account, userID primitive.ObjectID, entryType string, amount float64, op OrderFundsOp) LedgerEntry {
	return LedgerEntry{
		ID:             primitive.NewObjectID(),
		WalletID:       after.ID,
		UserID:         userID,
		Type:           entryType,
		Amount:         amount,
		BalanceBefore:  before.CashBalance,
		BalanceAfter:   after.CashBalance,
		ReservedBefore: before.ReservedBalance,
		ReservedAfter:  after.ReservedBalance,
		ReferenceType:  op.ReferenceType,
		ReferenceID:    op.ReferenceID,
		Description:    op.Description,
		CreatedBy:      op.CreatedBy,
		CreatedAt:      after.UpdatedAt,
	}
}

func (r *MemoryRepository) priorOperationLocked(userID primitive.ObjectID, key, hash string) (*AdjustmentResult, error) {
	if key == "" {
		return nil, nil
	}
	prior, ok := r.operations[userID.Hex()+"|"+key]
	if !ok {
		return nil, nil
	}
	if prior.hash != hash {
		return nil, ErrIdempotencyConflict
	}
	result := prior.result
	return &result, nil
}

func (r *MemoryRepository) recordOperationLocked(account Account, entry LedgerEntry, key, hash string) *AdjustmentResult {
	entry.IdempotencyKey = key
	entry.OperationHash = hash
	r.ledger = append(r.ledger, entry)
	result := AdjustmentResult{Account: account, LedgerEntry: entry}
	if key != "" {
		r.operations[account.UserID.Hex()+"|"+key] = memoryOperation{hash: hash, result: result}
	}
	return &result
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
		{
			Keys: bson.D{{Key: "userId", Value: 1}, {Key: "idempotencyKey", Value: 1}},
			Options: options.Index().SetName("uniq_wallet_operation").SetUnique(true).SetPartialFilterExpression(bson.M{
				"idempotencyKey": bson.M{"$type": "string", "$gt": ""},
			}),
		},
	})
	return err
}

func (r *MongoRepository) EnsureAccount(ctx context.Context, userID primitive.ObjectID) (*Account, error) {
	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	now := time.Now().UTC()
	res := r.accounts.FindOneAndUpdate(
		ctx,
		bson.M{"userId": userID, "currency": CurrencyPaperINR},
		bson.M{"$setOnInsert": newAccount(userID, now)},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	)

	var account Account
	if err := res.Decode(&account); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrAccountNotFound
		}
		return nil, err
	}
	return &account, nil
}

func (r *MongoRepository) ApplyAdjustment(ctx context.Context, adjustment Adjustment) (*AdjustmentResult, error) {
	key := OperationKey(ctx)
	hash := adjustmentOperationHash(adjustment)
	return r.withIdempotentTransaction(ctx, key, func(runCtx context.Context) (*AdjustmentResult, error) {
		return r.applyAdjustment(runCtx, adjustment, key, hash)
	})
}

func (r *MongoRepository) applyAdjustment(ctx context.Context, adjustment Adjustment, key, hash string) (*AdjustmentResult, error) {
	if prior, ok, err := r.previousOperation(ctx, adjustment.UserID, key, hash); ok || err != nil {
		return prior, err
	}

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
				return nil, ErrInsufficientFunds
			}
			return nil, ErrAccountNotFound
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
		IdempotencyKey: key,
		OperationHash:  hash,
	}
	if _, err := r.ledger.InsertOne(ctx, entry); err != nil {
		if key != "" && mongo.IsDuplicateKeyError(err) {
			return nil, retryableIdempotencyError{cause: err}
		}
		return nil, err
	}

	return &AdjustmentResult{Account: updated, LedgerEntry: entry}, nil
}

func (r *MongoRepository) ReserveMargin(ctx context.Context, op OrderFundsOp) (*AdjustmentResult, error) {
	return r.applyBalanceMutation(ctx, balanceMutation{
		userID:              op.UserID,
		amount:              op.Amount,
		reservedDelta:       op.Amount,
		availableDelta:      -op.Amount,
		ledgerType:          LedgerOrderReserve,
		referenceType:       op.ReferenceType,
		referenceID:         op.ReferenceID,
		description:         op.Description,
		createdBy:           op.CreatedBy,
		requireAvailableGTE: op.Amount,
	})
}

func (r *MongoRepository) ReleaseMargin(ctx context.Context, op OrderFundsOp) (*AdjustmentResult, error) {
	return r.applyBalanceMutation(ctx, balanceMutation{
		userID:             op.UserID,
		amount:             op.Amount,
		reservedDelta:      -op.Amount,
		availableDelta:     op.Amount,
		ledgerType:         LedgerOrderRelease,
		referenceType:      op.ReferenceType,
		referenceID:        op.ReferenceID,
		description:        op.Description,
		createdBy:          op.CreatedBy,
		requireReservedGTE: op.Amount,
	})
}

func (r *MongoRepository) SettleBuyFill(ctx context.Context, op BuyFillOp) (*AdjustmentResult, error) {
	return r.applyBalanceMutation(ctx, balanceMutation{
		userID:              op.UserID,
		amount:              op.FillCost,
		cashDelta:           -op.FillCost,
		reservedDelta:       -op.ReserveRelease,
		ledgerType:          LedgerTradeDebit,
		referenceType:       op.ReferenceType,
		referenceID:         op.ReferenceID,
		description:         op.Description,
		createdBy:           op.CreatedBy,
		requireAvailableGTE: math.Max(op.FillCost-op.ReserveRelease, 0),
		requireReservedGTE:  op.ReserveRelease,
	})
}

func (r *MongoRepository) SettleSellFill(ctx context.Context, op SellFillOp) (*AdjustmentResult, error) {
	return r.applyBalanceMutation(ctx, balanceMutation{
		userID:         op.UserID,
		amount:         op.Proceeds,
		cashDelta:      op.Proceeds,
		availableDelta: op.Proceeds,
		ledgerType:     LedgerTradeCredit,
		referenceType:  op.ReferenceType,
		referenceID:    op.ReferenceID,
		description:    op.Description,
		createdBy:      op.CreatedBy,
	})
}

func (r *MongoRepository) SettleShortOpenFill(ctx context.Context, op ShortOpenFillOp) (*AdjustmentResult, error) {
	return r.applyBalanceMutation(ctx, balanceMutation{
		userID:        op.UserID,
		amount:        op.Proceeds,
		cashDelta:     op.Proceeds,
		reservedDelta: op.Proceeds,
		ledgerType:    LedgerTradeCredit,
		referenceType: op.ReferenceType,
		referenceID:   op.ReferenceID,
		description:   op.Description,
		createdBy:     op.CreatedBy,
	})
}

func (r *MongoRepository) SettleForcedBuyFill(ctx context.Context, op BuyFillOp) (*AdjustmentResult, error) {
	return r.applyBalanceMutation(ctx, balanceMutation{
		userID: op.UserID, amount: op.FillCost,
		cashDelta: -op.FillCost, reservedDelta: -op.ReserveRelease,
		ledgerType: LedgerTradeDebit, referenceType: op.ReferenceType,
		referenceID: op.ReferenceID, description: op.Description, createdBy: op.CreatedBy,
		requireReservedGTE: op.ReserveRelease,
		operationKind:      "forced-buy",
	})
}

func (r *MongoRepository) ReverseProviderContract(ctx context.Context, op ProviderVoidOp) (*AdjustmentResult, error) {
	cashDelta := round2(op.CashDelta)
	reservedDelta := round2(op.ReservedDelta)
	return r.applyBalanceMutation(ctx, balanceMutation{
		userID: op.UserID, amount: math.Abs(cashDelta) + math.Abs(reservedDelta),
		cashDelta: cashDelta, reservedDelta: reservedDelta,
		ledgerType: LedgerProviderVoid, referenceType: op.ReferenceType,
		referenceID: op.ReferenceID, description: op.Description, createdBy: op.CreatedBy,
		requireReservedGTE: math.Max(-reservedDelta, 0),
		operationKind:      "provider-void",
	})
}

type balanceMutation struct {
	userID              primitive.ObjectID
	amount              float64
	cashDelta           float64
	reservedDelta       float64
	availableDelta      float64
	ledgerType          string
	referenceType       string
	referenceID         string
	description         string
	createdBy           primitive.ObjectID
	requireAvailableGTE float64
	requireReservedGTE  float64
	requireCashGTE      float64
	operationKind       string
}

func (r *MongoRepository) applyBalanceMutation(ctx context.Context, m balanceMutation) (*AdjustmentResult, error) {
	amount := round2(m.amount)
	if amount <= 0 {
		return nil, errInvalidAmount
	}
	key := OperationKey(ctx)
	hash := balanceMutationOperationHash(m)
	return r.withIdempotentTransaction(ctx, key, func(runCtx context.Context) (*AdjustmentResult, error) {
		return r.applyBalanceMutationOnce(runCtx, m, amount, key, hash)
	})
}

func (r *MongoRepository) applyBalanceMutationOnce(ctx context.Context, m balanceMutation, amount float64, key, hash string) (*AdjustmentResult, error) {
	if prior, ok, err := r.previousOperation(ctx, m.userID, key, hash); ok || err != nil {
		return prior, err
	}

	account, err := r.EnsureAccount(ctx, m.userID)
	if err != nil {
		return nil, err
	}

	ctx, cancel := timeoutCtx(ctx)
	defer cancel()

	filter := bson.M{"_id": account.ID}
	if m.requireAvailableGTE > 0 {
		filter["availableBalance"] = bson.M{"$gte": round2(m.requireAvailableGTE)}
	}
	if m.requireReservedGTE > 0 {
		filter["reservedBalance"] = bson.M{"$gte": round2(m.requireReservedGTE)}
	}
	if m.requireCashGTE > 0 {
		filter["cashBalance"] = bson.M{"$gte": round2(m.requireCashGTE)}
	}

	inc := bson.M{}
	if m.cashDelta != 0 {
		inc["cashBalance"] = round2(m.cashDelta)
	}
	if m.reservedDelta != 0 {
		inc["reservedBalance"] = round2(m.reservedDelta)
	}
	if m.availableDelta != 0 {
		inc["availableBalance"] = round2(m.availableDelta)
	} else if m.cashDelta != 0 || m.reservedDelta != 0 {
		inc["availableBalance"] = round2(m.cashDelta - m.reservedDelta)
	}

	update := bson.M{
		"$inc": inc,
		"$set": bson.M{"updatedAt": time.Now().UTC()},
	}

	res := r.accounts.FindOneAndUpdate(
		ctx,
		filter,
		update,
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	if err := res.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrInsufficientFunds
		}
		return nil, err
	}

	var updated Account
	if err := res.Decode(&updated); err != nil {
		return nil, err
	}
	updated.CashBalance = round2(updated.CashBalance)
	updated.ReservedBalance = round2(updated.ReservedBalance)
	updated.AvailableBalance = round2(updated.CashBalance - updated.ReservedBalance)

	entry := LedgerEntry{
		ID:             primitive.NewObjectID(),
		WalletID:       updated.ID,
		UserID:         updated.UserID,
		Type:           m.ledgerType,
		Amount:         amount,
		BalanceBefore:  round2(account.CashBalance),
		BalanceAfter:   updated.CashBalance,
		ReservedBefore: round2(account.ReservedBalance),
		ReservedAfter:  updated.ReservedBalance,
		ReferenceType:  m.referenceType,
		ReferenceID:    m.referenceID,
		Description:    m.description,
		CreatedBy:      m.createdBy,
		CreatedAt:      time.Now().UTC(),
		IdempotencyKey: key,
		OperationHash:  hash,
	}
	if _, err := r.ledger.InsertOne(ctx, entry); err != nil {
		if key != "" && mongo.IsDuplicateKeyError(err) {
			return nil, retryableIdempotencyError{cause: err}
		}
		return nil, err
	}

	return &AdjustmentResult{Account: updated, LedgerEntry: entry}, nil
}

type retryableIdempotencyError struct {
	cause error
}

func (e retryableIdempotencyError) Error() string {
	return "wallet operation raced with an identical request"
}

func (e retryableIdempotencyError) Unwrap() error {
	return e.cause
}

func (e retryableIdempotencyError) HasErrorLabel(label string) bool {
	return label == "TransientTransactionError"
}

func (r *MongoRepository) withIdempotentTransaction(
	ctx context.Context,
	key string,
	fn func(context.Context) (*AdjustmentResult, error),
) (*AdjustmentResult, error) {
	if key == "" || mongo.SessionFromContext(ctx) != nil {
		return fn(ctx)
	}

	session, err := r.accounts.Database().Client().StartSession()
	if err != nil {
		return nil, err
	}
	defer session.EndSession(ctx)

	txCtx, cancel := timeoutCtx(ctx)
	defer cancel()
	var result *AdjustmentResult
	_, err = session.WithTransaction(txCtx, func(sessCtx mongo.SessionContext) (interface{}, error) {
		var txErr error
		result, txErr = fn(sessCtx)
		return nil, txErr
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()))
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (r *MongoRepository) previousOperation(
	ctx context.Context,
	userID primitive.ObjectID,
	key, hash string,
) (*AdjustmentResult, bool, error) {
	if key == "" {
		return nil, false, nil
	}

	lookupCtx, cancel := timeoutCtx(ctx)
	defer cancel()
	var entry LedgerEntry
	err := r.ledger.FindOne(lookupCtx, bson.M{
		"userId": userID, "idempotencyKey": key,
	}).Decode(&entry)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if entry.OperationHash == "" || entry.OperationHash != hash {
		return nil, false, ErrIdempotencyConflict
	}

	var account Account
	if err := r.accounts.FindOne(lookupCtx, bson.M{"_id": entry.WalletID}).Decode(&account); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, false, ErrAccountNotFound
		}
		return nil, false, err
	}
	account.CashBalance = round2(entry.BalanceAfter)
	account.ReservedBalance = round2(entry.ReservedAfter)
	account.AvailableBalance = round2(account.CashBalance - account.ReservedBalance)
	account.UpdatedAt = entry.CreatedAt
	return &AdjustmentResult{Account: account, LedgerEntry: entry}, true, nil
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

func adjustmentOperationHash(a Adjustment) string {
	return operationHash(
		"adjust", a.UserID.Hex(), round2(a.Delta), round2(a.Amount), a.Type,
		a.ReferenceType, a.ReferenceID, a.Description, a.CreatedBy.Hex(),
	)
}

func fundsOperationHash(kind string, op OrderFundsOp, amount float64) string {
	return operationHash(
		"funds", kind, op.UserID.Hex(), round2(amount), op.ReferenceType,
		op.ReferenceID, op.Description, op.CreatedBy.Hex(),
	)
}

func buyFillOperationHash(op BuyFillOp, fillCost, release float64) string {
	return operationHash(
		"buy", op.UserID.Hex(), round2(fillCost), round2(release), op.ReferenceType,
		op.ReferenceID, op.Description, op.CreatedBy.Hex(),
	)
}

func forcedBuyFillOperationHash(op BuyFillOp, fillCost, release float64) string {
	return operationHash(
		"forced-buy", op.UserID.Hex(), round2(fillCost), round2(release), op.ReferenceType,
		op.ReferenceID, op.Description, op.CreatedBy.Hex(),
	)
}

func providerVoidOperationHash(op ProviderVoidOp, cashDelta, reservedDelta float64) string {
	return operationHash(
		"provider-void", op.UserID.Hex(), round2(cashDelta), round2(reservedDelta),
		op.ReferenceType, op.ReferenceID, op.Description, op.CreatedBy.Hex(),
	)
}

func sellFillOperationHash(op SellFillOp, proceeds float64) string {
	return operationHash(
		"sell", op.UserID.Hex(), round2(proceeds), op.ReferenceType,
		op.ReferenceID, op.Description, op.CreatedBy.Hex(),
	)
}

func shortOpenOperationHash(op ShortOpenFillOp, proceeds float64) string {
	return operationHash(
		"short-open", op.UserID.Hex(), round2(proceeds), op.ReferenceType,
		op.ReferenceID, op.Description, op.CreatedBy.Hex(),
	)
}

func balanceMutationOperationHash(m balanceMutation) string {
	return operationHash(
		"mutation", m.operationKind, m.userID.Hex(), round2(m.amount), round2(m.cashDelta), round2(m.reservedDelta),
		round2(m.availableDelta), m.ledgerType, m.referenceType, m.referenceID,
		m.description, m.createdBy.Hex(), round2(m.requireAvailableGTE),
		round2(m.requireReservedGTE), round2(m.requireCashGTE),
	)
}

func operationHash(values ...any) string {
	payload, err := json.Marshal(values)
	if err != nil {
		payload = []byte(fmt.Sprintf("%#v", values))
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
