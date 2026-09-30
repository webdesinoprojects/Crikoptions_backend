// Package feed picks the provider adapter the feed pipeline reads, so every
// entrypoint that runs the worker makes the same choice, and owns the one-off
// work a switch between feeds needs.
package feed

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/cricketline"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/store"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/worker"
)

// The active-feed lease is owned by a feed, not a process: every process of
// the feed renews it, and a process configured for the other feed may not
// retire anything while it is held. That stops a stale instance from the
// previous feed (a rolling deploy, a forgotten worker) and the new one from
// parking each other's fixtures back and forth.
const (
	activeFeedLease    = "active-feed"
	activeFeedLeaseTTL = 15 * time.Minute
	activeFeedRenew    = 5 * time.Minute
)

// NewProvider builds the adapter for cfg.Feed and reports the fixture id range
// it owns.
func NewProvider(cfg client.Config, httpClient *http.Client) (worker.Provider, store.FixtureIDRange, error) {
	switch cfg.Feed {
	case client.FeedCricketLine:
		provider, err := cricketline.New(cfg, httpClient)
		if err != nil {
			return nil, store.FixtureIDRange{}, err
		}
		return provider, store.FixtureIDRange{Min: cricketline.IDMin, Max: cricketline.IDMax}, nil
	case client.FeedCricLive:
		provider, err := client.New(cfg, httpClient)
		if err != nil {
			return nil, store.FixtureIDRange{}, err
		}
		return provider, store.FixtureIDRange{Min: 1, Max: cricketline.IDNamespace}, nil
	}
	return nil, store.FixtureIDRange{}, fmt.Errorf("unknown feed %q", cfg.Feed)
}

// Prepare runs before the worker starts. In live mode it takes the
// active-feed lease and, while holding it, retires what another feed left
// behind — now and every few minutes, so fixtures a departing instance of the
// old feed republished during a deploy are cleaned up too. A shadow run never
// touches shared data. An adapter with a fixture directory is seeded from the
// fixtures already stored, which costs no provider request.
func Prepare(ctx context.Context, cfg client.Config, provider worker.Provider, own store.FixtureIDRange, feedStore *store.Store, owner string, logger *log.Logger) {
	if logger == nil {
		logger = log.Default()
	}
	if seeder, ok := provider.(interface{ Seed([]client.Fixture) }); ok {
		now := time.Now().UTC()
		targets, err := feedStore.FixtureTargetsBetween(ctx, own, now.Add(-24*time.Hour), now.Add(15*24*time.Hour))
		if err != nil {
			logger.Printf("feed directory seed: %v", err)
		} else {
			fixtures := make([]client.Fixture, 0, len(targets))
			for _, target := range targets {
				fixtures = append(fixtures, fixtureFromTarget(target))
			}
			seeder.Seed(fixtures)
		}
	}
	if cfg.Mode != client.ModeLive {
		return
	}
	retire := func() {
		now := time.Now().UTC()
		held, err := feedStore.ClaimSchedule(ctx, activeFeedLease, string(cfg.Feed), now, activeFeedLeaseTTL)
		switch {
		case err != nil:
			logger.Printf("feed switch: active-feed lease: %v", err)
			return
		case !held:
			logger.Printf("feed switch: another feed holds the active-feed lease; %s will not retire its fixtures (instance %s)", cfg.Feed, owner)
			return
		}
		parked, hidden, err := feedStore.RetireForeignFixtures(ctx, own, now)
		switch {
		case err != nil:
			logger.Printf("feed switch: retire foreign fixtures: %v", err)
		case parked > 0 || hidden > 0:
			logger.Printf("feed switch: parked %d fixture target(s) and hid %d upcoming match(es) from the previous feed", parked, hidden)
		}
	}
	retire()
	go func() {
		ticker := time.NewTicker(activeFeedRenew)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				retire()
			}
		}
	}()
}

func fixtureFromTarget(target store.FixtureTarget) client.Fixture {
	return client.Fixture{
		ID: target.ID, SeriesID: target.LeagueID, Format: target.Format, StartingAt: target.StartTime,
		LocalTeamID: target.LocalTeamID, VisitorTeamID: target.VisitorTeamID,
		LocalTeamShort: target.LocalTeamShort, VisitorTeamShort: target.VisitorTeamShort,
	}
}
