package simulator

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// InningsConfig holds per-innings configuration from matches_config.csv.
type InningsConfig struct {
	MatchID           string
	TeamA             string
	TeamB             string
	Format            string
	Innings           int
	ReplayIntervalSec int
	StartStriker      string
	StartNonStriker   string
	StartBowler       string
	BattingTeam       string
	BowlingTeam       string
	StatusOnStart     string
	TargetScore       int
	ScriptName        string
}

// BallRow holds one delivery row from a ball_events CSV.
type BallRow struct {
	MatchID        string
	EventSeq       int
	Innings        int
	Runs           int
	IsWicket       bool
	Extra          string // "" | "wide" | "noball"
	StrikerName    string
	NonStrikerName string
	BowlerName     string
	NextBatterName string
	WicketType     string
	DelaySec       int
	Over           int
	BallInOver     int
	IsLegalBall    bool
	RunsOffBat     int
	ExtraRuns      int
	IsBoundary     bool
	IsFour         bool
	IsSix          bool
	ScoreAfter     int
	WicketsAfter   int
	Commentary     string
	EndInnings     bool
	EndMatch       bool
	ChangeBowler   string // new bowler name for next over, if non-empty
	SwapStrike     bool
}

// CSVDataset is the fully loaded script for one match.
type CSVDataset struct {
	MatchID     string
	Innings1    InningsConfig
	Innings2    InningsConfig
	HasInnings2 bool
	Events      map[int][]BallRow // keyed by innings number (1 or 2)
}

// LoadDataset loads matches_config.csv and ball_events CSV(s) from dataDir/scriptName/.
func LoadDataset(dataDir, scriptName string) (*CSVDataset, error) {
	dir := filepath.Join(dataDir, scriptName)

	configs, err := loadMatchesConfig(filepath.Join(dir, "matches_config.csv"))
	if err != nil {
		return nil, fmt.Errorf("matches_config: %w", err)
	}
	if len(configs) == 0 {
		return nil, fmt.Errorf("matches_config.csv is empty")
	}

	ds := &CSVDataset{
		MatchID: configs[0].MatchID,
		Events:  make(map[int][]BallRow),
	}
	for _, cfg := range configs {
		switch cfg.Innings {
		case 1:
			ds.Innings1 = cfg
		case 2:
			ds.Innings2 = cfg
			ds.HasInnings2 = true
		}
	}

	rows, err := loadBallEvents(dir)
	if err != nil {
		return nil, fmt.Errorf("ball_events: %w", err)
	}
	for _, r := range rows {
		if r.Innings >= 1 {
			ds.Events[r.Innings] = append(ds.Events[r.Innings], r)
		}
	}
	return ds, nil
}

func loadMatchesConfig(path string) ([]InningsConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.TrimLeadingSpace = true
	header, err := r.Read()
	if err != nil {
		return nil, err
	}
	idx := colIndex(header)

	var out []InningsConfig
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, InningsConfig{
			MatchID:           field(rec, idx, "match_id"),
			TeamA:             field(rec, idx, "team_a"),
			TeamB:             field(rec, idx, "team_b"),
			Format:            field(rec, idx, "format"),
			Innings:           intField(rec, idx, "innings"),
			ReplayIntervalSec: intField(rec, idx, "replay_interval_sec"),
			StartStriker:      field(rec, idx, "start_striker"),
			StartNonStriker:   field(rec, idx, "start_non_striker"),
			StartBowler:       field(rec, idx, "start_bowler"),
			BattingTeam:       field(rec, idx, "batting_team"),
			BowlingTeam:       field(rec, idx, "bowling_team"),
			StatusOnStart:     field(rec, idx, "status_on_start"),
			TargetScore:       intField(rec, idx, "target_score"),
			ScriptName:        field(rec, idx, "script_name"),
		})
	}
	return out, nil
}

// loadBallEvents tries candidate filenames in preference order, supplementing
// per-innings files when a combined file only covers one innings.
func loadBallEvents(dir string) ([]BallRow, error) {
	// Prefer a single file covering both innings.
	for _, name := range []string{"ball_events_full_match.csv", "ball_events.csv"} {
		rows, err := readBallCSV(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		has1, has2 := false, false
		for _, r := range rows {
			if r.Innings == 1 {
				has1 = true
			}
			if r.Innings == 2 {
				has2 = true
			}
		}
		if has1 && has2 {
			return rows, nil
		}
		// Partial file — fill in the missing innings from per-innings files.
		var combined []BallRow
		combined = append(combined, rows...)
		if !has1 {
			if r1, e := readBallCSV(filepath.Join(dir, "ball_events_innings1.csv")); e == nil {
				combined = append(combined, r1...)
			}
		}
		if !has2 {
			if r2, e := readBallCSV(filepath.Join(dir, "ball_events_innings2.csv")); e == nil {
				combined = append(combined, r2...)
			}
		}
		if len(combined) > 0 {
			return combined, nil
		}
	}

	// Fall back: combine both per-innings files.
	var all []BallRow
	r1, e1 := readBallCSV(filepath.Join(dir, "ball_events_innings1.csv"))
	r2, e2 := readBallCSV(filepath.Join(dir, "ball_events_innings2.csv"))
	if e1 != nil && e2 != nil {
		return nil, fmt.Errorf("no ball events CSV found in %s", dir)
	}
	all = append(all, r1...)
	all = append(all, r2...)
	return all, nil
}

func readBallCSV(path string) ([]BallRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.TrimLeadingSpace = true
	r.LazyQuotes = true
	header, err := r.Read()
	if err != nil {
		return nil, err
	}
	idx := colIndex(header)

	var out []BallRow
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue // skip malformed lines
		}
		innings := intField(rec, idx, "innings")
		if innings == 0 {
			continue // skip invalid rows
		}
		delay := intField(rec, idx, "delay_sec")
		if delay <= 0 {
			delay = 15
		}
		out = append(out, BallRow{
			MatchID:        field(rec, idx, "match_id"),
			EventSeq:       intField(rec, idx, "event_seq"),
			Innings:        innings,
			Runs:           intField(rec, idx, "runs"),
			IsWicket:       boolField(rec, idx, "is_wicket"),
			Extra:          strings.ToLower(strings.TrimSpace(field(rec, idx, "extra"))),
			StrikerName:    field(rec, idx, "striker_name"),
			NonStrikerName: field(rec, idx, "non_striker_name"),
			BowlerName:     field(rec, idx, "bowler_name"),
			NextBatterName: field(rec, idx, "next_batter_name"),
			WicketType:     field(rec, idx, "wicket_type"),
			DelaySec:       delay,
			Over:           intField(rec, idx, "over"),
			BallInOver:     intField(rec, idx, "ball_in_over"),
			IsLegalBall:    boolField(rec, idx, "is_legal_ball"),
			RunsOffBat:     intField(rec, idx, "runs_off_bat"),
			ExtraRuns:      intField(rec, idx, "extra_runs"),
			IsBoundary:     boolField(rec, idx, "is_boundary"),
			IsFour:         boolField(rec, idx, "is_four"),
			IsSix:          boolField(rec, idx, "is_six"),
			ScoreAfter:     intField(rec, idx, "score_after"),
			WicketsAfter:   intField(rec, idx, "wickets_after"),
			Commentary:     field(rec, idx, "commentary"),
			EndInnings:     boolField(rec, idx, "end_innings"),
			EndMatch:       boolField(rec, idx, "end_match"),
			ChangeBowler:   field(rec, idx, "change_bowler"),
			SwapStrike:     boolField(rec, idx, "swap_strike"),
		})
	}
	return out, nil
}

// --- CSV helpers ---

func colIndex(header []string) map[string]int {
	m := make(map[string]int, len(header))
	for i, h := range header {
		m[strings.ToLower(strings.TrimSpace(h))] = i
	}
	return m
}

func field(rec []string, idx map[string]int, col string) string {
	i, ok := idx[col]
	if !ok || i >= len(rec) {
		return ""
	}
	return strings.TrimSpace(rec[i])
}

func intField(rec []string, idx map[string]int, col string) int {
	n, _ := strconv.Atoi(field(rec, idx, col))
	return n
}

func boolField(rec []string, idx map[string]int, col string) bool {
	v := strings.ToLower(strings.TrimSpace(field(rec, idx, col)))
	return v == "true" || v == "1" || v == "yes"
}
