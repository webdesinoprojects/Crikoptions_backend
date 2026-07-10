package markets

import (
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestBuildOptionChainHistoryStartsAtInningsStart(t *testing.T) {
	start := time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)
	matchID := primitive.NewObjectID()
	marketID := primitive.NewObjectID()
	service := NewService(NewMemoryRepository())

	history, err := service.BuildOptionChainHistory(
		Market{
			ID:      marketID,
			MatchID: matchID.Hex(),
			QuantityLadder: []LadderEntry{
				{BuyerQty: 10, SellerQty: 12},
			},
		},
		matches.Match{
			ID:        matchID,
			Format:    "T20",
			Innings:   1,
			StartTime: start,
		},
		[]matches.BallEvent{
			{MatchID: matchID.Hex(), Innings: 1, LegalBall: true, Runs: 4, CreatedAt: start.Add(15 * time.Second)},
			{MatchID: matchID.Hex(), Innings: 1, LegalBall: true, Runs: 1, CreatedAt: start.Add(30 * time.Second)},
		},
	)
	if err != nil {
		t.Fatalf("BuildOptionChainHistory: %v", err)
	}
	if history.StartedAt != start.UnixMilli() {
		t.Fatalf("StartedAt = %d, want %d", history.StartedAt, start.UnixMilli())
	}

	pointsByTimestamp := make(map[int64]int)
	for _, point := range history.Points {
		pointsByTimestamp[point.Timestamp]++
	}
	if pointsByTimestamp[start.UnixMilli()] == 0 {
		t.Fatalf("missing opening option-chain snapshot at innings start")
	}
	if pointsByTimestamp[start.Add(30*time.Second).UnixMilli()] == 0 {
		t.Fatalf("missing option-chain snapshot for second delivery")
	}
	if got := pointsByTimestamp[start.UnixMilli()]; got != 25 {
		t.Fatalf("opening snapshot points = %d, want 25 strikes", got)
	}
}
