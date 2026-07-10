package matches

import (
	"context"
	"testing"
)

func liveSeedService(t *testing.T) *Service {
	t.Helper()
	// Memory repo seeds CSK (id "1"/aa) live and RCB (id "2"/bb) live.
	return NewService(NewMemoryRepository(), NewMemoryEventRepository(), nil)
}

func feedBall(t *testing.T, svc *Service, matchID string, runs int, wicket bool, extra string) {
	t.Helper()
	if _, err := svc.RecordBall(context.Background(), matchID, BallEventRequest{
		Runs:     runs,
		IsWicket: wicket,
		Extra:    extra,
	}); err != nil {
		t.Fatalf("RecordBall(%s, %d): %v", matchID, runs, err)
	}
}

func runsOf(events []BallEventResponse) []int {
	out := make([]int, len(events))
	for i, e := range events {
		out[i] = e.Runs
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestEvents_ThisOverOrderPerMatch(t *testing.T) {
	svc := liveSeedService(t)

	// Feed 6,1,2 on RCB vs KKR (short id "2").
	feedBall(t, svc, "2", 6, false, "")
	feedBall(t, svc, "2", 1, false, "")
	feedBall(t, svc, "2", 2, false, "")

	got, err := svc.GetRecentEvents(context.Background(), "2", 6)
	if err != nil {
		t.Fatalf("GetRecentEvents: %v", err)
	}
	if want := []int{6, 1, 2}; !equalInts(runsOf(got), want) {
		t.Fatalf("RCB runs = %v, want %v", runsOf(got), want)
	}
	// Over/ball positions must be exact.
	for i, e := range got {
		if e.Over != 0 || e.Ball != i+1 {
			t.Fatalf("event %d over/ball = %d/%d, want 0/%d", i, e.Over, e.Ball, i+1)
		}
	}
}

func TestEvents_IndependentPerMatch(t *testing.T) {
	svc := liveSeedService(t)

	feedBall(t, svc, "2", 6, false, "")
	feedBall(t, svc, "2", 1, false, "")
	feedBall(t, svc, "1", 4, false, "") // CSK vs MI

	rcb, _ := svc.GetRecentEvents(context.Background(), "2", 6)
	csk, _ := svc.GetRecentEvents(context.Background(), "1", 6)

	if want := []int{6, 1}; !equalInts(runsOf(rcb), want) {
		t.Fatalf("RCB runs = %v, want %v", runsOf(rcb), want)
	}
	if want := []int{4}; !equalInts(runsOf(csk), want) {
		t.Fatalf("CSK runs = %v, want %v", runsOf(csk), want)
	}
}

func TestEvents_ExtraDoesNotConsumeLegalBall(t *testing.T) {
	svc := liveSeedService(t)

	feedBall(t, svc, "2", 6, false, "")     // legal ball 1
	feedBall(t, svc, "2", 1, false, "wide") // extra, not legal
	feedBall(t, svc, "2", 2, false, "")     // legal ball 2

	got, _ := svc.GetRecentEvents(context.Background(), "2", 6)
	if want := []int{6, 1, 2}; !equalInts(runsOf(got), want) {
		t.Fatalf("runs = %v, want %v", runsOf(got), want)
	}

	// The wide must share the next legal ball's position (over 0, ball 2) and
	// the following legal ball must also be ball 2 (wide did not advance count).
	if got[1].Extra == nil || *got[1].Extra != ExtraWide {
		t.Fatalf("event[1].extra = %v, want wide", got[1].Extra)
	}
	if got[1].Ball != 2 || got[2].Ball != 2 {
		t.Fatalf("ball positions = %d,%d, want 2,2", got[1].Ball, got[2].Ball)
	}
}

func TestEvents_LimitReturnsLastN(t *testing.T) {
	svc := liveSeedService(t)
	for _, r := range []int{1, 2, 3, 4, 6, 0, 1} { // 7 legal balls
		feedBall(t, svc, "2", r, false, "")
	}
	got, _ := svc.GetRecentEvents(context.Background(), "2", 6)
	if want := []int{2, 3, 4, 6, 0, 1}; !equalInts(runsOf(got), want) {
		t.Fatalf("runs = %v, want last 6 %v", runsOf(got), want)
	}
}

func TestEvents_InningsEventsReturnsChronologicalFromStart(t *testing.T) {
	svc := liveSeedService(t)
	for _, r := range []int{1, 2, 3, 4, 6, 0, 1} {
		feedBall(t, svc, "2", r, false, "")
	}

	got, err := svc.GetInningsEvents(context.Background(), "2", 1, 120)
	if err != nil {
		t.Fatalf("GetInningsEvents: %v", err)
	}

	runs := make([]int, len(got))
	for i, event := range got {
		runs[i] = event.Runs
	}
	if want := []int{1, 2, 3, 4, 6, 0, 1}; !equalInts(runs, want) {
		t.Fatalf("runs = %v, want full innings %v", runs, want)
	}
}

func TestEvents_EmptyWhenNoBalls(t *testing.T) {
	svc := liveSeedService(t)
	got, err := svc.GetRecentEvents(context.Background(), "1", 6)
	if err != nil {
		t.Fatalf("GetRecentEvents: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no events, got %d", len(got))
	}
}
