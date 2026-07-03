package simulator

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

var cleanBallHeaders = []string{
	"event_seq", "innings", "runs", "is_wicket", "extra",
	"next_batter_name", "wicket_type", "delay_sec",
	"score_after", "wickets_after", "commentary",
	"end_innings", "end_match", "change_bowler",
}

var cleanConfigHeaders = []string{
	"match_id", "innings", "replay_interval_sec",
	"start_striker", "start_non_striker", "start_bowler", "target_score",
}

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
	if lastI1.BallsLeftAfter != 0 {
		t.Fatalf("innings 1 final balls left = %d, want 0", lastI1.BallsLeftAfter)
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

func TestSimulatorCSVHeadersAreClean(t *testing.T) {
	dataDir := filepath.Join("..", "..", "..", "data", "simulator")
	for _, script := range []string{"csk_vs_mi", "rcb_vs_kkr"} {
		ballPath := filepath.Join(dataDir, script, "ball_events_full_match.csv")
		if got := readCSVHeader(t, ballPath); !reflect.DeepEqual(got, cleanBallHeaders) {
			t.Fatalf("%s header = %#v, want %#v", ballPath, got, cleanBallHeaders)
		}

		configPath := filepath.Join(dataDir, script, "matches_config.csv")
		if got := readCSVHeader(t, configPath); !reflect.DeepEqual(got, cleanConfigHeaders) {
			t.Fatalf("%s header = %#v, want %#v", configPath, got, cleanConfigHeaders)
		}

		for _, removed := range []string{"ball_events.csv", "ball_events_innings1.csv", "ball_events_innings2.csv"} {
			if _, err := os.Stat(filepath.Join(dataDir, script, removed)); !os.IsNotExist(err) {
				t.Fatalf("%s should not exist after CSV cleanup", filepath.Join(dataDir, script, removed))
			}
		}
	}
}

func readCSVHeader(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	header, err := csv.NewReader(f).Read()
	if err != nil {
		t.Fatalf("read header %s: %v", path, err)
	}
	return header
}
