package worker

import (
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/store"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

// A scoreboard that has not changed for quietFeedAfter is read at the break
// cadence, whatever phase the match claims to be in. This is what turned the
// 15s finalizing loop on a match that ended days ago into one read a minute.
func TestQuietFeedIntervalSlowsUnchangedSnapshots(t *testing.T) {
	cfg := client.Config{BreakInterval: time.Minute, FinalizingInterval: 15 * time.Second}
	fast := 6 * time.Second
	if got := quietFeedInterval(fast, matches.StatusLive, quietFeedAfter-time.Second, cfg); got != fast {
		t.Fatalf("a feed quiet for under the threshold was slowed to %s", got)
	}
	if got := quietFeedInterval(fast, matches.StatusLive, quietFeedAfter, cfg); got != time.Minute {
		t.Fatalf("quiet live feed interval = %s, want %s", got, time.Minute)
	}
	if got := quietFeedInterval(cfg.FinalizingInterval, matches.StatusInningsBreak, time.Hour, cfg); got != time.Minute {
		t.Fatalf("quiet innings-break interval = %s, want %s", got, time.Minute)
	}
	// Never shorten, and leave other schedules alone.
	if got := quietFeedInterval(15*time.Minute, matches.StatusUpcoming, time.Hour, cfg); got != 15*time.Minute {
		t.Fatalf("pre-match schedule was changed to %s", got)
	}
	if got := quietFeedInterval(5*time.Minute, matches.StatusLive, time.Hour, cfg); got != 5*time.Minute {
		t.Fatalf("a longer interval was shortened to %s", got)
	}
}

// An ODI is never read at the T20 cadence: at 6s one ODI alone spends most
// of the day's allowance and blows through its strict request budget.
func TestFormatPollIntervalHoldsODIToMaxCadence(t *testing.T) {
	cfg := client.Config{MinPollInterval: 6 * time.Second, MaxPollInterval: 10 * time.Second}
	if got := formatPollInterval("T20", cfg.MinPollInterval, cfg); got != cfg.MinPollInterval {
		t.Fatalf("T20 cadence = %s, want %s", got, cfg.MinPollInterval)
	}
	if got := formatPollInterval("ODI", cfg.MinPollInterval, cfg); got != cfg.MaxPollInterval {
		t.Fatalf("ODI cadence = %s, want %s", got, cfg.MaxPollInterval)
	}
	if got := formatPollInterval("odi", cfg.MinPollInterval, cfg); got != cfg.MaxPollInterval {
		t.Fatalf("format match must be case-insensitive, got %s", got)
	}
	// A cadence the budget pacer has already stretched is left alone.
	if got := formatPollInterval("ODI", 20*time.Second, cfg); got != 20*time.Second {
		t.Fatalf("paced ODI cadence was shortened to %s", got)
	}
}

func TestSnapshotUnchangedSinceTracksIdenticalReads(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-10 * time.Minute)
	target := store.FixtureTarget{LastSnapshotHash: "abc", UnchangedSince: &earlier}
	if got := snapshotUnchangedSince(target, "abc", now); !got.Equal(earlier) {
		t.Fatalf("identical snapshot must keep the earlier stamp, got %s", got)
	}
	if got := snapshotUnchangedSince(target, "def", now); !got.Equal(now) {
		t.Fatalf("a changed snapshot must restart the clock, got %s", got)
	}
	if got := snapshotUnchangedSince(store.FixtureTarget{LastSnapshotHash: "abc"}, "abc", now); !got.Equal(now) {
		t.Fatalf("a target without a stamp starts the clock now, got %s", got)
	}
	if got := snapshotUnchangedSince(target, "", now); !got.Equal(now) {
		t.Fatalf("an empty hash must not count as unchanged, got %s", got)
	}
}

// A fixture the live feed has dropped is probed slowly even when its own
// endpoint still answers; only discovery may restore the live cadence.
func TestClampAbsentProbeHoldsSlowCadence(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fast := now.Add(6 * time.Second)
	absent := store.FixtureTarget{ParkedReason: store.ParkReasonAbsentFromLive}
	if got := clampAbsentProbe(absent, now, fast); !got.Equal(now.Add(store.AbsentProbeInterval)) {
		t.Fatalf("absent fixture next poll = %s, want %s", got, now.Add(store.AbsentProbeInterval))
	}
	later := now.Add(time.Hour)
	if got := clampAbsentProbe(absent, now, later); !got.Equal(later) {
		t.Fatalf("a later schedule was pulled forward to %s", got)
	}
	if got := clampAbsentProbe(store.FixtureTarget{}, now, fast); !got.Equal(fast) {
		t.Fatalf("a listed fixture was slowed to %s", got)
	}
}

// As the day's allowance runs down the live cadence stretches rather than
// stopping dead at midday.
func TestBudgetPacedIntervalStretchesWithSpend(t *testing.T) {
	cfg := client.Config{MinPollInterval: 6 * time.Second, MaxPollInterval: 10 * time.Second}
	base := cfg.MinPollInterval
	usable := 4000
	if got, tier := budgetPacedInterval(base, 1000, usable, cfg); got != base || tier != "" {
		t.Fatalf("25%% spent: interval=%s tier=%q", got, tier)
	}
	if got, tier := budgetPacedInterval(base, 2000, usable, cfg); got != cfg.MaxPollInterval || tier == "" {
		t.Fatalf("50%% spent: interval=%s tier=%q", got, tier)
	}
	if got, _ := budgetPacedInterval(base, 3000, usable, cfg); got != 2*cfg.MaxPollInterval {
		t.Fatalf("75%% spent: interval=%s", got)
	}
	if got, _ := budgetPacedInterval(base, 4000, usable, cfg); got != 2*cfg.MaxPollInterval {
		t.Fatalf("fully spent: interval=%s", got)
	}
	// A base already slower than the tier is left alone.
	if got, _ := budgetPacedInterval(time.Minute, 3000, usable, cfg); got != time.Minute {
		t.Fatalf("slow base was shortened to %s", got)
	}
	if got, _ := budgetPacedInterval(base, 3000, 0, cfg); got != base {
		t.Fatalf("unknown allowance must not pace, got %s", got)
	}
	if usableDailyRequests(client.Config{DailyRequestLimit: 5000, QuotaReservePercent: 20}) != 4000 {
		t.Fatal("usable daily requests must apply the reserve")
	}
}
