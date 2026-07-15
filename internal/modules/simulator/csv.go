package simulator

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

// InningsConfig holds per-innings configuration from matches_config.csv.
type InningsConfig struct {
	MatchID           string
	Innings           int
	ReplayIntervalSec int
	StartStriker      string
	StartNonStriker   string
	StartBowler       string
	TargetScore       int
	TotalBalls        int // legal balls per innings (120 T20, 300 ODI); 0 → 120
}

// BallRow holds one delivery row from a ball_events CSV.
type BallRow struct {
	EventSeq       int
	Innings        int
	Runs           int
	IsWicket       bool
	Extra          string // "" | "wide" | "noball"
	NextBatterName string
	WicketType     string
	DelaySec       int
	ScoreAfter     int
	WicketsAfter   int
	BallsLeftAfter int
	Commentary     string
	EndInnings     bool
	EndMatch       bool
	ChangeBowler   string // new bowler name for next over, if non-empty
}

// CSVDataset is the fully loaded script for one match.
type CSVDataset struct {
	MatchID     string
	TotalBalls  int // legal balls per innings
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

	totalBalls := matches.BallsT20
	for _, cfg := range configs {
		if cfg.TotalBalls > totalBalls {
			totalBalls = cfg.TotalBalls
		}
	}
	if configs[0].TotalBalls > 0 {
		totalBalls = configs[0].TotalBalls
	}

	ds := &CSVDataset{
		MatchID:    configs[0].MatchID,
		TotalBalls: totalBalls,
		Events:     make(map[int][]BallRow),
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

	rows, err := loadBallEvents(dir, totalBalls)
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
			Innings:           intField(rec, idx, "innings"),
			ReplayIntervalSec: intField(rec, idx, "replay_interval_sec"),
			StartStriker:      field(rec, idx, "start_striker"),
			StartNonStriker:   field(rec, idx, "start_non_striker"),
			StartBowler:       field(rec, idx, "start_bowler"),
			TargetScore:       intField(rec, idx, "target_score"),
			TotalBalls:        intField(rec, idx, "total_balls"),
		})
	}
	return out, nil
}

// loadBallEvents loads the single full-match source-of-truth CSV for a script.
func loadBallEvents(dir string, totalBalls int) ([]BallRow, error) {
	return readBallCSV(filepath.Join(dir, "ball_events_full_match.csv"), totalBalls)
}

func readBallCSV(path string, totalBalls int) ([]BallRow, error) {
	if totalBalls <= 0 {
		totalBalls = matches.BallsT20
	}
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
	required := []string{
		"event_seq", "innings", "runs", "is_wicket", "extra",
		"next_batter_name", "wicket_type", "delay_sec",
		"score_after", "wickets_after", "commentary",
		"end_innings", "end_match", "change_bowler",
	}
	for _, col := range required {
		if _, ok := idx[col]; !ok {
			return nil, fmt.Errorf("missing required column %q in %s", col, path)
		}
	}

	var out []BallRow
	legalByInnings := map[int]int{}
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
		extra := normalizeCSVExtra(field(rec, idx, "extra"))
		if extra == "" {
			legalByInnings[innings]++
		}
		out = append(out, BallRow{
			EventSeq:       intField(rec, idx, "event_seq"),
			Innings:        innings,
			Runs:           intField(rec, idx, "runs"),
			IsWicket:       boolField(rec, idx, "is_wicket"),
			Extra:          extra,
			NextBatterName: field(rec, idx, "next_batter_name"),
			WicketType:     field(rec, idx, "wicket_type"),
			DelaySec:       delay,
			ScoreAfter:     intField(rec, idx, "score_after"),
			WicketsAfter:   intField(rec, idx, "wickets_after"),
			BallsLeftAfter: max(0, totalBalls-legalByInnings[innings]),
			Commentary:     field(rec, idx, "commentary"),
			EndInnings:     boolField(rec, idx, "end_innings"),
			EndMatch:       boolField(rec, idx, "end_match"),
			ChangeBowler:   field(rec, idx, "change_bowler"),
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

func normalizeCSVExtra(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "wide":
		return "wide"
	case "noball", "no_ball", "no-ball":
		return "noball"
	default:
		return ""
	}
}
