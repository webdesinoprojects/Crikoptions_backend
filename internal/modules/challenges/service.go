package challenges

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	CreditDailyChallengeReward(ctx context.Context, userID primitive.ObjectID, challengeID, dateUTC, description string, amount float64) (*wallet.AdjustmentResult, error)
	GetLedger(ctx context.Context, userID primitive.ObjectID, limit int64) ([]wallet.LedgerEntry, error)
}

type Service struct {
	positions PositionService
	wallet    WalletService
	daily     DailyStore
	now       func() time.Time
}

func NewService(positions PositionService, wallet WalletService) *Service {
	return NewServiceWithStore(positions, wallet, NewMemoryDailyStore())
}

func NewServiceWithStore(positions PositionService, wallet WalletService, daily DailyStore) *Service {
	if daily == nil {
		daily = NewMemoryDailyStore()
	}
	return &Service{positions: positions, wallet: wallet, daily: daily}
}

func (s *Service) EnsureIndexes(ctx context.Context) error {
	if s == nil || s.daily == nil {
		return nil
	}
	return s.daily.EnsureIndexes(ctx)
}

func (s *Service) nowUTC() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func utcDate(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// Evaluate returns every challenge with server-derived progress and whether its
// reward has already been paid. Academy challenges come first from positions;
// the four daily IDs are always appended so the client can find them by id.
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
	academy := EvaluatePositions(pos)
	for i := range academy {
		academy[i].Claimed = claimed[academy[i].ID]
	}
	daily, err := s.evaluateDaily(ctx, userID)
	if err != nil {
		return nil, err
	}
	return append(daily, academy...), nil
}

func (s *Service) evaluateDaily(ctx context.Context, userID primitive.ObjectID) ([]Challenge, error) {
	out := emptyDailyChallenges()
	if s.daily == nil {
		return out, nil
	}
	dateUTC := utcDate(s.nowUTC())
	rows, err := s.daily.ListForDate(ctx, userID, dateUTC)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]DailyProgress, len(rows))
	for _, row := range rows {
		byID[row.ChallengeID] = row
	}
	for i := range out {
		if row, ok := byID[out[i].ID]; ok {
			out[i].Progress = row.Progress
			out[i].Claimed = row.Claimed
			out[i] = finalizeDaily(out[i])
		}
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
	today := utcDate(s.nowUTC())
	for _, e := range entries {
		if e.Type != wallet.LedgerChallengeReward {
			continue
		}
		claimed[e.ReferenceID] = true
		if id, date, ok := splitDailyClaimRef(e.ReferenceID); ok && date == today {
			claimed[id] = true
		}
	}
	return claimed, nil
}

func splitDailyClaimRef(ref string) (id, date string, ok bool) {
	if len(ref) < 12 {
		return "", "", false
	}
	// daily refs are "{id}:{YYYY-MM-DD}"
	if ref[len(ref)-11] != ':' {
		return "", "", false
	}
	date = ref[len(ref)-10:]
	if len(date) != 10 || date[4] != '-' || date[7] != '-' {
		return "", "", false
	}
	id = ref[:len(ref)-11]
	if id == "" || !isDailyChallenge(id) {
		return "", "", false
	}
	return id, date, true
}

// ledgerScanLimit bounds the ledger read. It comfortably exceeds the number of
// challenges, and a reward that scrolled past it is still protected from a
// double payout by the wallet's unique operation key.
const ledgerScanLimit = 200

// Claim pays a challenge reward. It re-derives completion from stored state on
// every call, so a client that asks for an unearned reward is refused.
func (s *Service) Claim(ctx context.Context, userID primitive.ObjectID, challengeID string) (*Challenge, error) {
	if isDailyChallenge(challengeID) {
		return s.claimDaily(ctx, userID, challengeID)
	}
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

func (s *Service) claimDaily(ctx context.Context, userID primitive.ObjectID, challengeID string) (*Challenge, error) {
	def, ok := dailyByID[challengeID]
	if !ok {
		return nil, ErrUnknownChallenge
	}
	dateUTC := utcDate(s.nowUTC())
	row, err := s.daily.Get(ctx, userID, challengeID, dateUTC)
	if err != nil {
		return nil, err
	}
	current := finalizeDaily(Challenge{
		ID: def.ID, AcademyID: def.AcademyID, Title: def.Title,
		Description: def.Description, Target: def.Target, Reward: def.Reward,
		Progress: row.Progress, Claimed: row.Claimed,
	})
	if current.Claimed {
		return nil, ErrAlreadyClaimed
	}
	if !current.Claimable() {
		return nil, ErrNotComplete
	}
	if _, err := s.wallet.CreditDailyChallengeReward(
		ctx, userID, challengeID, dateUTC,
		fmt.Sprintf("Challenge reward — %s", def.Title), def.Reward,
	); err != nil {
		return nil, err
	}
	if err := s.daily.MarkClaimed(ctx, userID, challengeID, dateUTC); err != nil {
		return nil, err
	}
	current.Claimed = true
	return &current, nil
}

// OnProfitableClose records a fill-time snapshot and increments today's daily
// challenges that the close window qualifies for. Duplicate fill IDs are no-ops.
func (s *Service) OnProfitableClose(ctx context.Context, event positions.CloseFill) error {
	if s == nil || s.daily == nil {
		return nil
	}
	if event.RealizedPnL <= 0 || event.FillID == "" || event.UserID.IsZero() {
		return nil
	}
	closedAt := event.ClosedAt
	if closedAt.IsZero() {
		closedAt = s.nowUTC()
	} else {
		closedAt = closedAt.UTC()
	}
	dateUTC := utcDate(closedAt)
	closeClock := event.Close
	if closeClock.MatchID == "" {
		closeClock.MatchID = event.Open.MatchID
	}
	if closeClock.At.IsZero() {
		closeClock.At = closedAt
	}
	if err := s.daily.RecordClose(ctx, CloseEvent{
		FillID:      event.FillID,
		UserID:      event.UserID,
		DateUTC:     dateUTC,
		RealizedPnL: event.RealizedPnL,
		Open:        event.Open,
		Close:       closeClock,
		CreatedAt:   closedAt,
	}); err != nil {
		return err
	}
	for _, id := range matchingDailyIDs(event.Open, closeClock, event.RealizedPnL) {
		def := dailyByID[id]
		if _, err := s.daily.Increment(ctx, event.UserID, id, dateUTC, event.FillID, def.Target); err != nil {
			return err
		}
	}
	return nil
}
