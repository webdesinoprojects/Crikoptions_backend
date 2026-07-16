package orders

import (
	"context"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
)

func TestProviderSettlementWalletKeyUsesImmutableContract(t *testing.T) {
	userID := primitive.NewObjectID()
	executionID := primitive.NewObjectID()
	clientOrderID := "settlement:market-id:120:9:innings_score_v1:close"
	ctx := fillWalletOperationContext(
		context.Background(), userID, &Order{ClientOrderID: clientOrderID}, executionID, true, "sell-fill",
	)

	want := "wallet:user:" + userID.Hex() + ":contract:" + clientOrderID + ":sell-fill"
	if got := wallet.OperationKey(ctx); got != want {
		t.Fatalf("operation key = %q, want %q", got, want)
	}
}

func TestNormalFillWalletKeyUsesExecutionScope(t *testing.T) {
	userID := primitive.NewObjectID()
	executionID := primitive.NewObjectID()
	ctx := fillWalletOperationContext(
		context.Background(), userID, &Order{ClientOrderID: "settlement:user-supplied"}, executionID, false, "buy-fill",
	)

	key := wallet.OperationKey(ctx)
	if !strings.Contains(key, ":execution:"+executionID.Hex()+":buy-fill") {
		t.Fatalf("normal fill operation key = %q, want execution scope", key)
	}
}
