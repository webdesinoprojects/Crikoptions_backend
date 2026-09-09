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
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/reconcile"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/store"
)

var ErrQuotaReserved = errors.New("CricLive quota reserve reached")

// scheduleHorizon bounds how far ahead the schedule is turned into fixture
// targets. CricLive publishes months of fixtures in one response; polling
// targets are only useful near their start time.
const (
	scheduleLookback = 2 * 24 * time.Hour
	scheduleHorizon  = 14 * 24 * time.Hour
)

type Provider interface {
	LiveScores(context.Context) (client.LiveResponse, client.RateLimit, error)
	Schedule(context.Context) (client.ScheduleResponse, client.RateLimit, error)
	Commentary(context.Context, int64) (client.CommentaryResponse, client.RateLimit, error)
	Scorecard(context.Context, int64) (client.ScorecardResponse, client.RateLimit, error)
	Overs(context.Context, int64) (client.OversResponse, client.RateLimit, error)
}

type Storage interface {
	SyncLeagues(context.Context, []client.Series, time.Time, bool) error
	UpsertSeries(context.Context, []client.Series, time.Time) error
	EnabledLeagueIDs(context.Context) ([]int64, error)
	UpsertFixtureTargets(context.Context, []client.Fixture, time.Time, bool, bool) error
	PublishFixtureMatches(context.Context, []client.Fixture, time.Time, bool) error
	ConsumeRequestQuota(context.Context, string, time.Time, int, int) (bool, error)
	ClaimSchedule(context.Context, string, string, time.Time, time.Duration) (bool, error)
	DueTargets(context.Context, time.Time, int64) ([]store.FixtureTarget, error)
	PollableTargetCount(context.Context, time.Time) (int64, error)
	OpenTargetCount(context.Context, time.Time, string) (int64, error)
	ClaimTarget(context.Context, int64, string, time.Time, time.Duration) (string, bool, error)
	RenewTargetLease(context.Context, int64, string, string, time.Time) error
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
		return nil, errors.New("cannot construct CricLive worker in off mode")
	}
	if provider == nil || storage == nil {
		return nil, errors.New("CricLive worker requires provider and storage")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, errors.New("CricLive worker owner is required")
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
				w.logger.Printf("criclive dispatch: %v", err)
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
					w.logger.Printf("criclive %s sync: %v", name, err)
				}
			}
		}
	}()
}

func (w *Worker) bootstrap(ctx context.Context) error {
	if err := w.syncFixtures(ctx); err != nil {
		w.logger.Printf("criclive initial schedule sync: %v", err)
	}
	if err := w.discoverLive(ctx); err != nil {
		w.logger.Printf("criclive initial live discovery: %v", err)
	}
	if count, err := w.store.RescheduleStaleTargets(ctx, w.now().UTC()); err != nil {
		w.logger.Printf("criclive reschedule stale targets: %v", err)
	} else if count > 0 {
		w.logger.Printf("criclive rescheduled %d fixture targets for immediate poll", count)
	}
	return nil
}

// syncFixtures reads the CricLive schedule, which is a single global response
// covering every series. It is the authority for start times and is therefore
// also where the series directory is refreshed.
func (w *Worker) syncFixtures(ctx context.Context) error {
	if !w.fixtureSyncMu.TryLock() {
		return nil
	}
	defer w.fixtureSyncMu.Unlock()
	claimed, err := w.store.ClaimSchedule(ctx, "fixtures-catalog", w.owner, w.now().UTC(), 15*time.Minute)
	if err != nil || !claimed {
		return err
	}
	if !w.takeProviderQuota(ctx, client.EndpointSchedule) {
		return ErrQuotaReserved
	}
	response, rateLimit, err := w.provider.Schedule(ctx)
	w.quota.observe(client.EndpointSchedule, w.now().UTC(), rateLimit)
	if err != nil {
		return err
	}

	now := w.now().UTC()
	from, to := now.Add(-scheduleLookback), now.Add(scheduleHorizon)
	series := make([]client.Series, 0, 64)
	seenSeries := make(map[int64]struct{}, 64)
	fixtures := make([]client.Fixture, 0, 128)
	for _, day := range response.Data {
		for _, group := range day.Series {
			if group.SeriesID > 0 {
				if _, seen := seenSeries[group.SeriesID]; !seen {
					seenSeries[group.SeriesID] = struct{}{}
					series = append(series, client.Series{
						ID: group.SeriesID, Name: group.SeriesName, Category: group.SeriesCategory,
					})
				}
			}
			for _, match := range group.Matches {
				fixture := client.FixtureFromScheduleMatch(match, group)
				if fixture.ID <= 0 || fixture.StartingAt.IsZero() {
					continue
				}
				if fixture.StartingAt.Before(from) || fixture.StartingAt.After(to) {
					continue
				}
				fixtures = append(fixtures, fixture)
			}
		}
	}

	// The series directory is refreshed from the full schedule, not from the
	// windowed fixture list, so a series with no fixture in the window is not
	// mistaken for one that has been withdrawn.
	if err := w.store.SyncLeagues(ctx, series, now, w.cfg.Mode == client.ModeLive); err != nil {
		return err
	}
	if err := w.store.UpsertFixtureTargets(ctx, fixtures, now, w.cfg.Mode == client.ModeLive, w.cfg.AllowMidMatchLiveAdmission); err != nil {
		return err
	}
	if w.cfg.Mode == client.ModeLive {
		if err := w.store.PublishFixtureMatches(ctx, fixtures, now, w.cfg.AllowMidMatchLiveAdmission); err != nil {
			return err
		}
	}
	leagueIDs, err := w.store.EnabledLeagueIDs(ctx)
	if err != nil {
		return err
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
	if !w.takeProviderQuota(ctx, client.EndpointLive) {
		return ErrQuotaReserved
	}
	response, rateLimit, err := w.provider.LiveScores(ctx)
	w.quota.observe(client.EndpointLive, w.now().UTC(), rateLimit)
	if err != nil {
		return err
	}
	now := w.now().UTC()
	fixtures := make([]client.Fixture, 0, len(response.Data))
	series := make([]client.Series, 0, len(response.Data))
	seenSeries := make(map[int64]struct{}, len(response.Data))
	for _, item := range response.Data {
		fixture := client.FixtureFromMatchItem(item, now)
		if fixture.ID <= 0 {
			continue
		}
		// A live fixture whose date could not be parsed still needs a start
		// time for scheduling; treat it as under way now.
		if fixture.StartingAt.IsZero() {
			fixture.StartingAt = now
		}
		fixtures = append(fixtures, fixture)
		if fixture.SeriesID > 0 {
			if _, seen := seenSeries[fixture.SeriesID]; !seen {
				seenSeries[fixture.SeriesID] = struct{}{}
				series = append(series, client.Series{
					ID: fixture.SeriesID, Name: fixture.SeriesName, Category: fixture.MatchType,
				})
			}
		}
	}
	// Live series are upserted without the revocation sweep the schedule
	// performs: /cricket/live only ever shows a handful of series, so treating
	// it as the full directory would disable everything else.
	if err := w.store.UpsertSeries(ctx, series, now); err != nil {
		return err
	}
	if err := w.store.UpsertFixtureTargets(ctx, fixtures, now, w.cfg.Mode == client.ModeLive, w.cfg.AllowMidMatchLiveAdmission); err != nil {
		return err
	}
	if w.cfg.Mode == client.ModeLive {
		if err := w.store.PublishFixtureMatches(ctx, fixtures, now, w.cfg.AllowMidMatchLiveAdmission); err != nil {
			return err
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

// leaseHeartbeat keeps a claimed fixture lease alive for as long as this worker
// is genuinely working on it. The lease exists to stop two workers polling the
// same fixture, not to cap how long a poll may take — and a first apply that
// backfills a whole innings of deliveries can exceed any fixed TTL. It returns
// a stop func that must be called before the poll records its outcome.
func (w *Worker) leaseHeartbeat(ctx context.Context, fixtureID int64, token string) func() {
	ttl := w.cfg.LeaseTTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	// Renew well inside the TTL so one slow or dropped renewal is survivable.
	// Derived from the TTL rather than floored at a constant: a floor larger
	// than the TTL would never renew in time, which is the very failure this
	// heartbeat exists to prevent.
	interval := ttl / 3
	if interval > 10*time.Second {
		interval = 10 * time.Second
	}
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				err := w.store.RenewTargetLease(ctx, fixtureID, w.owner, token, w.now().UTC().Add(ttl))
				if err == nil {
					continue
				}
				// The lease is gone; another worker owns the fixture now. Stop
				// renewing and let the poll's own fencing reject its result.
				if errors.Is(err, store.ErrFixtureLeaseLost) {
					w.logger.Printf("criclive fixture %d lease lost while polling", fixtureID)
					return
				}
				w.logger.Printf("criclive fixture %d renew lease: %v", fixtureID, err)
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

// pollTarget fetches one match. The commentary endpoint carries the
// authoritative live state including who is at the crease, and the overs
// endpoint carries the recent ball-by-ball. The scorecard is fetched alongside
// them so the innings detail stays in step without a later round trip.
func (w *Worker) pollTarget(ctx context.Context, target store.FixtureTarget, token string, activeInterval time.Duration) {
	stopHeartbeat := w.leaseHeartbeat(ctx, target.ID, token)
	defer stopHeartbeat()
	now := w.now().UTC()
	// One poll spends three provider requests; they are reserved together so a
	// partially funded poll never produces a half-read snapshot.
	if !w.takeProviderQuotaN(ctx, client.EndpointCommentary, 3) {
		if w.cfg.Mode == client.ModeLive {
			if err := w.store.ResetFinalizationHolds(ctx, target.ID, w.owner, token, now); err != nil {
				if errors.Is(err, store.ErrFixtureLeaseLost) {
					w.logger.Printf("criclive fixture %d quota hold abandoned: lease lost", target.ID)
					return
				}
				w.logger.Printf("criclive fixture %d reset finalization hold: %v", target.ID, err)
			}
			_ = w.store.MarkFeedUnavailable(ctx, target.ID, matches.FeedStateQuotaLimited, "quota_limited", now, nil)
		}
		if err := w.store.FailTargetPoll(ctx, target.ID, w.owner, token, ErrQuotaReserved, now, now.Add(w.cfg.MaxPollInterval)); err != nil {
			w.logger.Printf("criclive fixture %d record quota hold: %v", target.ID, err)
		}
		return
	}

	snapshot, raw, err := w.fetchSnapshot(ctx, target)
	if err != nil {
		w.handlePollFailure(ctx, target, token, intervalForProviderStatus(target.ProviderStatus, activeInterval, w.cfg), err)
		return
	}
	receivedAt := w.now().UTC()
	snapshot.ReceivedAt = receivedAt

	projection, err := reconcile.ReduceSnapshot(snapshot)
	if err != nil {
		_ = w.store.SavePayload(ctx, target.ID, string(w.cfg.Mode), raw, receivedAt, w.cfg.RawPayloadTTL, false, err)
		if w.cfg.Mode == client.ModeLive {
			providerStatus := snapshotState(snapshot)
			if providerStatus != "" && reconcile.IsTerminalProviderStatus(providerStatus) {
				closed, closeErr := w.store.ApplyProviderTerminalClosure(ctx, target.ID, providerStatus, receivedAt, store.ApplyOptions{
					Mode: string(w.cfg.Mode), LeaseOwner: w.owner, LeaseToken: token,
				})
				if closeErr != nil {
					w.logger.Printf("criclive fixture %d terminal closure: %v", target.ID, closeErr)
				} else if closed {
					w.logger.Printf("criclive fixture %d: closed after reduce failure provider=%s", target.ID, providerStatus)
					next := receivedAt.Add(intervalForProviderStatus(providerStatus, activeInterval, w.cfg))
					_ = w.store.CompleteTargetPoll(ctx, target.ID, w.owner, token, string(w.cfg.Mode), "", providerStatus, receivedAt, next)
					return
				}
			}
			if resetErr := w.store.ResetFinalizationHolds(ctx, target.ID, w.owner, token, receivedAt); resetErr != nil {
				if errors.Is(resetErr, store.ErrFixtureLeaseLost) {
					return
				}
				w.logger.Printf("criclive fixture %d reset finalization hold: %v", target.ID, resetErr)
			}
			state, blocker := matches.FeedStateReconciling, "reconciling"
			if errors.Is(err, reconcile.ErrUnsupportedFormat) {
				state, blocker = matches.FeedStateUnsupported, reconcile.UnsupportedBlocker(err)
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
			"criclive fixture %d: applied feed=%s stateVersion=%d reconciling=%t",
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
		w.logger.Printf("criclive fixture %d complete poll: %v", target.ID, err)
	}
}

// fetchSnapshot reads the three per-match endpoints and returns the combined
// snapshot alongside the raw payload retained for diagnostics. Commentary is
// mandatory; the overs and scorecard add detail but cannot invalidate a score,
// so their failures are logged and tolerated.
func (w *Worker) fetchSnapshot(ctx context.Context, target store.FixtureTarget) (client.Snapshot, []byte, error) {
	commentary, rateLimit, err := w.provider.Commentary(ctx, target.ID)
	w.quota.observe(client.EndpointCommentary, w.now().UTC(), rateLimit)
	if err != nil {
		return client.Snapshot{}, nil, err
	}

	overs, rateLimit, oversErr := w.provider.Overs(ctx, target.ID)
	w.quota.observe(client.EndpointOvers, w.now().UTC(), rateLimit)
	if oversErr != nil {
		w.logger.Printf("criclive fixture %d overs unavailable: %v", target.ID, oversErr)
	}

	scorecard, rateLimit, cardErr := w.provider.Scorecard(ctx, target.ID)
	w.quota.observe(client.EndpointScorecard, w.now().UTC(), rateLimit)
	if cardErr != nil {
		w.logger.Printf("criclive fixture %d scorecard unavailable: %v", target.ID, cardErr)
	}

	snapshot := client.Snapshot{
		MatchID:    target.ID,
		Fixture:    fixtureFromTarget(target),
		Commentary: commentary.Data,
		Scorecard:  scorecard.Data,
		Overs:      overs.Data,
	}
	raw, err := json.Marshal(struct {
		Commentary json.RawMessage `json:"commentary,omitempty"`
		Overs      json.RawMessage `json:"overs,omitempty"`
		Scorecard  json.RawMessage `json:"scorecard,omitempty"`
	}{Commentary: commentary.Raw, Overs: overs.Raw, Scorecard: scorecard.Raw})
	if err != nil {
		return snapshot, nil, err
	}
	snapshot.Raw = raw
	return snapshot, raw, nil
}

// fixtureFromTarget rebuilds the identity the reducer needs from the stored
// polling target, so a poll does not depend on a concurrent discovery pass.
func fixtureFromTarget(target store.FixtureTarget) client.Fixture {
	return client.Fixture{
		ID:            target.ID,
		SeriesID:      target.LeagueID,
		Format:        target.Format,
		StartingAt:    target.StartTime,
		LocalTeamID:   target.LocalTeamID,
		VisitorTeamID: target.VisitorTeamID,
		Status:        target.ProviderStatus,
		State:         target.ProviderStatus,
	}
}

// snapshotState reports the provider state from a snapshot that failed to
// reduce, so a finished match can still be closed out.
func snapshotState(snapshot client.Snapshot) string {
	if state := strings.TrimSpace(snapshot.Commentary.MiniScore.State); state != "" {
		return state
	}
	if state := strings.TrimSpace(snapshot.Commentary.MatchHeader.State); state != "" {
		return state
	}
	return strings.TrimSpace(snapshot.Fixture.State)
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
	return w.takeProviderQuotaN(ctx, endpoint, 1)
}

// takeProviderQuotaN reserves several requests at once. A CricLive poll reads
// three endpoints, and reserving them as a unit stops a poll from starting with
// only part of its budget available.
func (w *Worker) takeProviderQuotaN(ctx context.Context, endpoint string, count int) bool {
	if count < 1 {
		count = 1
	}
	now := w.now().UTC()
	for i := 0; i < count; i++ {
		if !w.quota.take(endpoint, now) {
			return false
		}
	}
	for i := 0; i < count; i++ {
		allowed, err := w.store.ConsumeRequestQuota(
			ctx, endpoint, now, w.cfg.HourlyRequestLimit, w.cfg.QuotaReservePercent,
		)
		if err != nil {
			w.logger.Printf("criclive quota %s: %v", endpoint, err)
			return false
		}
		if !allowed {
			return false
		}
	}
	return true
}

func (w *Worker) handlePollFailure(ctx context.Context, target store.FixtureTarget, token string, scheduled time.Duration, cause error) {
	now := w.now().UTC()
	next := now.Add(w.failureBackoff(target, cause))
	if errors.Is(cause, store.ErrFixtureLeaseLost) {
		// Another worker owns the fixture; it will record the outcome. Log it —
		// a silent return here once hid a fixture that retried for 90 minutes
		// while leaving no failure, no error and no advancing poll timestamp.
		w.logger.Printf("criclive fixture %d poll abandoned: lease lost", target.ID)
		return
	}
	if w.cfg.Mode == client.ModeLive {
		if err := w.store.ResetFinalizationHolds(ctx, target.ID, w.owner, token, now); err != nil {
			w.logger.Printf("criclive fixture %d reset finalization hold: %v", target.ID, err)
			if errors.Is(err, store.ErrFixtureLeaseLost) {
				return
			}
		}
		staleAfter := feedValidityForInterval(w.cfg, scheduled)
		cutoff := now.Add(-staleAfter)
		_ = w.store.MarkFeedUnavailable(ctx, target.ID, matches.FeedStateStale, "feed_stale", now, &cutoff)
	}
	if err := w.store.FailTargetPoll(ctx, target.ID, w.owner, token, cause, now, next); err != nil {
		w.logger.Printf("criclive fixture %d record poll failure (cause %v): %v", target.ID, cause, err)
	}
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
