package challenges

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/positions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

var base = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

func pos(side, market string, matched int, realized float64, at time.Time) positions.Position {
	return positions.Position{
		MatchID: "m1", MarketID: market, Side: side,
		MatchedLots: matched, RealizedPnL: realized,
		Status: "closed", UpdatedAt: at,
	}
}

func byID(list []Challenge, id string) Challenge {
	for _, c := range list {
		if c.ID == id {
			return c
		}
	}
	return Challenge{}
}

func TestProgressIsDerivedFromRealPositions(t *testing.T) {
	// One open buy, nothing closed: started but nothing earned.
	open := positions.Position{MatchID: "m1", MarketID: "k1", Side: sideBuy, Lots: 5, Status: "open"}
	got := EvaluatePositions([]positions.Position{open})

	if c := byID(got, "lc-1"); c.Status != StatusComplete {
		t.Fatalf("lc-1 status=%s progress=%d, want COMPLETE after a buy", c.Status, c.Progress)
	}
	if c := byID(got, "lc-2"); c.Status == StatusComplete {
		t.Fatal("lc-2 must not complete without a profitable close")
	}
	// A buy must not advance the short-call academy.
	if c := byID(got, "sc-1"); c.Status == StatusComplete {
		t.Fatal("a BUY position completed a short-call challenge")
	}
}

func TestProfitableClosesAndPerInningsGrouping(t *testing.T) {
	got := EvaluatePositions([]positions.Position{
		pos(sideBuy, "k1", 1, 10, base),
		pos(sideBuy, "k1", 1, 10, base.Add(time.Minute)),
		pos(sideBuy, "k2", 1, 10, base.Add(2*time.Minute)), // different innings
	})

	if c := byID(got, "lc-2"); c.Status != StatusComplete {
		t.Fatalf("lc-2 = %s, want COMPLETE", c.Status)
	}
	// 3 wins overall but only 2 inside one innings: the innings rule must hold.
	if c := byID(got, "lc-3"); c.Status == StatusComplete {
		t.Fatalf("lc-3 completed on %d wins spread across innings", c.Progress)
	}
}

func TestStreakBreaksOnALoss(t *testing.T) {
	var book []positions.Position
	for i := 0; i < 4; i++ {
		book = append(book, pos(sideSell, "k1", 1, 10, base.Add(time.Duration(i)*time.Minute)))
	}
	book = append(book, pos(sideSell, "k1", 1, -5, base.Add(4*time.Minute))) // breaks it
	book = append(book, pos(sideSell, "k1", 1, 10, base.Add(5*time.Minute)))

	got := EvaluatePositions(book)
	if c := byID(got, "sc-4"); c.Status == StatusComplete {
		t.Fatalf("sc-4 completed with a losing trade inside the run (progress=%d)", c.Progress)
	}
	// 5 profitable closes exist, so the non-consecutive challenge does complete.
	if c := byID(got, "sc-3"); c.Status != StatusComplete {
		t.Fatalf("sc-3 = %s, want COMPLETE on 5 wins in one innings", c.Status)
	}
}

func TestUnverifiableChallengesStayLockedAndUnclaimable(t *testing.T) {
	for _, id := range []string{"lc-5", "sc-5"} {
		c := byID(EvaluatePositions(nil), id)
		if c.Status != StatusLocked || c.LockedReason == "" {
			t.Fatalf("%s = %s reason=%q, want LOCKED with a reason", id, c.Status, c.LockedReason)
		}
		if c.Claimable() {
			t.Fatalf("%s must never be claimable while unverifiable", id)
		}
	}
}

// ── claim path ───────────────────────────────────────────────────────────────

type fakePositions struct{ book []positions.Position }

func (f fakePositions) ListUserPositions(context.Context, primitive.ObjectID, positions.PositionFilter) ([]positions.Position, error) {
	return f.book, nil
}

type fakeWallet struct {
	credited map[string]float64
	ledger   []wallet.LedgerEntry
}

func (f *fakeWallet) CreditChallengeReward(_ context.Context, _ primitive.ObjectID, challengeID, _ string, amount float64) (*wallet.AdjustmentResult, error) {
	if f.credited == nil {
		f.credited = map[string]float64{}
	}
	f.credited[challengeID] += amount
	f.ledger = append(f.ledger, wallet.LedgerEntry{Type: wallet.LedgerChallengeReward, ReferenceID: challengeID, Amount: amount})
	return &wallet.AdjustmentResult{}, nil
}

func (f *fakeWallet) GetLedger(context.Context, primitive.ObjectID, int64) ([]wallet.LedgerEntry, error) {
	return f.ledger, nil
}

func newService(book []positions.Position) (*Service, *fakeWallet) {
	w := &fakeWallet{}
	return NewService(fakePositions{book: book}, w), w
}

func TestClaimRefusesAnUnearnedReward(t *testing.T) {
	svc, w := newService(nil) // no trading at all
	_, err := svc.Claim(context.Background(), primitive.NewObjectID(), "lc-1")
	if !errors.Is(err, ErrNotComplete) {
		t.Fatalf("err=%v, want ErrNotComplete", err)
	}
	if len(w.credited) != 0 {
		t.Fatalf("wallet was credited for an unearned challenge: %v", w.credited)
	}
}

func TestClaimPaysTheServerSideRewardOnce(t *testing.T) {
	svc, w := newService([]positions.Position{pos(sideBuy, "k1", 1, 10, base)})
	user := primitive.NewObjectID()

	claimed, err := svc.Claim(context.Background(), user, "lc-1")
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !claimed.Claimed {
		t.Fatal("returned challenge not marked claimed")
	}
	if w.credited["lc-1"] != definitionByID["lc-1"].Reward {
		t.Fatalf("credited %v, want the server-owned reward %v", w.credited["lc-1"], definitionByID["lc-1"].Reward)
	}

	// Second attempt must be refused, and must not move any more money.
	if _, err := svc.Claim(context.Background(), user, "lc-1"); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("second claim err=%v, want ErrAlreadyClaimed", err)
	}
	if w.credited["lc-1"] != definitionByID["lc-1"].Reward {
		t.Fatalf("reward paid twice: %v", w.credited["lc-1"])
	}
}

func TestClaimRejectsUnknownAndLockedChallenges(t *testing.T) {
	svc, w := newService([]positions.Position{pos(sideBuy, "k1", 1, 10, base)})
	user := primitive.NewObjectID()

	if _, err := svc.Claim(context.Background(), user, "does-not-exist"); !errors.Is(err, ErrUnknownChallenge) {
		t.Fatalf("unknown challenge err=%v", err)
	}
	if _, err := svc.Claim(context.Background(), user, "lc-5"); !errors.Is(err, ErrNotComplete) {
		t.Fatalf("locked challenge err=%v, want ErrNotComplete", err)
	}
	if len(w.credited) != 0 {
		t.Fatalf("wallet moved for a rejected claim: %v", w.credited)
	}
}

func TestEvaluateReportsAlreadyClaimedRewards(t *testing.T) {
	svc, _ := newService([]positions.Position{pos(sideBuy, "k1", 1, 10, base)})
	user := primitive.NewObjectID()
	if _, err := svc.Claim(context.Background(), user, "lc-1"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	got, err := svc.Evaluate(context.Background(), user)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	c := byID(got, "lc-1")
	if !c.Claimed || c.Claimable() {
		t.Fatalf("lc-1 claimed=%v claimable=%v, want claimed and not claimable", c.Claimed, c.Claimable())
	}
}
