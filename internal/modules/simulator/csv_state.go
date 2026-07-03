package simulator

import (
	"context"
	"fmt"
	"log"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

func reconcileMatchToCSVRow(
	ctx context.Context,
	svc MatchService,
	matchID string,
	row BallRow,
	match *matches.Match,
) (*matches.Match, bool, error) {
	if match == nil {
		return nil, false, nil
	}
	if match.CurrentScore == row.ScoreAfter &&
		match.WicketsLost == row.WicketsAfter &&
		match.BallsLeft == row.BallsLeftAfter {
		return match, false, nil
	}

	targetScore := match.TargetScore
	corrected, err := svc.UpdateMatchScore(ctx, matchID, matches.UpdateScoreRequest{
		Innings:      match.Innings,
		CurrentScore: row.ScoreAfter,
		WicketsLost:  row.WicketsAfter,
		BallsLeft:    row.BallsLeftAfter,
		TargetScore:  &targetScore,
		Status:       match.Status,
	})
	if err != nil {
		return match, false, err
	}
	if corrected == nil {
		return match, false, fmt.Errorf("csv score correction returned nil match")
	}
	return corrected, true, nil
}

func reconcileMatchToLastCSVRow(
	ctx context.Context,
	svc MatchService,
	matchID string,
	ds *CSVDataset,
	match *matches.Match,
	innings int,
	cursor int,
) (*matches.Match, bool, error) {
	row, ok := lastAppliedCSVRow(ds, innings, cursor)
	if !ok {
		return match, false, nil
	}
	return reconcileMatchToCSVRow(ctx, svc, matchID, row, match)
}

func lastAppliedCSVRow(ds *CSVDataset, innings int, cursor int) (BallRow, bool) {
	if ds == nil || cursor <= 0 {
		return BallRow{}, false
	}
	events := ds.Events[innings]
	if len(events) == 0 {
		return BallRow{}, false
	}
	if cursor > len(events) {
		cursor = len(events)
	}
	return events[cursor-1], true
}

func lastAppliedResumePosition(match *matches.Match, ds *CSVDataset, counts map[int]int) (int, int) {
	if match == nil || ds == nil {
		return 0, 0
	}
	innings := match.Innings
	if innings < 1 {
		innings = 1
	}
	if counts[innings] > 0 && len(ds.Events[innings]) > 0 {
		return innings, counts[innings]
	}
	if innings == 2 && counts[1] > 0 && len(ds.Events[1]) > 0 {
		return 1, counts[1]
	}
	if counts[2] > 0 && len(ds.Events[2]) > 0 {
		return 2, counts[2]
	}
	if counts[1] > 0 && len(ds.Events[1]) > 0 {
		return 1, counts[1]
	}
	return innings, 0
}

func logCSVStateCorrection(matchID string, innings int, row BallRow, before *matches.Match, after *matches.Match) {
	if before == nil || after == nil {
		return
	}
	log.Printf("simulator[%s]: aligned to CSV innings=%d seq=%d from %d/%d (%d balls left) to %d/%d (%d balls left)",
		matchID, innings, row.EventSeq,
		before.CurrentScore, before.WicketsLost, before.BallsLeft,
		after.CurrentScore, after.WicketsLost, after.BallsLeft)
}
