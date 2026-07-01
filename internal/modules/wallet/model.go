package wallet

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	CurrencyPaperINR = "PAPER_INR"

	LedgerAdminCredit   = "ADMIN_CREDIT"
	LedgerAdminDebit    = "ADMIN_DEBIT"
	LedgerOrderReserve  = "ORDER_RESERVE"
	LedgerOrderRelease  = "ORDER_RELEASE"
	LedgerTradeDebit    = "TRADE_DEBIT"
	LedgerTradeCredit   = "TRADE_CREDIT"
	LedgerWelcomeBonus  = "WELCOME_BONUS"
	LedgerUserTopUp     = "USER_TOPUP"

	AccountActive = "ACTIVE"
)

type Account struct {
	ID               primitive.ObjectID `json:"_id" bson:"_id,omitempty"`
	UserID           primitive.ObjectID `json:"userId" bson:"userId"`
	Currency         string             `json:"currency" bson:"currency"`
	CashBalance      float64            `json:"cashBalance" bson:"cashBalance"`
	ReservedBalance  float64            `json:"reservedBalance" bson:"reservedBalance"`
	AvailableBalance float64            `json:"availableBalance" bson:"availableBalance"`
	Status           string             `json:"status" bson:"status"`
	CreatedAt        time.Time          `json:"createdAt" bson:"createdAt"`
	UpdatedAt        time.Time          `json:"updatedAt" bson:"updatedAt"`
}

type LedgerEntry struct {
	ID             primitive.ObjectID `json:"_id" bson:"_id,omitempty"`
	WalletID       primitive.ObjectID `json:"walletId" bson:"walletId"`
	UserID         primitive.ObjectID `json:"userId" bson:"userId"`
	Type           string             `json:"type" bson:"type"`
	Amount         float64            `json:"amount" bson:"amount"`
	BalanceBefore  float64            `json:"balanceBefore" bson:"balanceBefore"`
	BalanceAfter   float64            `json:"balanceAfter" bson:"balanceAfter"`
	ReservedBefore float64            `json:"reservedBefore" bson:"reservedBefore"`
	ReservedAfter  float64            `json:"reservedAfter" bson:"reservedAfter"`
	ReferenceType  string             `json:"referenceType" bson:"referenceType"`
	ReferenceID    string             `json:"referenceId" bson:"referenceId"`
	Description    string             `json:"description" bson:"description"`
	CreatedBy      primitive.ObjectID `json:"createdBy" bson:"createdBy"`
	CreatedAt      time.Time          `json:"createdAt" bson:"createdAt"`
}

type LedgerFilter struct {
	UserID primitive.ObjectID
	Limit  int64
}

type Adjustment struct {
	UserID        primitive.ObjectID
	Delta         float64
	Amount        float64
	Type          string
	ReferenceType string
	ReferenceID   string
	Description   string
	CreatedBy     primitive.ObjectID
}

type AdjustmentResult struct {
	Account     Account     `json:"wallet"`
	LedgerEntry LedgerEntry `json:"ledgerEntry"`
}

// OrderFundsOp moves margin between available and reserved balances.
type OrderFundsOp struct {
	UserID        primitive.ObjectID
	Amount        float64
	ReferenceType string
	ReferenceID   string
	Description   string
	CreatedBy     primitive.ObjectID
}

// BuyFillOp settles a buy fill against reserved margin.
type BuyFillOp struct {
	UserID         primitive.ObjectID
	FillCost       float64
	ReserveRelease float64
	ReferenceType  string
	ReferenceID    string
	Description    string
	CreatedBy      primitive.ObjectID
}

// SellFillOp credits proceeds from a sell fill.
type SellFillOp struct {
	UserID        primitive.ObjectID
	Proceeds      float64
	ReferenceType string
	ReferenceID   string
	Description   string
	CreatedBy     primitive.ObjectID
}
