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

var (
	ErrAccountNotFound     = errors.New("wallet account not found")
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

func (r *MemoryRepository) ReserveMargin(_ context.Context, op OrderFundsOp) (*AdjustmentResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	amount := round2(op.Amount)
	if amount <= 0 {
		return nil, errInvalidAmount
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
	return &AdjustmentResult{Account: after, LedgerEntry: entry}, nil
}

func (r *MemoryRepository) ReleaseMargin(_ context.Context, op OrderFundsOp) (*AdjustmentResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	amount := round2(op.Amount)
	if amount <= 0 {
		return nil, errInvalidAmount
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
	return &AdjustmentResult{Account: after, LedgerEntry: entry}, nil
}

func (r *MemoryRepository) SettleBuyFill(_ context.Context, op BuyFillOp) (*AdjustmentResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fillCost := round2(op.FillCost)
	release := round2(op.ReserveRelease)
	if fillCost <= 0 || release < 0 {
		return nil, errInvalidAmount
	}

	now := time.Now().UTC()
	before := r.ensureAccountLocked(op.UserID, now)
	if before.CashBalance < fillCost || before.ReservedBalance < release {
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
	return &AdjustmentResult{Account: after, LedgerEntry: entry}, nil
}

func (r *MemoryRepository) SettleSellFill(_ context.Context, op SellFillOp) (*AdjustmentResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	proceeds := round2(op.Proceeds)
	if proceeds <= 0 {
		return nil, errInvalidAmount
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
	return &AdjustmentResult{Account: after, LedgerEntry: entry}, nil
}

func (r *MemoryRepository) SettleShortOpenFill(_ context.Context, op ShortOpenFillOp) (*AdjustmentResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	proceeds := round2(op.Proceeds)
	if proceeds <= 0 {
		return nil, errInvalidAmount
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
	return &AdjustmentResult{Account: after, LedgerEntry: entry}, nil
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
	}
	if _, err := r.ledger.InsertOne(ctx, entry); err != nil {
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
		userID:             op.UserID,
		amount:             op.FillCost,
		cashDelta:          -op.FillCost,
		reservedDelta:      -op.ReserveRelease,
		ledgerType:         LedgerTradeDebit,
		referenceType:      op.ReferenceType,
		referenceID:        op.ReferenceID,
		description:        op.Description,
		createdBy:          op.CreatedBy,
		requireCashGTE:     op.FillCost,
		requireReservedGTE: op.ReserveRelease,
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
}

func (r *MongoRepository) applyBalanceMutation(ctx context.Context, m balanceMutation) (*AdjustmentResult, error) {
	amount := round2(m.amount)
	if amount <= 0 {
		return nil, errInvalidAmount
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
