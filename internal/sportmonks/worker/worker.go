package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/reconcile"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/store"
)

var ErrQuotaReserved = errors.New("Sportmonks quota reserve reached")

type Provider interface {
	Leagues(context.Context, client.PageOptions) (client.Envelope[[]client.League], error)
	Scores(context.Context) (client.Envelope[[]client.Score], error)
	LiveScores(context.Context, client.LiveScoresOptions) (client.Envelope[[]client.Fixture], error)
	FixtureByID(context.Context, int64, client.FixtureOptions) (client.Envelope[client.Fixture], error)
	Fixtures(context.Context, client.FixturesOptions) (client.Envelope[[]client.Fixture], error)
}

type Storage interface {
	SyncLeagues(context.Context, []client.League, time.Time, bool) error
	EnabledLeagueIDs(context.Context) ([]int64, error)
	SaveCatalog(context.Context, []client.Score, time.Time) (reconcile.Catalog, error)
	LoadCatalog(context.Context) (reconcile.Catalog, error)
	UpsertFixtureTargets(context.Context, []client.Fixture, time.Time, bool, bool) error
	PublishFixtureMatches(context.Context, []client.Fixture, time.Time, bool) error
	ConsumeRequestQuota(context.Context, string, time.Time, int, int) (bool, error)
	ClaimSchedule(context.Context, string, string, time.Time, time.Duration) (bool, error)
	DueTargets(context.Context, time.Time, int64) ([]store.FixtureTarget, error)
	PollableTargetCount(context.Context, time.Time) (int64, error)
	OpenTargetCount(context.Context, time.Time, string) (int64, error)
	ClaimTarget(context.Context, int64, string, time.Time, time.Duration) (string, bool, error)
	CompleteTargetPoll(context.Context, int64, string, string, string, string, string, time.Time, time.Time) error
	FailTargetPoll(context.Context, int64, string, string, error, time.Time, time.Time) error
	DeferTarget(context.Context, int64, time.Time, string) error
	SavePayload(context.Context, int64, string, []byte, time.Time, time.Duration, bool, error) error
	ApplyProjection(context.Context, reconcile.Projection, []byte, time.Time, store.ApplyOptions) (store.ApplyResult, error)
	ApplyProviderTerminalClosure(context.Context, int64, string, time.Time, store.ApplyOptions) (bool, error)
	CompleteStuckTerminalMatches(context.Context, time.Time) (int64, error)
	MarkFeedUnavailable(context.Context, int64, string, string, time.Time, *time.Time) error
	MarkFeedFrozen(context.Context, int64, time.Time, time.Time) error
	ResetFinalizationHolds(context.Context, int64, string, string, time.Time) error
	RescheduleStaleTargets(context.Context, time.Time) (int64, error)
}

type Logger interface {
	Printf(string, ...any)
}

type Worker struct {
	cfg      client.Config
	provider Provider
	store    Storage
	owner    string
	logger   Logger
	quota    *quotaWindow

	catalogMu           sync.RWMutex
	catalog             reconcile.Catalog
	fixtureSyncMu       sync.Mutex
	fixturesMu          sync.RWMutex
	fixtureLeagueKey    string
	fixtureLeaguesKnown bool
	randomMu            sync.Mutex
	random              *rand.Rand
	wg                  sync.WaitGroup
	semaphore           chan struct{}
	now                 func() time.Time
}

func New(cfg client.Config, provider Provider, storage Storage, owner string, logger Logger) (*Worker, error) {
	if cfg.Mode == client.ModeOff {
		return nil, errors.New("cannot construct Sportmonks worker in off mode")
	}
	if provider == nil || storage == nil {
		return nil, errors.New("Sportmonks worker requires provider and storage")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, errors.New("Sportmonks worker owner is required")
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Worker{
		cfg: cfg, provider: provider, store: storage, owner: owner, logger: logger,
		quota:     newQuotaWindow(cfg.HourlyRequestLimit, cfg.QuotaReservePercent),
		random:    rand.New(rand.NewSource(time.Now().UnixNano())),
		semaphore: make(chan struct{}, cfg.MaxConcurrency), now: time.Now,
	}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	if err := w.bootstrap(ctx); err != nil {
		return err
	}
	w.startPeriodic(ctx, "metadata", w.cfg.MetadataSyncInterval, w.syncMetadata)
	w.startPeriodic(ctx, "fixture catalog", w.cfg.FixtureSyncInterval, w.syncFixtures)
	w.startPeriodic(ctx, "live discovery", w.cfg.DiscoveryInterval, w.discoverLive)
	dispatchTicker := time.NewTicker(time.Second)
	defer dispatchTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.wg.Wait()
			return nil
		case <-dispatchTicker.C:
			if err := w.dispatch(ctx); err != nil {
				w.logger.Printf("sportmonks dispatch: %v", err)
			}
		}
	}
}

func (w *Worker) startPeriodic(ctx context.Context, name string, interval time.Duration, run func(context.Context) error) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := run(ctx); err != nil && ctx.Err() == nil {
					w.logger.Printf("sportmonks %s sync: %v", name, err)
				}
			}
		}
	}()
}

func (w *Worker) bootstrap(ctx context.Context) error {
	if err := w.syncLeagues(ctx); err != nil {
		w.logger.Printf("sportmonks league sync failed; retaining existing allowlist: %v", err)
	}
	if err := w.syncCatalog(ctx); err != nil {
		catalog, loadErr := w.store.LoadCatalog(ctx)
		if loadErr != nil {
			return fmt.Errorf("Sportmonks score catalog unavailable: sync=%v cached=%v", err, loadErr)
		}
		w.setCatalog(catalog)
		w.logger.Printf("sportmonks using cached score catalog after sync failure: %v", err)
	}
	if err := w.syncFixtures(ctx); err != nil {
		w.logger.Printf("sportmonks initial fixture sync: %v", err)
	}
	if err := w.discoverLive(ctx); err != nil {
		w.logger.Printf("sportmonks initial live discovery: %v", err)
	}
	if count, err := w.store.RescheduleStaleTargets(ctx, w.now().UTC()); err != nil {
		w.logger.Printf("sportmonks reschedule stale targets: %v", err)
	} else if count > 0 {
		w.logger.Printf("sportmonks rescheduled %d fixture targets for immediate poll", count)
	}
	return nil
}

func (w *Worker) syncMetadata(ctx context.Context) error {
	if err := w.syncLeagues(ctx); err != nil {
		return err
	}
	return w.syncCatalog(ctx)
}

func (w *Worker) syncLeagues(ctx context.Context) error {
	claimed, err := w.store.ClaimSchedule(ctx, "leagues", w.owner, w.now().UTC(), 5*time.Minute)
	if err != nil || !claimed {
		return err
	}
	var leagues []client.League
	for page := 1; ; page++ {
		if !w.takeProviderQuota(ctx, "leagues") {
			return ErrQuotaReserved
		}
		envelope, err := w.provider.Leagues(ctx, client.PageOptions{Page: page, Sort: "id"})
		w.quota.observe("leagues", w.now().UTC(), envelope.RateLimit)
		if err != nil {
			return err
		}
		leagues = append(leagues, envelope.Data...)
		if envelope.Meta.Pagination == nil {
			break
		}
		if _, more := envelope.Meta.Pagination.NextPage(); !more {
			break
		}
	}
	return w.store.SyncLeagues(ctx, leagues, w.now().UTC(), w.cfg.Mode == client.ModeLive)
}

func (w *Worker) syncCatalog(ctx context.Context) error {
	claimed, err := w.store.ClaimSchedule(ctx, "scores", w.owner, w.now().UTC(), 5*time.Minute)
	if err != nil {
		return err
	}
	if !claimed {
		catalog, loadErr := w.store.LoadCatalog(ctx)
		if loadErr == nil {
			w.setCatalog(catalog)
		}
		return loadErr
	}
	if !w.takeProviderQuota(ctx, "scores") {
		return ErrQuotaReserved
	}
	envelope, err := w.provider.Scores(ctx)
	w.quota.observe("scores", w.now().UTC(), envelope.RateLimit)
	if err != nil {
		return err
	}
	catalog, err := w.store.SaveCatalog(ctx, envelope.Data, w.now().UTC())
	if err != nil {
		return err
	}
	w.setCatalog(catalog)
	return nil
}

func (w *Worker) syncFixtures(ctx context.Context) error {
	if !w.fixtureSyncMu.TryLock() {
		return nil
	}
	defer w.fixtureSyncMu.Unlock()
	claimed, err := w.store.ClaimSchedule(ctx, "fixtures-catalog", w.owner, w.now().UTC(), 15*time.Minute)
	if err != nil || !claimed {
		return err
	}
	leagueIDs, err := w.store.EnabledLeagueIDs(ctx)
	if err != nil {
		return err
	}
	now := w.now().UTC()
	for _, leagueID := range leagueIDs {
		for page := 1; ; page++ {
			if !w.takeProviderQuota(ctx, "fixtures") {
				return ErrQuotaReserved
			}
			envelope, err := w.provider.Fixtures(ctx, client.FixturesOptions{
				From: now.Add(-2 * 24 * time.Hour), To: now.Add(14 * 24 * time.Hour),
				LeagueID: leagueID, Page: page, Sort: "starting_at",
				Includes: []string{"localteam", "visitorteam"},
			})
			w.quota.observe("fixtures", w.now().UTC(), envelope.RateLimit)
			if err != nil {
				return err
			}
			if err := w.store.UpsertFixtureTargets(ctx, envelope.Data, now, w.cfg.Mode == client.ModeLive, w.cfg.AllowMidMatchLiveAdmission); err != nil {
				return err
			}
			if w.cfg.Mode == client.ModeLive {
				if err := w.store.PublishFixtureMatches(ctx, envelope.Data, now, w.cfg.AllowMidMatchLiveAdmission); err != nil {
					return err
				}
			}
			if envelope.Meta.Pagination == nil {
				break
			}
			if _, more := envelope.Meta.Pagination.NextPage(); !more {
				break
			}
		}
	}
	w.setFixtureLeagues(leagueIDs)
	return nil
}

func (w *Worker) discoverLive(ctx context.Context) error {
	leaseTTL := w.cfg.DiscoveryInterval - time.Second
	if leaseTTL < 5*time.Second {
		leaseTTL = 5 * time.Second
	}
	claimed, err := w.store.ClaimSchedule(ctx, "live-discovery", w.owner, w.now().UTC(), leaseTTL)
	if err != nil || !claimed {
		return err
	}
	leagueIDs, err := w.store.EnabledLeagueIDs(ctx)
	if err != nil {
		return err
	}
	if w.fixtureLeaguesChanged(leagueIDs) {
		if err := w.syncFixtures(ctx); err != nil {
			return err
		}
	}
	enabled := make(map[int64]struct{}, len(leagueIDs))
	for _, id := range leagueIDs {
		enabled[id] = struct{}{}
	}
	for page := 1; ; page++ {
		if !w.takeProviderQuota(ctx, "livescores") {
			return ErrQuotaReserved
		}
		envelope, err := w.provider.LiveScores(ctx, client.LiveScoresOptions{
			Page: page, Includes: []string{"localteam", "visitorteam"},
		})
		w.quota.observe("livescores", w.now().UTC(), envelope.RateLimit)
		if err != nil {
			return err
		}
		eligible := make([]client.Fixture, 0, len(envelope.Data))
		for _, fixture := range envelope.Data {
			if _, ok := enabled[fixture.LeagueID]; ok {
				eligible = append(eligible, fixture)
			}
		}
		if err := w.store.UpsertFixtureTargets(ctx, eligible, w.now().UTC(), w.cfg.Mode == client.ModeLive, w.cfg.AllowMidMatchLiveAdmission); err != nil {
			return err
		}
		if w.cfg.Mode == client.ModeLive {
			if err := w.store.PublishFixtureMatches(ctx, eligible, w.now().UTC(), w.cfg.AllowMidMatchLiveAdmission); err != nil {
				return err
			}
		}
		if envelope.Meta.Pagination == nil {
			break
		}
		if _, more := envelope.Meta.Pagination.NextPage(); !more {
			break
		}
	}
	return nil
}

func (w *Worker) dispatch(ctx context.Context) error {
	now := w.now().UTC()
	claimed, err := w.store.ClaimSchedule(ctx, "fixture-dispatch", w.owner, now, 2*time.Second)
	if err != nil || !claimed {
		return err
	}
	targets, err := w.store.DueTargets(ctx, now, 100)
	if err != nil || len(targets) == 0 {
		return err
	}
	activeCount, err := w.store.PollableTargetCount(ctx, now)
	if err != nil {
		return err
	}
	openCount, err := w.store.OpenTargetCount(ctx, now, string(w.cfg.Mode))
	if err != nil {
		return err
	}
	newBudget := newFixtureBudget(int(openCount))
	newAdmitted := 0
	pollInterval := adaptivePollInterval(int(activeCount), w.cfg)
	sort.SliceStable(targets, func(i, j int) bool {
		iOpen := targetOpenInMode(targets[i], w.cfg.Mode)
		jOpen := targetOpenInMode(targets[j], w.cfg.Mode)
		if iOpen != jOpen {
			return iOpen
		}
		return targets[i].NextPollAt.Before(targets[j].NextPollAt)
	})
	for index := range targets {
		target := targets[index]
		alreadyOpen := targetOpenInMode(target, w.cfg.Mode)
		if !alreadyOpen && newAdmitted >= newBudget {
			_ = w.store.DeferTarget(ctx, target.ID, now.Add(w.cfg.MaxPollInterval), "quota_limited")
			if w.cfg.Mode == client.ModeLive {
				_ = w.store.MarkFeedUnavailable(ctx, target.ID, matches.FeedStateQuotaLimited, "quota_limited", now, nil)
			}
			continue
		}
		if !alreadyOpen {
			newAdmitted++
		}
		select {
		case w.semaphore <- struct{}{}:
		case <-ctx.Done():
			return nil
		default:
			return nil
		}
		token, claimed, err := w.store.ClaimTarget(ctx, target.ID, w.owner, now, w.cfg.LeaseTTL)
		if err != nil {
			<-w.semaphore
			return err
		}
		if !claimed {
			<-w.semaphore
			continue
		}
		w.wg.Add(1)
		go func(target store.FixtureTarget, token string) {
			defer w.wg.Done()
			defer func() { <-w.semaphore }()
			if ctx.Err() != nil {
				_ = w.store.FailTargetPoll(context.Background(), target.ID, w.owner, token, ctx.Err(), w.now().UTC(), w.now().UTC().Add(time.Second))
				return
			}
			w.pollTarget(ctx, target, token, pollInterval)
		}(target, token)
	}
	return nil
}

func (w *Worker) pollTarget(ctx context.Context, target store.FixtureTarget, token string, activeInterval time.Duration) {
	now := w.now().UTC()
	if !w.takeProviderQuota(ctx, "fixtures") {
		if w.cfg.Mode == client.ModeLive {
			if err := w.store.ResetFinalizationHolds(ctx, target.ID, w.owner, token, now); err != nil {
				if errors.Is(err, store.ErrFixtureLeaseLost) {
					return
				}
				w.logger.Printf("sportmonks fixture %d reset finalization hold: %v", target.ID, err)
			}
			_ = w.store.MarkFeedUnavailable(ctx, target.ID, matches.FeedStateQuotaLimited, "quota_limited", now, nil)
		}
		_ = w.store.FailTargetPoll(ctx, target.ID, w.owner, token, ErrQuotaReserved, now, now.Add(w.cfg.MaxPollInterval))
		return
	}
	envelope, err := w.provider.FixtureByID(ctx, target.ID, client.FixtureOptions{Includes: []string{
		"balls", "runs", "scoreboards", "batting", "bowling", "batting.batsman", "bowling.bowler",
	}})
	w.quota.observe("fixtures", w.now().UTC(), envelope.RateLimit)
	if err != nil {
		w.handlePollFailure(ctx, target, token, intervalForProviderStatus(target.ProviderStatus, activeInterval, w.cfg), err)
		return
	}
	receivedAt := w.now().UTC()
	raw := envelope.Data.Raw
	if len(raw) == 0 {
		raw, err = json.Marshal(envelope.Data)
		if err != nil {
			w.handlePollFailure(ctx, target, token, intervalForProviderStatus(target.ProviderStatus, activeInterval, w.cfg), err)
			return
		}
	}
	projection, err := reconcile.ReduceFixtureJSON(raw, w.getCatalog())
	if err != nil {
		_ = w.store.SavePayload(ctx, target.ID, string(w.cfg.Mode), raw, receivedAt, w.cfg.RawPayloadTTL, false, err)
		if w.cfg.Mode == client.ModeLive {
			if providerStatus, ok := reconcile.FixtureProviderStatus(raw); ok && reconcile.IsTerminalProviderStatus(providerStatus) {
				closed, closeErr := w.store.ApplyProviderTerminalClosure(ctx, target.ID, providerStatus, receivedAt, store.ApplyOptions{
					Mode: string(w.cfg.Mode), LeaseOwner: w.owner, LeaseToken: token,
				})
				if closeErr != nil {
					w.logger.Printf("sportmonks fixture %d terminal closure: %v", target.ID, closeErr)
				} else if closed {
					w.logger.Printf("sportmonks fixture %d: closed after reduce failure provider=%s", target.ID, providerStatus)
					next := receivedAt.Add(intervalForProviderStatus(providerStatus, activeInterval, w.cfg))
					_ = w.store.CompleteTargetPoll(ctx, target.ID, w.owner, token, string(w.cfg.Mode), "", providerStatus, receivedAt, next)
					return
				}
			}
			if resetErr := w.store.ResetFinalizationHolds(ctx, target.ID, w.owner, token, receivedAt); resetErr != nil {
				if errors.Is(resetErr, store.ErrFixtureLeaseLost) {
					return
				}
				w.logger.Printf("sportmonks fixture %d reset finalization hold: %v", target.ID, resetErr)
			}
			state, blocker := matches.FeedStateReconciling, "reconciling"
			if errors.Is(err, reconcile.ErrUnsupportedFormat) {
				state, blocker = matches.FeedStateUnsupported, "unsupported"
			}
			_ = w.store.MarkFeedUnavailable(ctx, target.ID, state, blocker, receivedAt, nil)
		}
		_ = w.store.FailTargetPoll(ctx, target.ID, w.owner, token, err, receivedAt, receivedAt.Add(w.failureBackoff(target, err)))
		return
	}
	scheduledInterval := intervalForProjection(projection, store.ApplyResult{}, activeInterval, w.cfg)
	result, err := w.store.ApplyProjection(ctx, projection, raw, receivedAt, store.ApplyOptions{
		Mode: string(w.cfg.Mode), LeaseOwner: w.owner, LeaseToken: token,
		AllowCorrections:        w.cfg.AllowLiveCorrections,
		AllowMidMatchAdmission:  w.cfg.AllowMidMatchLiveAdmission,
		InningsFinalizationHold: w.cfg.InningsFinalizationHold,
		MatchFinalizationHold:   w.cfg.MatchFinalizationHold, RawPayloadTTL: w.cfg.RawPayloadTTL,
		FeedValidity: feedValidityForInterval(w.cfg, scheduledInterval),
	})
	if err != nil {
		w.handlePollFailure(ctx, target, token, scheduledInterval, err)
		return
	}
	if result.Applied {
		w.logger.Printf(
			"sportmonks fixture %d: applied feed=%s stateVersion=%d reconciling=%t",
			target.ID, result.FeedState, result.StateVersion, result.Reconciling,
		)
	}
	// Do NOT MarkFeedFrozen after a successful poll. Quiet scoreboards (no runs
	// for 90s) are normal in cricket; treating them as feed_stale blocked trading
	// while polls were healthy. Real outages are handled by ExpireStaleFeeds /
	// handlePollFailure when LastSuccessfulPollAt goes stale.
	nextInterval := intervalForProjection(projection, result, activeInterval, w.cfg)
	next := receivedAt.Add(w.jitter(nextInterval))
	next = clampPreMatchPoll(projection, next)
	if err := w.store.CompleteTargetPoll(ctx, target.ID, w.owner, token, string(w.cfg.Mode), projection.SnapshotHash, projection.ProviderStatus, receivedAt, next); err != nil {
		w.logger.Printf("sportmonks fixture %d complete poll: %v", target.ID, err)
	}
}

func targetOpenInMode(target store.FixtureTarget, mode client.Mode) bool {
	return target.LastSuccessAt != nil && target.LastSuccessMode == string(mode)
}

func clampPreMatchPoll(projection reconcile.Projection, candidate time.Time) time.Time {
	if projection.Status != matches.StatusUpcoming || projection.StartTime.IsZero() {
		return candidate
	}
	resumeAt := projection.StartTime.Add(-30 * time.Minute)
	if resumeAt.After(candidate) {
		return resumeAt
	}
	return candidate
}

func (w *Worker) takeProviderQuota(ctx context.Context, endpoint string) bool {
	now := w.now().UTC()
	if !w.quota.take(endpoint, now) {
		return false
	}
	allowed, err := w.store.ConsumeRequestQuota(
		ctx, endpoint, now, w.cfg.HourlyRequestLimit, w.cfg.QuotaReservePercent,
	)
	if err != nil {
		w.logger.Printf("sportmonks quota %s: %v", endpoint, err)
		return false
	}
	return allowed
}

func (w *Worker) handlePollFailure(ctx context.Context, target store.FixtureTarget, token string, scheduled time.Duration, cause error) {
	now := w.now().UTC()
	next := now.Add(w.failureBackoff(target, cause))
	if errors.Is(cause, store.ErrFixtureLeaseLost) {
		return
	}
	if w.cfg.Mode == client.ModeLive {
		if err := w.store.ResetFinalizationHolds(ctx, target.ID, w.owner, token, now); err != nil {
			w.logger.Printf("sportmonks fixture %d reset finalization hold: %v", target.ID, err)
			if errors.Is(err, store.ErrFixtureLeaseLost) {
				return
			}
		}
		staleAfter := feedValidityForInterval(w.cfg, scheduled)
		cutoff := now.Add(-staleAfter)
		_ = w.store.MarkFeedUnavailable(ctx, target.ID, matches.FeedStateStale, "feed_stale", now, &cutoff)
	}
	_ = w.store.FailTargetPoll(ctx, target.ID, w.owner, token, cause, now, next)
}

// feedValidityForInterval is how long a successful poll keeps the feed fresh.
// Sized to tolerate several missed polls plus one slow HTTP round-trip.
func feedValidityForInterval(cfg client.Config, scheduled time.Duration) time.Duration {
	if scheduled <= 0 {
		scheduled = cfg.MaxPollInterval
	}
	httpBudget := cfg.HTTPTimeout
	if httpBudget <= 0 {
		httpBudget = 15 * time.Second
	}
	return maxDuration(cfg.StaleMinimum, 4*scheduled+httpBudget+10*time.Second)
}

func intervalForProviderStatus(status string, active time.Duration, cfg client.Config) time.Duration {
	lower := strings.ToLower(strings.TrimSpace(status))
	switch {
	case strings.Contains(lower, "finished"):
		return cfg.FinalizingInterval
	case strings.Contains(lower, "break"), strings.Contains(lower, "lunch"), strings.Contains(lower, "tea"),
		strings.Contains(lower, "dinner"), strings.Contains(lower, "int."):
		return cfg.BreakInterval
	case strings.Contains(lower, "innings"):
		return active
	default:
		return cfg.PreMatchInterval
	}
}

func (w *Worker) failureBackoff(target store.FixtureTarget, cause error) time.Duration {
	var limited *client.RateLimitError
	if errors.As(cause, &limited) && limited.RetryAfter > 0 {
		return minDuration(limited.RetryAfter, 10*time.Minute)
	}
	shift := min(target.ConsecutiveFailures, 5)
	return minDuration(time.Duration(1<<shift)*5*time.Second, 2*time.Minute)
}

func adaptivePollInterval(active int, cfg client.Config) time.Duration {
	// Live trading always prefers the minimum interval when few fixtures are active.
	fast := cfg.FastPollingEnabled || cfg.Mode == client.ModeLive
	if !fast {
		return cfg.MaxPollInterval
	}
	switch {
	case active <= 2:
		return cfg.MinPollInterval
	case active <= 4:
		return maxDuration(cfg.MinPollInterval, 5*time.Second)
	default:
		return cfg.MaxPollInterval
	}
}

func newFixtureBudget(open int) int {
	if open >= 6 {
		return 0
	}
	if open < 0 {
		open = 0
	}
	return 6 - open
}

func intervalForProjection(projection reconcile.Projection, result store.ApplyResult, active time.Duration, cfg client.Config) time.Duration {
	if result.FeedState == matches.FeedStateFinalizing {
		return cfg.FinalizingInterval
	}
	switch projection.Status {
	case matches.StatusLive:
		return active
	case matches.StatusInningsBreak:
		for _, innings := range projection.Innings {
			if innings.Number == projection.CurrentInnings && innings.Complete {
				return cfg.FinalizingInterval
			}
		}
		return cfg.BreakInterval
	case matches.StatusCompleted:
		if result.FeedState == matches.FeedStateTerminal {
			return 24 * time.Hour
		}
		return cfg.FinalizingInterval
	case matches.StatusAbandoned:
		if result.FeedState == matches.FeedStateFinalizing {
			return cfg.FinalizingInterval
		}
		return 24 * time.Hour
	default:
		return cfg.PreMatchInterval
	}
}

func (w *Worker) setCatalog(catalog reconcile.Catalog) {
	w.catalogMu.Lock()
	w.catalog = catalog
	w.catalogMu.Unlock()
}

func (w *Worker) getCatalog() reconcile.Catalog {
	w.catalogMu.RLock()
	defer w.catalogMu.RUnlock()
	copy := make(reconcile.Catalog, len(w.catalog))
	for id, score := range w.catalog {
		copy[id] = score
	}
	return copy
}

func (w *Worker) setFixtureLeagues(ids []int64) {
	w.fixturesMu.Lock()
	w.fixtureLeagueKey = leagueKey(ids)
	w.fixtureLeaguesKnown = true
	w.fixturesMu.Unlock()
}

func (w *Worker) fixtureLeaguesChanged(ids []int64) bool {
	w.fixturesMu.RLock()
	defer w.fixturesMu.RUnlock()
	return !w.fixtureLeaguesKnown || w.fixtureLeagueKey != leagueKey(ids)
}

func leagueKey(ids []int64) string {
	copy := append([]int64(nil), ids...)
	sort.Slice(copy, func(i, j int) bool { return copy[i] < copy[j] })
	parts := make([]string, len(copy))
	for i, id := range copy {
		parts[i] = fmt.Sprintf("%d", id)
	}
	return strings.Join(parts, ",")
}

func (w *Worker) jitter(interval time.Duration) time.Duration {
	if interval <= 0 {
		return time.Second
	}
	w.randomMu.Lock()
	factor := 0.9 + w.random.Float64()*0.2
	w.randomMu.Unlock()
	return time.Duration(float64(interval) * factor)
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
