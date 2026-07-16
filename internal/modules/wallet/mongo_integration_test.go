package wallet

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

func TestMongoWalletOperationIdempotency(t *testing.T) {
	uri := strings.TrimSpace(os.Getenv("MONGO_INTEGRATION_URI"))
	if uri == "" {
		t.Skip("MONGO_INTEGRATION_URI is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect to Mongo replica set: %v", err)
	}
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		t.Fatalf("ping Mongo primary: %v", err)
	}
	db := client.Database("crikoptions_wallet_it_" + primitive.NewObjectID().Hex())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if err := db.Drop(cleanupCtx); err != nil {
			t.Errorf("drop integration database: %v", err)
		}
		if err := client.Disconnect(cleanupCtx); err != nil {
			t.Errorf("disconnect integration client: %v", err)
		}
	})

	repo := NewMongoRepository(db)
	if err := repo.EnsureIndexes(ctx); err != nil {
		t.Fatalf("ensure wallet indexes: %v", err)
	}
	svc := NewService(repo)
	userID := primitive.NewObjectID()
	if _, err := svc.AdminCredit(ctx, primitive.NewObjectID(), userID, FundingRequest{Amount: 1000}); err != nil {
		t.Fatalf("seed wallet: %v", err)
	}

	key := "wallet:user:" + userID.Hex() + ":contract:settlement:market:120:11:innings_score_v1:close:sell-fill"
	opCtx := WithOperationKey(ctx, key)
	const callers = 4
	results := make(chan *AdjustmentResult, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, callErr := svc.SettleSellFill(opCtx, userID, 75, "settlement-order", "provider settlement proceeds")
			results <- result
			errs <- callErr
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for callErr := range errs {
		if callErr != nil {
			t.Fatalf("concurrent settlement retry: %v", callErr)
		}
	}

	var first *AdjustmentResult
	for result := range results {
		if result == nil {
			t.Fatal("concurrent settlement returned nil result")
		}
		if first == nil {
			first = result
		} else if result.LedgerEntry.ID != first.LedgerEntry.ID {
			t.Fatalf("ledger id = %s, want %s", result.LedgerEntry.ID.Hex(), first.LedgerEntry.ID.Hex())
		}
	}
	if first == nil {
		t.Fatal("no settlement result")
	}
	if _, err := svc.SettleSellFill(opCtx, userID, 76, "settlement-order", "provider settlement proceeds"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed retry err = %v, want ErrIdempotencyConflict", err)
	}

	account, err := svc.GetWallet(ctx, userID)
	if err != nil {
		t.Fatalf("read wallet: %v", err)
	}
	if account.CashBalance != 1075 || account.ReservedBalance != 0 || account.AvailableBalance != 1075 {
		t.Fatalf("wallet = %.2f/%.2f/%.2f, want one 75 credit", account.CashBalance, account.ReservedBalance, account.AvailableBalance)
	}
	count, err := repo.ledger.CountDocuments(ctx, bson.M{"userId": userID, "idempotencyKey": key})
	if err != nil || count != 1 {
		t.Fatalf("idempotent ledger count = %d, err=%v", count, err)
	}

	duplicate := first.LedgerEntry
	duplicate.ID = primitive.NewObjectID()
	if _, err := repo.ledger.InsertOne(ctx, duplicate); !mongo.IsDuplicateKeyError(err) {
		t.Fatalf("duplicate ledger insert err = %v, want duplicate-key error", err)
	}
}
