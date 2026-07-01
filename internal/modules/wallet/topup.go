package wallet

import (
	"context"
	"errors"
	"math"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

var (
	// ErrBonusAlreadyClaimed is returned when a user tries to claim the welcome
	// bonus a second time.
	ErrBonusAlreadyClaimed = errors.New("welcome bonus already claimed")
	// ErrTopUpAmountInvalid is returned for top-up amounts outside the valid range.
	ErrTopUpAmountInvalid = errors.New("top-up amount must be between 1 and 99999")
)

const welcomeBonusAmount = 100000.0 // ₹1,00,000

// ApplyWelcomeCredit credits ₹1,00,000 paper money to a newly registered user.
// It is idempotent: if the user already has a WELCOME_BONUS ledger entry the
// call returns nil without creating a duplicate credit.
func (s *Service) ApplyWelcomeCredit(ctx context.Context, userID primitive.ObjectID) error {
	// Check whether the bonus has already been credited (idempotency guard).
	entries, err := s.repo.ListLedger(ctx, LedgerFilter{UserID: userID, Limit: 100})
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Type == LedgerWelcomeBonus {
			return ErrBonusAlreadyClaimed
		}
	}

	_, err = s.repo.ApplyAdjustment(ctx, Adjustment{
		UserID:        userID,
		Delta:         welcomeBonusAmount,
		Amount:        welcomeBonusAmount,
		Type:          LedgerWelcomeBonus,
		ReferenceType: "SIGNUP_BONUS",
		ReferenceID:   primitive.NewObjectID().Hex(),
		Description:   "Welcome bonus — ₹1,00,000 paper money credited on signup",
		CreatedBy:     userID,
	})
	return err
}

// UserTopUp credits paper money to the user's own wallet.
// Amount must be between ₹1 and ₹99,999 per transaction.
func (s *Service) UserTopUp(ctx context.Context, userID primitive.ObjectID, amount float64) (*FundingResponse, error) {
	amount = math.Round(amount*100) / 100
	if amount <= 0 || amount > 99999 || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return nil, ErrTopUpAmountInvalid
	}

	result, err := s.repo.ApplyAdjustment(ctx, Adjustment{
		UserID:        userID,
		Delta:         amount,
		Amount:        amount,
		Type:          LedgerUserTopUp,
		ReferenceType: "USER_TOPUP",
		ReferenceID:   primitive.NewObjectID().Hex(),
		Description:   "Self-service paper wallet top-up",
		CreatedBy:     userID,
	})
	if err != nil {
		return nil, err
	}
	return &FundingResponse{Wallet: result.Account, LedgerEntry: result.LedgerEntry}, nil
}
