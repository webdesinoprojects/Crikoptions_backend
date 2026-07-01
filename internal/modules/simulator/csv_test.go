package simulator

import (
	"path/filepath"
	"testing"
)

func TestLoadDataset_rcb_vs_kkr(t *testing.T) {
	dataDir := filepath.Join("..", "..", "..", "data", "simulator")
	ds, err := LoadDataset(dataDir, "rcb_vs_kkr")
	if err != nil {
		t.Fatalf("LoadDataset: %v", err)
	}

	if ds.MatchID != "0000000000000000000000bb" {
		t.Fatalf("MatchID = %q, want …bb", ds.MatchID)
	}
	if !ds.HasInnings2 {
		t.Fatal("expected innings 2 config")
	}
	if len(ds.Events[1]) == 0 {
		t.Fatal("innings 1 events empty")
	}
	if len(ds.Events[2]) == 0 {
		t.Fatal("innings 2 events empty")
	}

	// RCB 177/8 at end of innings 1.
	lastI1 := ds.Events[1][len(ds.Events[1])-1]
	if !lastI1.EndInnings {
		t.Fatalf("last innings-1 row end_innings = false, want true")
	}
	if lastI1.ScoreAfter != 177 || lastI1.WicketsAfter != 8 {
		t.Fatalf("innings 1 final = %d/%d, want 177/8", lastI1.ScoreAfter, lastI1.WicketsAfter)
	}

	// KKR chase target 178; win 179/4.
	if ds.Innings2.TargetScore != 178 {
		t.Fatalf("innings 2 target = %d, want 178", ds.Innings2.TargetScore)
	}
	if ds.Innings2.StartStriker != "Phil Salt" {
		t.Fatalf("innings 2 striker = %q, want Phil Salt", ds.Innings2.StartStriker)
	}

	lastI2 := ds.Events[2][len(ds.Events[2])-1]
	if !lastI2.EndMatch {
		t.Fatal("last innings-2 row should end_match=true")
	}
	if lastI2.ScoreAfter != 179 || lastI2.WicketsAfter != 4 {
		t.Fatalf("innings 2 final = %d/%d, want 179/4", lastI2.ScoreAfter, lastI2.WicketsAfter)
	}
}

func TestLoadDataset_csk_vs_mi(t *testing.T) {
	dataDir := filepath.Join("..", "..", "..", "data", "simulator")
	ds, err := LoadDataset(dataDir, "csk_vs_mi")
	if err != nil {
		t.Fatalf("LoadDataset: %v", err)
	}
	if ds.MatchID != "0000000000000000000000aa" {
		t.Fatalf("MatchID = %q, want …aa", ds.MatchID)
	}
	if len(ds.Events[1]) == 0 || len(ds.Events[2]) == 0 {
		t.Fatal("expected events for both innings")
	}
}
