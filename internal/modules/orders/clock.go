package orders

import (
	"strings"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

func snapshotMatchClock(m *matches.Match, at time.Time) executions.MatchClock {
	if m == nil {
		return executions.MatchClock{}
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	format := strings.TrimSpace(m.Format)
	scheduled := m.ScheduledBalls
	if scheduled <= 0 {
		scheduled = matches.TotalBallsForFormat(format)
	}
	legal := 0
	if scheduled > 0 {
		legal = scheduled - m.BallsLeft
		if legal < 0 {
			legal = 0
		}
	}
	overs := strings.TrimSpace(m.OversText)
	if legal == 0 {
		if parsed := executions.ParseLegalBalls(overs); parsed > 0 {
			legal = parsed
		}
	}
	return executions.MatchClock{
		OversText:  overs,
		LegalBalls: legal,
		Innings:    m.Innings,
		Format:     format,
		MatchID:    m.ID.Hex(),
		At:         at.UTC(),
	}
}
