package simulator

import (
	"testing"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

func TestDeriveResumePlan_freshStart(t *testing.T) {
	match := &matches.Match{Status: matches.StatusLive, Innings: 1, BallsLeft: 120}
	plan := deriveResumePlan(match, &CSVDataset{Events: map[int][]BallRow{1: {{}}, 2: {}}}, map[int]int{1: 0, 2: 0}, map[int]int{1: 0, 2: 0}, false)
	if !plan.freshStart || plan.skip {
		t.Fatalf("plan = %+v, want freshStart", plan)
	}
}

func TestDeriveResumePlan_resumeMidInnings(t *testing.T) {
	match := &matches.Match{
		Status: matches.StatusLive, Innings: 1,
		CurrentScore: 42, WicketsLost: 1, BallsLeft: 70,
	}
	ds := &CSVDataset{
		HasInnings2: true,
		Events: map[int][]BallRow{
			1: make([]BallRow, 121),
			2: make([]BallRow, 94),
		},
	}
	plan := deriveResumePlan(match, ds, map[int]int{1: 50, 2: 0}, map[int]int{1: 50, 2: 0}, false)
	if plan.freshStart || plan.skip {
		t.Fatalf("plan = %+v, want resume at cursor 50", plan)
	}
	if plan.innings != 1 || plan.cursor != 50 {
		t.Fatalf("plan = %+v, want innings=1 cursor=50", plan)
	}
}

func TestDeriveResumePlan_innings2AfterInnings1Done(t *testing.T) {
	match := &matches.Match{Status: matches.StatusLive, Innings: 1, CurrentScore: 177, BallsLeft: 0}
	ds := &CSVDataset{
		HasInnings2: true,
		Events: map[int][]BallRow{
			1: make([]BallRow, 121),
			2: make([]BallRow, 94),
		},
	}
	plan := deriveResumePlan(match, ds, map[int]int{1: 121, 2: 10}, map[int]int{1: 120, 2: 10}, false)
	if plan.skip || plan.freshStart {
		t.Fatalf("plan = %+v, want resume innings 2", plan)
	}
	if plan.innings != 2 || plan.cursor != 10 {
		t.Fatalf("plan = %+v, want innings=2 cursor=10", plan)
	}
}

func TestDeriveResumePlan_skipCompleted(t *testing.T) {
	match := &matches.Match{Status: matches.StatusCompleted, Innings: 2}
	ds := &CSVDataset{Events: map[int][]BallRow{1: make([]BallRow, 10), 2: make([]BallRow, 10)}}
	plan := deriveResumePlan(match, ds, map[int]int{1: 10, 2: 10}, map[int]int{1: 10, 2: 10}, false)
	if !plan.skip {
		t.Fatalf("plan = %+v, want skip for completed match", plan)
	}
}

func TestDeriveResumePlan_autoLoopRestartsCompleted(t *testing.T) {
	match := &matches.Match{Status: matches.StatusCompleted, Innings: 2}
	ds := &CSVDataset{Events: map[int][]BallRow{1: make([]BallRow, 10), 2: make([]BallRow, 10)}}
	plan := deriveResumePlan(match, ds, map[int]int{1: 10, 2: 10}, map[int]int{1: 10, 2: 10}, true)
	if !plan.freshStart || plan.skip {
		t.Fatalf("plan = %+v, want freshStart when autoLoop", plan)
	}
}

func TestDeriveResumePlan_freshStartsWhenHistoryAndScoreDisagree(t *testing.T) {
	match := &matches.Match{Status: matches.StatusLive, Innings: 1, BallsLeft: 32}
	ds := &CSVDataset{
		HasInnings2: true,
		Events: map[int][]BallRow{
			1: make([]BallRow, 121),
			2: make([]BallRow, 94),
		},
	}
	plan := deriveResumePlan(match, ds, map[int]int{1: 92, 2: 0}, map[int]int{1: 91, 2: 0}, true)
	if !plan.freshStart || plan.skip {
		t.Fatalf("plan = %+v, want freshStart for stale event history", plan)
	}
}

func TestDeriveResumePlan_skipAllEventsApplied(t *testing.T) {
	match := &matches.Match{Status: matches.StatusLive, Innings: 2, BallsLeft: 117}
	ds := &CSVDataset{
		HasInnings2: true,
		Events: map[int][]BallRow{
			1: make([]BallRow, 5),
			2: make([]BallRow, 3),
		},
	}
	plan := deriveResumePlan(match, ds, map[int]int{1: 5, 2: 3}, map[int]int{1: 5, 2: 3}, false)
	if !plan.skip {
		t.Fatalf("plan = %+v, want skip when all events applied", plan)
	}
}
