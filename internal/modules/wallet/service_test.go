package wallet

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestAdminCreditCreatesWalletAndLedger(t *testing.T) {
	svc := NewService(NewMemoryRepository())
	userID := primitive.NewObjectID()
	adminID := primitive.NewObjectID()

	result, err := svc.AdminCredit(context.Background(), adminID, userID, FundingRequest{
		Amount: 1000.257,
		Reason: "Initial paper money",
	})
	if err != nil {
		t.Fatalf("AdminCredit: %v", err)
	}

	if result.Wallet.UserID != userID {
		t.Fatalf("wallet user = %s, want %s", result.Wallet.UserID.Hex(), userID.Hex())
	}
	if result.Wallet.CashBalance != 1000.26 {
		t.Fatalf("cash = %.2f, want 1000.26", result.Wallet.CashBalance)
	}
	if result.Wallet.AvailableBalance != 1000.26 {
		t.Fatalf("available = %.2f, want 1000.26", result.Wallet.AvailableBalance)
	}
	if result.LedgerEntry.Type != LedgerAdminCredit {
		t.Fatalf("ledger type = %q, want %q", result.LedgerEntry.Type, LedgerAdminCredit)
	}
	if result.LedgerEntry.BalanceBefore != 0 || result.LedgerEntry.BalanceAfter != 1000.26 {
		t.Fatalf("ledger balance %.2f -> %.2f, want 0 -> 1000.26", result.LedgerEntry.BalanceBefore, result.LedgerEntry.BalanceAfter)
	}
	if result.LedgerEntry.CreatedBy != adminID {
		t.Fatalf("ledger createdBy = %s, want %s", result.LedgerEntry.CreatedBy.Hex(), adminID.Hex())
	}

	entries, err := svc.GetLedger(context.Background(), userID, 10)
	if err != nil {
		t.Fatalf("GetLedger: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(entries))
	}
}

func TestAdminDebitReducesAvailableBalance(t *testing.T) {
	svc := NewService(NewMemoryRepository())
	userID := primitive.NewObjectID()
	adminID := primitive.NewObjectID()

	if _, err := svc.AdminCredit(context.Background(), adminID, userID, FundingRequest{Amount: 500}); err != nil {
		t.Fatalf("AdminCredit: %v", err)
	}
	result, err := svc.AdminDebit(context.Background(), adminID, userID, FundingRequest{
		Amount: 125.5,
		Reason: "Manual correction",
	})
	if err != nil {
		t.Fatalf("AdminDebit: %v", err)
	}

	if result.Wallet.CashBalance != 374.5 {
		t.Fatalf("cash = %.2f, want 374.50", result.Wallet.CashBalance)
	}
	if result.LedgerEntry.Type != LedgerAdminDebit {
		t.Fatalf("ledger type = %q, want %q", result.LedgerEntry.Type, LedgerAdminDebit)
	}
	if result.LedgerEntry.Amount != 125.5 {
		t.Fatalf("ledger amount = %.2f, want 125.50", result.LedgerEntry.Amount)
	}
}

func TestAdminDebitRejectsInsufficientBalance(t *testing.T) {
	svc := NewService(NewMemoryRepository())
	userID := primitive.NewObjectID()
	adminID := primitive.NewObjectID()

	if _, err := svc.AdminCredit(context.Background(), adminID, userID, FundingRequest{Amount: 100}); err != nil {
		t.Fatalf("AdminCredit: %v", err)
	}
	if _, err := svc.AdminDebit(context.Background(), adminID, userID, FundingRequest{Amount: 150}); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("AdminDebit err = %v, want ErrInsufficientFunds", err)
	}

	wallet, err := svc.GetWallet(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetWallet: %v", err)
	}
	if wallet.AvailableBalance != 100 {
		t.Fatalf("available = %.2f, want 100", wallet.AvailableBalance)
	}
	entries, err := svc.GetLedger(context.Background(), userID, 10)
	if err != nil {
		t.Fatalf("GetLedger: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ledger entries = %d, want only the original credit", len(entries))
	}
}

func TestFundingRejectsInvalidAmount(t *testing.T) {
	svc := NewService(NewMemoryRepository())
	userID := primitive.NewObjectID()
	adminID := primitive.NewObjectID()

	for _, amount := range []float64{0, -1} {
		if _, err := svc.AdminCredit(context.Background(), adminID, userID, FundingRequest{Amount: amount}); !errors.Is(err, ErrInvalidAmount) {
			t.Fatalf("AdminCredit(%v) err = %v, want ErrInvalidAmount", amount, err)
		}
	}
}

func TestShortOpenFillCreditsCashAndReservesProceeds(t *testing.T) {
	svc := NewService(NewMemoryRepository())
	userID := primitive.NewObjectID()
	adminID := primitive.NewObjectID()

	if _, err := svc.AdminCredit(context.Background(), adminID, userID, FundingRequest{Amount: 1000}); err != nil {
		t.Fatalf("AdminCredit: %v", err)
	}
	if _, err := svc.ReserveOrderMargin(context.Background(), userID, 100, "order-1", "short initial margin"); err != nil {
		t.Fatalf("ReserveOrderMargin: %v", err)
	}
	if _, err := svc.SettleShortOpenFill(context.Background(), userID, 100, "order-1", "short sale proceeds"); err != nil {
		t.Fatalf("SettleShortOpenFill: %v", err)
	}

	wallet, err := svc.GetWallet(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetWallet: %v", err)
	}
	if wallet.CashBalance != 1100 || wallet.ReservedBalance != 200 || wallet.AvailableBalance != 900 {
		t.Fatalf("wallet after short = cash %.2f reserved %.2f available %.2f, want 1100/200/900", wallet.CashBalance, wallet.ReservedBalance, wallet.AvailableBalance)
	}

	if _, err := svc.SettleBuyFill(context.Background(), userID, 80, 0, "order-2", "short cover cost"); err != nil {
		t.Fatalf("SettleBuyFill: %v", err)
	}
	if _, err := svc.ReleaseOrderMargin(context.Background(), userID, 200, "order-2", "release short collateral"); err != nil {
		t.Fatalf("ReleaseOrderMargin: %v", err)
	}

	wallet, err = svc.GetWallet(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetWallet after cover: %v", err)
	}
	if wallet.CashBalance != 1020 || wallet.ReservedBalance != 0 || wallet.AvailableBalance != 1020 {
		t.Fatalf("wallet after cover = cash %.2f reserved %.2f available %.2f, want 1020/0/1020", wallet.CashBalance, wallet.ReservedBalance, wallet.AvailableBalance)
	}
}

func TestWalletOperationKeyReturnsPriorResultWithoutReapplyingBalance(t *testing.T) {
	repo := NewMemoryRepository()
	svc := NewService(repo)
	userID := primitive.NewObjectID()
	if _, err := svc.AdminCredit(context.Background(), primitive.NewObjectID(), userID, FundingRequest{Amount: 1000}); err != nil {
		t.Fatalf("AdminCredit: %v", err)
	}

	key := "wallet:user:" + userID.Hex() + ":contract:settlement:market:100:7:innings_score_v1:close:reserve"
	ctx := WithOperationKey(context.Background(), key)
	first, err := svc.ReserveOrderMargin(ctx, userID, 125, "settlement-order", "settlement reserve")
	if err != nil {
		t.Fatalf("first ReserveOrderMargin: %v", err)
	}
	second, err := svc.ReserveOrderMargin(ctx, userID, 125, "settlement-order", "settlement reserve")
	if err != nil {
		t.Fatalf("retry ReserveOrderMargin: %v", err)
	}
	if first.LedgerEntry.ID != second.LedgerEntry.ID {
		t.Fatalf("retry ledger id = %s, want %s", second.LedgerEntry.ID.Hex(), first.LedgerEntry.ID.Hex())
	}
	if second.LedgerEntry.IdempotencyKey != key || second.LedgerEntry.OperationHash == "" {
		t.Fatalf("idempotency metadata was not retained")
	}

	account, err := svc.GetWallet(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetWallet: %v", err)
	}
	if account.CashBalance != 1000 || account.ReservedBalance != 125 || account.AvailableBalance != 875 {
		t.Fatalf("wallet = %.2f/%.2f/%.2f, want 1000/125/875", account.CashBalance, account.ReservedBalance, account.AvailableBalance)
	}
	ledger, err := svc.GetLedger(context.Background(), userID, 10)
	if err != nil {
		t.Fatalf("GetLedger: %v", err)
	}
	if len(ledger) != 2 {
		t.Fatalf("ledger entries = %d, want funding plus one reserve", len(ledger))
	}
}

func TestWalletOperationKeyRejectsDifferentFinancialInputs(t *testing.T) {
	svc := NewService(NewMemoryRepository())
	userID := primitive.NewObjectID()
	key := "wallet:user:" + userID.Hex() + ":contract:void:market:100:0:innings_score_v1:close:sell-fill"
	ctx := WithOperationKey(context.Background(), key)
	if _, err := svc.SettleSellFill(ctx, userID, 50, "void-order", "void proceeds"); err != nil {
		t.Fatalf("first SettleSellFill: %v", err)
	}
	if _, err := svc.SettleSellFill(ctx, userID, 60, "void-order", "void proceeds"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("retry err = %v, want ErrIdempotencyConflict", err)
	}
	account, err := svc.GetWallet(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetWallet: %v", err)
	}
	if account.CashBalance != 50 {
		t.Fatalf("cash = %.2f, want one 50 credit", account.CashBalance)
	}
}

func TestConcurrentWalletOperationKeyAppliesOnce(t *testing.T) {
	svc := NewService(NewMemoryRepository())
	userID := primitive.NewObjectID()
	ctx := WithOperationKey(context.Background(), "wallet:user:"+userID.Hex()+":execution:abc:short-open-proceeds")

	const callers = 16
	results := make(chan *AdjustmentResult, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := svc.SettleShortOpenFill(ctx, userID, 40, "order-1", "short proceeds")
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	var ledgerID primitive.ObjectID
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent mutation: %v", err)
		}
	}
	for result := range results {
		if result == nil {
			t.Fatal("concurrent mutation returned nil result")
		}
		if ledgerID.IsZero() {
			ledgerID = result.LedgerEntry.ID
		} else if result.LedgerEntry.ID != ledgerID {
			t.Fatalf("ledger id = %s, want %s", result.LedgerEntry.ID.Hex(), ledgerID.Hex())
		}
	}
	account, err := svc.GetWallet(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetWallet: %v", err)
	}
	if account.CashBalance != 40 || account.ReservedBalance != 40 {
		t.Fatalf("wallet = %.2f/%.2f, want one 40 short-open mutation", account.CashBalance, account.ReservedBalance)
	}
}

func TestForcedProviderBuyCanSettleNegativeCashExactlyOnce(t *testing.T) {
	ctx := context.Background()
	svc := NewService(NewMemoryRepository())
	userID := primitive.NewObjectID()
	if _, err := svc.AdminCredit(ctx, primitive.NewObjectID(), userID, FundingRequest{Amount: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReserveOrderMargin(ctx, userID, 4, "short", "short collateral"); err != nil {
		t.Fatal(err)
	}

	opCtx := WithOperationKey(ctx, "provider-settlement:"+userID.Hex())
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := svc.SettleForcedBuyFill(opCtx, userID, 100, 4, "settlement", "forced provider cover"); err != nil {
			t.Fatalf("attempt %d: %v", attempt+1, err)
		}
	}
	account, err := svc.GetWallet(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if account.CashBalance != -90 || account.ReservedBalance != 0 || account.AvailableBalance != -90 {
		t.Fatalf("wallet = %+v, want -90/0/-90", account)
	}
	ledger, err := svc.GetLedger(ctx, userID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 3 {
		t.Fatalf("ledger entries = %d, want seed, reserve, and one forced debit", len(ledger))
	}
}

func TestRegularBuyCannotConsumeFundsReservedForAnotherOrder(t *testing.T) {
	ctx := context.Background()
	svc := NewService(NewMemoryRepository())
	userID := primitive.NewObjectID()
	if _, err := svc.AdminCredit(ctx, primitive.NewObjectID(), userID, FundingRequest{Amount: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReserveOrderMargin(ctx, userID, 900, "other", "other order"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SettleBuyFill(ctx, userID, 200, 0, "buy", "unreserved buy"); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("SettleBuyFill error = %v, want ErrInsufficientFunds", err)
	}
	account, err := svc.GetWallet(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if account.CashBalance != 1000 || account.ReservedBalance != 900 || account.AvailableBalance != 100 {
		t.Fatalf("wallet mutated on rejected buy: %+v", account)
	}
}
