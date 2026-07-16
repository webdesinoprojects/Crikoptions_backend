package watchdog

import (
	"context"
	"log"
	"time"
)

type Store interface {
	ExpireStaleFeeds(context.Context, time.Time, int64) (int, error)
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
			log.Printf("Sportmonks stale-feed watchdog: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
