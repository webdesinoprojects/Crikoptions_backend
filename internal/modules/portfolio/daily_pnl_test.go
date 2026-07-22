package portfolio

import (
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/positions"
)

func istTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

// A position carried in from an earlier day must not contribute its lifetime
// mark-to-market to today's number. This is the defect that made "Today's P&L"
// show a drifting non-zero value for users who had not traded today.
func TestDailyPnLExcludesCarriedPositionMarkToMarket(t *testing.T) {
	now := istTime(t, "2026-07-22T14:00:00+05:30")
	open := []PortfolioPosition{{
		UnrealizedPnL: 4200,
		RealizedPnL:   150,
		OpenedAt:      "2026-07-20T18:30:00+05:30",
	}}

	if got := computeDailyPnL(open, nil, now); got != 0 {
		t.Fatalf("dailyPnL = %.2f, want 0 for a position opened two days ago", got)
	}
}

func TestDailyPnLCountsTodaysPositionMarkToMarketAndRealized(t *testing.T) {
	now := istTime(t, "2026-07-22T14:00:00+05:30")
	open := []PortfolioPosition{{
		UnrealizedPnL: 300,
		RealizedPnL:   75,
		OpenedAt:      "2026-07-22T09:15:00+05:30",
	}}

	if got := computeDailyPnL(open, nil, now); got != 375 {
		t.Fatalf("dailyPnL = %.2f, want 375", got)
	}
}

func TestDailyPnLCountsOnlyTradesClosedToday(t *testing.T) {
	now := istTime(t, "2026-07-22T14:00:00+05:30")
	closed := []ClosedTrade{
		{RealizedPnL: 500, ClosedAt: "2026-07-22T11:00:00+05:30"},
		{RealizedPnL: 9000, ClosedAt: "2026-07-21T23:00:00+05:30"},
	}

	if got := computeDailyPnL(nil, closed, now); got != 500 {
		t.Fatalf("dailyPnL = %.2f, want 500", got)
	}
}

func TestDailyPnLIsZeroWithNoActivityToday(t *testing.T) {
	now := istTime(t, "2026-07-22T14:00:00+05:30")
	open := []PortfolioPosition{{UnrealizedPnL: -1234.56, OpenedAt: "2026-07-19T20:00:00+05:30"}}
	closed := []ClosedTrade{{RealizedPnL: 999, ClosedAt: "2026-07-18T20:00:00+05:30"}}

	if got := computeDailyPnL(open, closed, now); got != 0 {
		t.Fatalf("dailyPnL = %.2f, want 0", got)
	}
}

// The trading day rolls at IST midnight, not UTC midnight. A trade at 02:00 IST
// is today's; the same instant is 20:30 UTC "yesterday", which is what the old
// time.Local comparison reported on a UTC container.
func TestTradingDayRollsAtISTMidnight(t *testing.T) {
	now := istTime(t, "2026-07-22T02:00:00+05:30")
	closed := []ClosedTrade{{RealizedPnL: 250, ClosedAt: "2026-07-22T01:00:00+05:30"}}

	if got := computeDailyPnL(nil, closed, now); got != 250 {
		t.Fatalf("dailyPnL = %.2f, want 250 for a 01:00 IST trade", got)
	}

	beforeMidnight := []ClosedTrade{{RealizedPnL: 250, ClosedAt: "2026-07-21T23:30:00+05:30"}}
	if got := computeDailyPnL(nil, beforeMidnight, now); got != 0 {
		t.Fatalf("dailyPnL = %.2f, want 0 for a 23:30 IST trade on the prior day", got)
	}
}

func TestUnparseableOpenedAtIsNotCountedAsToday(t *testing.T) {
	now := istTime(t, "2026-07-22T14:00:00+05:30")
	open := []PortfolioPosition{{UnrealizedPnL: 777, OpenedAt: ""}}

	if got := computeDailyPnL(open, nil, now); got != 0 {
		t.Fatalf("dailyPnL = %.2f, want 0", got)
	}
}

// A zero-lot row carries no exposure. Counting it produced the phantom
// "1 open position" card that no exit flow could clear.
func TestFilterPositionsByStatusDropsZeroLotOpenRows(t *testing.T) {
	in := []positions.Position{
		{ID: "phantom", Status: "open", Lots: 0},
		{ID: "real", Status: "open", Lots: 5},
		{ID: "done", Status: "closed", Lots: 0},
	}

	open := filterPositionsByStatus(in, "open")
	if len(open) != 1 || open[0].ID != "real" {
		t.Fatalf("open = %+v, want only the 5-lot position", open)
	}

	if closed := filterPositionsByStatus(in, "closed"); len(closed) != 1 {
		t.Fatalf("closed = %+v, want the zero-lot closed row kept", closed)
	}
}
