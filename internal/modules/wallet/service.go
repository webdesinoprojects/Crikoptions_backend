package wallet

import (
	"context"
	"errors"
	"math"
	"strings"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

var (
	errInvalidAmount     = errors.New("amount must be positive")
	errInvalidUserID     = errors.New("invalid userId")
	errInsufficientFunds = errors.New("insufficient available wallet balance")
)

type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) GetWallet(ctx context.Context, userID primitive.ObjectID) (*Account, error) {
	return s.repo.EnsureAccount(ctx, userID)
}

func (s *Service) GetLedger(ctx context.Context, userID primitive.ObjectID, limit int64) ([]LedgerEntry, error) {
	return s.repo.ListLedger(ctx, LedgerFilter{UserID: userID, Limit: limit})
}

func (s *Service) AdminGetWallet(ctx context.Context, userID primitive.ObjectID) (*Account, error) {
	return s.repo.EnsureAccount(ctx, userID)
}

func (s *Service) AdminListLedger(ctx context.Context, userID primitive.ObjectID, limit int64) ([]LedgerEntry, error) {
	return s.repo.ListLedger(ctx, LedgerFilter{UserID: userID, Limit: limit})
}

func (s *Service) AdminCredit(ctx context.Context, adminID, userID primitive.ObjectID, req FundingRequest) (*FundingResponse, error) {
	amount, reason, err := validateFundingRequest(req)
	if err != nil {
		return nil, err
	}

	result, err := s.repo.ApplyAdjustment(ctx, Adjustment{
		UserID:        userID,
		Delta:         amount,
		Amount:        amount,
		Type:          LedgerAdminCredit,
		ReferenceType: "ADMIN_ACTION",
		ReferenceID:   primitive.NewObjectID().Hex(),
		Description:   reason,
		CreatedBy:     adminID,
	})
	if err != nil {
		return nil, err
	}
	return &FundingResponse{Wallet: result.Account, LedgerEntry: result.LedgerEntry}, nil
}

func (s *Service) AdminDebit(ctx context.Context, adminID, userID primitive.ObjectID, req FundingRequest) (*FundingResponse, error) {
	amount, reason, err := validateFundingRequest(req)
	if err != nil {
		return nil, err
	}

	account, err := s.repo.EnsureAccount(ctx, userID)
	if err != nil {
		return nil, err
	}
	if account.AvailableBalance < amount {
		return nil, errInsufficientFunds
	}

	result, err := s.repo.ApplyAdjustment(ctx, Adjustment{
		UserID:        userID,
		Delta:         -amount,
		Amount:        amount,
		Type:          LedgerAdminDebit,
		ReferenceType: "ADMIN_ACTION",
		ReferenceID:   primitive.NewObjectID().Hex(),
		Description:   reason,
		CreatedBy:     adminID,
	})
	if err != nil {
		return nil, err
	}
	return &FundingResponse{Wallet: result.Account, LedgerEntry: result.LedgerEntry}, nil
}

func ParseUserID(hex string) (primitive.ObjectID, error) {
	id, err := primitive.ObjectIDFromHex(strings.TrimSpace(hex))
	if err != nil || id.IsZero() {
		return primitive.ObjectID{}, errInvalidUserID
	}
	return id, nil
}

func validateFundingRequest(req FundingRequest) (float64, string, error) {
	amount := round2(req.Amount)
	if amount <= 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return 0, "", errInvalidAmount
	}

	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "Paper wallet admin adjustment"
	}
	if len(reason) > 280 {
		reason = reason[:280]
	}
	return amount, reason, nil
}

func round2(value float64) float64 {
	return math.Round(value*100) / 100
}
