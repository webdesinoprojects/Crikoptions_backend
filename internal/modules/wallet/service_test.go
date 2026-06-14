package wallet

import (
	"context"
	"errors"
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
