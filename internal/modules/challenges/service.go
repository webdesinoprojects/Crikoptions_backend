package challenges

import (
	"context"
	"errors"
	"fmt"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/positions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

var (
	// ErrUnknownChallenge is returned for an ID not in the server-owned table.
	ErrUnknownChallenge = errors.New("unknown challenge")
	// ErrNotComplete is returned when the user has not actually finished the
	// challenge. This is the check that replaces the client's self-report.
	ErrNotComplete = errors.New("challenge is not complete")
	// ErrAlreadyClaimed is returned when the reward was already paid out.
	ErrAlreadyClaimed = errors.New("challenge reward already claimed")
)

type PositionService interface {
	ListUserPositions(ctx context.Context, userID primitive.ObjectID, filter positions.PositionFilter) ([]positions.Position, error)
}

// WalletService is the subset of the wallet used to pay rewards and to read
// back which rewards have already been paid.
type WalletService interface {
	CreditChallengeReward(ctx context.Context, userID primitive.ObjectID, challengeID, description string, amount float64) (*wallet.AdjustmentResult, error)
	GetLedger(ctx context.Context, userID primitive.ObjectID, limit int64) ([]wallet.LedgerEntry, error)
}

type Service struct {
	positions PositionService
	wallet    WalletService
}

func NewService(positions PositionService, wallet WalletService) *Service {
	return &Service{positions: positions, wallet: wallet}
}

// Evaluate returns every challenge with server-derived progress and whether its
// reward has already been paid.
func (s *Service) Evaluate(ctx context.Context, userID primitive.ObjectID) ([]Challenge, error) {
	// An empty Status returns both open and closed rows, which is what progress
	// needs — a closed trade is the evidence for most challenges.
	pos, err := s.positions.ListUserPositions(ctx, userID, positions.PositionFilter{})
	if err != nil {
		return nil, err
	}
	claimed, err := s.claimedIDs(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := EvaluatePositions(pos)
	for i := range out {
		out[i].Claimed = claimed[out[i].ID]
	}
	return out, nil
}

// claimedIDs reads paid rewards straight from the wallet ledger, so claims need
// no separate store to fall out of sync with the money that was actually moved.
func (s *Service) claimedIDs(ctx context.Context, userID primitive.ObjectID) (map[string]bool, error) {
	entries, err := s.wallet.GetLedger(ctx, userID, ledgerScanLimit)
	if err != nil {
		return nil, err
	}
	claimed := make(map[string]bool)
	for _, e := range entries {
		if e.Type == wallet.LedgerChallengeReward {
			claimed[e.ReferenceID] = true
		}
	}
	return claimed, nil
}

// ledgerScanLimit bounds the ledger read. It comfortably exceeds the number of
// challenges, and a reward that scrolled past it is still protected from a
// double payout by the wallet's unique operation key.
const ledgerScanLimit = 200

// Claim pays a challenge reward. It re-derives completion from position data on
// every call, so a client that asks for an unearned reward is refused.
func (s *Service) Claim(ctx context.Context, userID primitive.ObjectID, challengeID string) (*Challenge, error) {
	def, ok := definitionByID[challengeID]
	if !ok {
		return nil, ErrUnknownChallenge
	}

	evaluated, err := s.Evaluate(ctx, userID)
	if err != nil {
		return nil, err
	}
	var current Challenge
	for _, c := range evaluated {
		if c.ID == challengeID {
			current = c
			break
		}
	}
	if current.Claimed {
		return nil, ErrAlreadyClaimed
	}
	if !current.Claimable() {
		return nil, ErrNotComplete
	}

	// Reward comes from the server-owned table, never from the request.
	if _, err := s.wallet.CreditChallengeReward(
		ctx, userID, challengeID,
		fmt.Sprintf("Challenge reward — %s", def.Title), def.Reward,
	); err != nil {
		return nil, err
	}

	current.Claimed = true
	return &current, nil
}
