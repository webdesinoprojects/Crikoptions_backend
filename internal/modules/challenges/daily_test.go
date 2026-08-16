package challenges

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/positions"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func clockAt(format, overs, matchID string, innings int) executions.MatchClock {
	return executions.MatchClock{
		OversText:  overs,
		LegalBalls: executions.ParseLegalBalls(overs),
		Innings:    innings,
		Format:     format,
		MatchID:    matchID,
		At:         time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
	}
}

func TestMatchingWindowsT20(t *testing.T) {
	match := "m1"
	openEarly := clockAt("T20", "4.0", match, 1)

	ids := matchingDailyIDs(openEarly, clockAt("T20", "5.4", match, 1), 10)
	if !containsString(ids, DailyPowerplayPro) || containsString(ids, DailyMiddleOverGenius) || containsString(ids, DailyDeathOverAssassin) {
		t.Fatalf("5.4 ids=%v, want powerplay only", ids)
	}

	ids = matchingDailyIDs(openEarly, clockAt("T20", "6.0", match, 1), 10)
	if !containsString(ids, DailyPowerplayPro) || containsString(ids, DailyMiddleOverGenius) {
		t.Fatalf("6.0 ids=%v, want still powerplay", ids)
	}

	ids = matchingDailyIDs(openEarly, clockAt("T20", "6.1", match, 1), 10)
	if !containsString(ids, DailyMiddleOverGenius) || containsString(ids, DailyPowerplayPro) {
		t.Fatalf("6.1 ids=%v, want middle", ids)
	}

	ids = matchingDailyIDs(openEarly, clockAt("T20", "15.0", match, 1), 10)
	if !containsString(ids, DailyMiddleOverGenius) || containsString(ids, DailyDeathOverAssassin) {
		t.Fatalf("15.0 ids=%v, want middle", ids)
	}

	ids = matchingDailyIDs(openEarly, clockAt("T20", "15.1", match, 1), 10)
	if !containsString(ids, DailyDeathOverAssassin) || containsString(ids, DailyMiddleOverGenius) {
		t.Fatalf("15.1 ids=%v, want death", ids)
	}

	ids = matchingDailyIDs(clockAt("T20", "18.5", match, 1), clockAt("T20", "19.2", match, 1), 10)
	if !containsString(ids, DailyDeathOverAssassin) {
		t.Fatalf("19.2 ids=%v, want death", ids)
	}
	if containsString(ids, DailyLastOverHero) {
		t.Fatalf("opened 18.5 must not count last-over-hero: %v", ids)
	}

	ids = matchingDailyIDs(clockAt("T20", "19.1", match, 1), clockAt("T20", "19.5", match, 1), 10)
	if !containsString(ids, DailyLastOverHero) || !containsString(ids, DailyDeathOverAssassin) {
		t.Fatalf("last-over round trip ids=%v, want last-over + death", ids)
	}
}

func TestMatchingWindowsODI(t *testing.T) {
	match := "odi-1"
	open := clockAt("ODI", "5.0", match, 1)

	ids := matchingDailyIDs(open, clockAt("ODI", "9.5", match, 1), 10)
	if !containsString(ids, DailyPowerplayPro) || containsString(ids, DailyMiddleOverGenius) {
		t.Fatalf("ODI 9.5 ids=%v, want powerplay", ids)
	}
	ids = matchingDailyIDs(open, clockAt("ODI", "10.1", match, 1), 10)
	if !containsString(ids, DailyMiddleOverGenius) || containsString(ids, DailyPowerplayPro) {
		t.Fatalf("ODI 10.1 ids=%v, want middle", ids)
	}
	ids = matchingDailyIDs(open, clockAt("ODI", "40.1", match, 1), 10)
	if !containsString(ids, DailyDeathOverAssassin) {
		t.Fatalf("ODI 40.1 ids=%v, want death", ids)
	}
	ids = matchingDailyIDs(clockAt("ODI", "49.2", match, 1), clockAt("ODI", "49.2", match, 1), 10)
	if !containsString(ids, DailyLastOverHero) {
		t.Fatalf("ODI last over ids=%v, want last-over-hero", ids)
	}
}

func TestMatchingIgnoresLossAndT10(t *testing.T) {
	match := "m1"
	closeClock := clockAt("T20", "5.4", match, 1)
	if ids := matchingDailyIDs(closeClock, closeClock, 0); len(ids) != 0 {
		t.Fatalf("flat close counted: %v", ids)
	}
	if ids := matchingDailyIDs(closeClock, closeClock, -4); len(ids) != 0 {
		t.Fatalf("loss counted: %v", ids)
	}
	t10 := clockAt("T10", "2.0", match, 1)
	if ids := matchingDailyIDs(t10, t10, 10); len(ids) != 0 {
		t.Fatalf("T10 counted: %v", ids)
	}
	missing := clockAt("", "5.4", match, 1)
	ids := matchingDailyIDs(missing, missing, 10)
	if !containsString(ids, DailyPowerplayPro) {
		t.Fatalf("missing format must be treated as T20 powerplay, ids=%v", ids)
	}
}

func TestDailyProgressClaimAndReset(t *testing.T) {
	ctx := context.Background()
	user := primitive.NewObjectID()
	day := time.Date(2026, 8, 16, 15, 0, 0, 0, time.UTC)
	svc, w := newService(nil)
	svc.now = func() time.Time { return day }

	closeAt := func(overs string, fill string) {
		clk := clockAt("T20", overs, "m1", 1)
		if err := svc.OnProfitableClose(ctx, positions.CloseFill{
			FillID: fill, UserID: user, ClosedAt: day,
			RealizedPnL: 12, Open: clk, Close: clk,
		}); err != nil {
			t.Fatalf("close %s: %v", fill, err)
		}
	}

	closeAt("5.4", "f1")
	closeAt("5.4", "f1") // duplicate fill
	closeAt("4.2", "f2")
	closeAt("3.1", "f3")

	got, err := svc.Evaluate(ctx, user)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	pp := byID(got, DailyPowerplayPro)
	if pp.Progress != 3 || pp.Status != StatusComplete || pp.Claimed {
		t.Fatalf("powerplay=%+v, want COMPLETE 3 unclaimed", pp)
	}
	if byID(got, "lc-1").ID != "lc-1" {
		t.Fatal("academy challenges missing from GET")
	}

	claimed, err := svc.Claim(ctx, user, DailyPowerplayPro)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed.Claimed || w.credited[DailyPowerplayPro] != 750 {
		t.Fatalf("claim credited=%v claimed=%v", w.credited, claimed.Claimed)
	}
	if _, err := svc.Claim(ctx, user, DailyPowerplayPro); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("second claim err=%v, want already claimed", err)
	}

	svc.now = func() time.Time { return day.Add(24 * time.Hour) }
	next, err := svc.Evaluate(ctx, user)
	if err != nil {
		t.Fatalf("next day evaluate: %v", err)
	}
	reset := byID(next, DailyPowerplayPro)
	if reset.Progress != 0 || reset.Claimed || reset.Status != StatusInProgress {
		t.Fatalf("after UTC midnight %+v, want 0 unclaimed IN_PROGRESS", reset)
	}
	if _, err := svc.Claim(ctx, user, DailyPowerplayPro); !errors.Is(err, ErrNotComplete) {
		t.Fatalf("unclaimed yesterday must expire, err=%v", err)
	}
}

func TestLastOverHeroCompletesOnOneRoundTrip(t *testing.T) {
	ctx := context.Background()
	user := primitive.NewObjectID()
	day := time.Date(2026, 8, 16, 18, 0, 0, 0, time.UTC)
	svc, _ := newService(nil)
	svc.now = func() time.Time { return day }

	open := clockAt("T20", "19.1", "m1", 1)
	closeClk := clockAt("T20", "19.5", "m1", 1)
	if err := svc.OnProfitableClose(ctx, positions.CloseFill{
		FillID: "last-1", UserID: user, ClosedAt: day,
		RealizedPnL: 40, Open: open, Close: closeClk,
	}); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := svc.Evaluate(ctx, user)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	hero := byID(got, DailyLastOverHero)
	if hero.Progress != 1 || hero.Status != StatusComplete {
		t.Fatalf("last-over-hero=%+v", hero)
	}
	death := byID(got, DailyDeathOverAssassin)
	if death.Progress != 1 {
		t.Fatalf("death progress=%d, want 1", death.Progress)
	}
}

func TestDailyChallengesNeverLocked(t *testing.T) {
	for _, c := range emptyDailyChallenges() {
		if c.Status != StatusInProgress {
			t.Fatalf("%s status=%s, want IN_PROGRESS at 0", c.ID, c.Status)
		}
	}
}
