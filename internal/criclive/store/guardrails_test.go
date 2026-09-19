package store

import (
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
)

func TestMaxLiveWindowByFormat(t *testing.T) {
	if got := MaxLiveWindow("T20"); got != MaxLiveWindowT20 {
		t.Fatalf("T20 window = %s", got)
	}
	if got := MaxLiveWindow("t20"); got != MaxLiveWindowT20 {
		t.Fatalf("format match must be case-insensitive, got %s", got)
	}
	if got := MaxLiveWindow("ODI"); got != MaxLiveWindowODI {
		t.Fatalf("ODI window = %s", got)
	}
	if got := MaxLiveWindow(""); got != MaxLiveWindowODI {
		t.Fatalf("unknown format must take the wider window, got %s", got)
	}
	if MaxRequestsPerFixture("T20") != MaxRequestsT20 || MaxRequestsPerFixture("ODI") != MaxRequestsODI {
		t.Fatal("request budget by format")
	}
	// The budgets are strict: a full match plus about a fifth in hand, and
	// never a whole day's allowance for one fixture.
	if MaxRequestsT20 < 2100 || MaxRequestsT20 > 2600 {
		t.Fatalf("T20 budget %d is not a full match (~2,100 at 6s) plus a margin", MaxRequestsT20)
	}
	if MaxRequestsODI < 2850 || MaxRequestsODI > 3700 {
		t.Fatalf("ODI budget %d is not a full match (~2,850 at 10s) plus a margin", MaxRequestsODI)
	}
}

// The window is measured from the later of the scheduled start and the first
// live sighting, so a rain-delayed match that started hours late is not
// parked mid-play — but a fixture never seen live is judged on its schedule.
func TestLiveWindowAnchoredOnLaterOfStartAndLiveSince(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	scheduled := now.Add(-9 * time.Hour)
	t20 := FixtureTarget{Format: "T20", StartTime: scheduled}
	if !LiveWindowExceeded(t20, now) {
		t.Fatal("a T20 scheduled 9h ago with no live sighting must be outside its window")
	}
	delayedStart := now.Add(-3 * time.Hour)
	t20.LiveSince = &delayedStart
	if LiveWindowExceeded(t20, now) {
		t.Fatal("a T20 first seen live 3h ago is still in play")
	}
	earlySighting := scheduled.Add(-time.Hour)
	t20.LiveSince = &earlySighting
	if !LiveWindowExceeded(t20, now) {
		t.Fatal("a live sighting before the schedule must not extend the window")
	}
	odi := FixtureTarget{Format: "ODI", StartTime: now.Add(-11 * time.Hour)}
	if LiveWindowExceeded(odi, now) {
		t.Fatal("an ODI 11h in is inside its 12h window")
	}
	odi.StartTime = now.Add(-13 * time.Hour)
	if !LiveWindowExceeded(odi, now) {
		t.Fatal("an ODI 13h in is outside its window")
	}
	if LiveWindowExceeded(FixtureTarget{Format: "T20"}, now) {
		t.Fatal("a fixture with no start time cannot be judged")
	}
}

func TestOverrunReasonOnlyJudgesLiveUnparkedTargets(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	zombie := FixtureTarget{Format: "T20", ProviderStatus: client.StateInProgress, StartTime: now.Add(-6 * 24 * time.Hour)}
	if got := OverrunReason(zombie, now); got != ParkReasonLiveWindow {
		t.Fatalf("days-old live fixture reason = %q", got)
	}
	spent := FixtureTarget{Format: "T20", ProviderStatus: client.StateInProgress, StartTime: now.Add(-time.Hour), RequestCount: MaxRequestsT20}
	if got := OverrunReason(spent, now); got != ParkReasonRequestBudget {
		t.Fatalf("budget-exhausted fixture reason = %q", got)
	}
	healthy := FixtureTarget{Format: "T20", ProviderStatus: client.StateInProgress, StartTime: now.Add(-2 * time.Hour), RequestCount: 1200}
	if got := OverrunReason(healthy, now); got != "" {
		t.Fatalf("healthy live fixture parked for %q", got)
	}
	// A finished or not-started fixture is on another schedule already.
	finished := zombie
	finished.ProviderStatus = "Complete"
	if got := OverrunReason(finished, now); got != "" {
		t.Fatalf("finished fixture judged live: %q", got)
	}
	// Once parked, the reason is not re-derived.
	parked := zombie
	parked.ParkedReason = ParkReasonAbsentFromLive
	if got := OverrunReason(parked, now); got != "" {
		t.Fatalf("parked fixture re-judged: %q", got)
	}
}

// Discovery re-arms every live fixture on each pass; a parked one must keep
// its schedule whatever the feed says.
func TestFixtureTargetNextPollKeepsParkedSchedule(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	live := client.Fixture{ID: 1, State: client.StateInProgress}
	parked := &FixtureTarget{ID: 1, ProviderStatus: client.StateInProgress, ParkedReason: ParkReasonLiveWindow, NextPollAt: now.Add(TerminalFixtureRecheck)}
	if _, apply, _ := fixtureTargetNextPoll(parked, live, now.Add(-time.Hour), now); apply {
		t.Fatal("a parked live fixture was re-armed by discovery")
	}
	unparked := &FixtureTarget{ID: 1, ProviderStatus: client.StateInProgress}
	if next, apply, _ := fixtureTargetNextPoll(unparked, live, now.Add(-time.Hour), now); !apply || !next.Equal(now) {
		t.Fatalf("an unparked live fixture must poll now, got apply=%t next=%s", apply, next)
	}
}
