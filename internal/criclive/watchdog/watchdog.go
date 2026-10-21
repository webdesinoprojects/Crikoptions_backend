package watchdog

import (
	"context"
	"log"
	"time"
)

type Store interface {
	ExpireStaleFeeds(context.Context, time.Time, int64) (int, error)
}

// Reaper closes provider matches that the feed will never close for us: those
// whose feed died without a terminal phase, and those whose feed is alive but
// keeps reporting a terminal phase the status never picked up.
type Reaper interface {
	AbandonDeadLiveMatches(context.Context, time.Time, time.Duration, time.Duration) (int64, error)
	ClosePhaseTerminalMatches(context.Context, time.Time, time.Duration) (int64, error)
}

// RunReaper periodically abandons orphaned live matches. It runs far slower than
// the feed watchdog: the thresholds are measured in hours, so checking every few
// minutes is ample and keeps the write load negligible.
func RunReaper(ctx context.Context, reaper Reaper, interval, deadAfter, maxDuration, stuckPhaseAfter time.Duration) {
	if reaper == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		now := time.Now().UTC()
		if n, err := reaper.AbandonDeadLiveMatches(ctx, now, deadAfter, maxDuration); err != nil && ctx.Err() == nil {
			log.Printf("CricLive dead-match reaper: %v", err)
		} else if n > 0 {
			log.Printf("CricLive dead-match reaper: abandoned %d orphaned live matches", n)
		}
		if n, err := reaper.ClosePhaseTerminalMatches(ctx, now, stuckPhaseAfter); err != nil && ctx.Err() == nil {
			log.Printf("CricLive stuck-phase reaper: %v", err)
		} else if n > 0 {
			log.Printf("CricLive stuck-phase reaper: closed %d matches whose phase was already terminal", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func Run(ctx context.Context, store Store, interval time.Duration) {
	if store == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := store.ExpireStaleFeeds(ctx, time.Now().UTC(), 100); err != nil && ctx.Err() == nil {
			log.Printf("CricLive stale-feed watchdog: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
