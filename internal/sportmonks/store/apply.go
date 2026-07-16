package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/realtime"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/reconcile"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

var (
	ErrStaleSnapshot      = errors.New("Sportmonks snapshot is older than committed state")
	ErrCorrectionDisabled = errors.New("live Sportmonks corrections are disabled")
	ErrConcurrentApply    = errors.New("Sportmonks match changed during apply")
	ErrFixtureLeaseLost   = errors.New("Sportmonks fixture lease was lost before apply")
	ErrSettlementInFlight = errors.New("settlement is already processing")
	ErrSettledCorrection  = errors.New("provider correction arrived after settlement")
	ErrTerminalRegression = errors.New("provider status regressed after terminal state")
	ErrFixtureIdentity    = errors.New("provider fixture identity changed")
	ErrMidMatchPromotion  = errors.New("provider fixture cannot be promoted after its scheduled start")
)

func (s *Store) ApplyProjection(ctx context.Context, projection reconcile.Projection, raw []byte, receivedAt time.Time, cfg ApplyOptions) (ApplyResult, error) {
	receivedAt = receivedAt.UTC()
	if cfg.RawPayloadTTL <= 0 {
		cfg.RawPayloadTTL = 30 * 24 * time.Hour
	}
	if err := s.SavePayload(ctx, projection.FixtureID, cfg.Mode, raw, receivedAt, cfg.RawPayloadTTL, true, nil); err != nil {
		return ApplyResult{}, fmt.Errorf("save provider payload: %w", err)
	}
	if cfg.Mode == "shadow" {
		return s.applyShadowProjection(ctx, projection, receivedAt, cfg)
	}
	if cfg.Mode != "live" {
		return ApplyResult{}, fmt.Errorf("unsupported apply mode %q", cfg.Mode)
	}

	session, err := s.db.Client().StartSession()
	if err != nil {
		return ApplyResult{}, err
	}
	defer session.EndSession(ctx)

	transactionOptions := options.Transaction().
		SetReadConcern(readconcern.Snapshot()).
		SetWriteConcern(writeconcern.Majority())
	value, err := session.WithTransaction(ctx, func(sessionContext mongo.SessionContext) (any, error) {
		return s.applyProjectionTransaction(sessionContext, projection, receivedAt, cfg)
	}, transactionOptions)
	if err != nil {
		switch {
		case errors.Is(err, ErrSettledCorrection):
			_ = s.recordIncident(ctx, projection, "settled_correction", err, receivedAt)
		case errors.Is(err, markets.ErrFinalRevisionConflict):
			_ = s.recordIncident(ctx, projection, "final_revision_conflict", err, receivedAt)
		case errors.Is(err, ErrTerminalRegression):
			_ = s.recordIncident(ctx, projection, "terminal_status_regression", err, receivedAt)
		case errors.Is(err, ErrSettlementInFlight):
			_ = s.recordIncident(ctx, projection, "settlement_in_flight_correction", err, receivedAt)
		case errors.Is(err, ErrFixtureIdentity):
			_ = s.recordIncident(ctx, projection, "fixture_identity_changed", err, receivedAt)
		case errors.Is(err, ErrMidMatchPromotion):
			_ = s.recordIncident(ctx, projection, "mid_match_promotion_rejected", err, receivedAt)
		}
		return ApplyResult{}, err
	}
	result, ok := value.(ApplyResult)
	if !ok {
		return ApplyResult{}, errors.New("invalid Sportmonks apply result")
	}
	return result, nil
}

func (s *Store) applyShadowProjection(ctx context.Context, projection reconcile.Projection, receivedAt time.Time, cfg ApplyOptions) (ApplyResult, error) {
	session, err := s.db.Client().StartSession()
	if err != nil {
		return ApplyResult{}, err
	}
	defer session.EndSession(ctx)
	value, err := session.WithTransaction(ctx, func(sessionContext mongo.SessionContext) (any, error) {
		if err := s.touchFixtureLease(sessionContext, projection.FixtureID, receivedAt, cfg); err != nil {
			return nil, err
		}
		var previous ShadowProjection
		err := s.shadow.FindOne(sessionContext, bson.M{"_id": projection.FixtureID}).Decode(&previous)
		if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
			return nil, err
		}
		version := previous.Version + 1
		corrections, missing := shadowDiff(previous.Projection.Deliveries, projection.Deliveries)
		record := ShadowProjection{
			FixtureID: projection.FixtureID, Version: version,
			Projection: projection, ReceivedAt: receivedAt, UpdatedAt: receivedAt,
		}
		if _, err := s.shadow.ReplaceOne(sessionContext, bson.M{"_id": projection.FixtureID}, record, options.Replace().SetUpsert(true)); err != nil {
			return nil, err
		}
		latency := int64(0)
		if projection.ProviderUpdatedAt != nil && receivedAt.After(*projection.ProviderUpdatedAt) {
			latency = receivedAt.Sub(*projection.ProviderUpdatedAt).Milliseconds()
		}
		if _, err := s.reports.InsertOne(sessionContext, ReconciliationReport{
			FixtureID: projection.FixtureID, Version: version,
			SnapshotHash: projection.SnapshotHash, DeliveryCount: len(projection.Deliveries),
			CorrectionCount: corrections, MissingDeliveryCount: missing,
			ProviderUpdateToReceiveMS: latency, Reconciled: true, ReceivedAt: receivedAt,
		}); err != nil {
			return nil, err
		}
		return ApplyResult{
			StateVersion: version, FeedState: "shadow", Applied: true,
			CorrectionCount: corrections, TombstoneCount: missing,
		}, nil
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()))
	if err != nil {
		return ApplyResult{}, err
	}
	result, ok := value.(ApplyResult)
	if !ok {
		return ApplyResult{}, errors.New("invalid shadow apply result")
	}
	return result, nil
}

func shadowDiff(previous, next []reconcile.Delivery) (corrections, missing int) {
	previousByID := make(map[string]string, len(previous))
	nextIDs := make(map[string]struct{}, len(next))
	for _, delivery := range previous {
		previousByID[delivery.ProviderEventID] = delivery.PayloadHash
	}
	for _, delivery := range next {
		nextIDs[delivery.ProviderEventID] = struct{}{}
		if hash, exists := previousByID[delivery.ProviderEventID]; exists && hash != delivery.PayloadHash {
			corrections++
		}
	}
	for providerID := range previousByID {
		if _, exists := nextIDs[providerID]; !exists {
			missing++
		}
	}
	return corrections, missing
}

func (s *Store) recordIncident(ctx context.Context, projection reconcile.Projection, kind string, cause error, now time.Time) error {
	message := cause.Error()
	if len(message) > 1000 {
		message = message[:1000]
	}
	matchID := ""
	var match struct {
		ID primitive.ObjectID `bson:"_id"`
	}
	if err := s.matches.FindOne(ctx, bson.M{
		"provider": ProviderName, "providerFixtureId": projection.FixtureID,
	}, options.FindOne().SetProjection(bson.M{"_id": 1})).Decode(&match); err == nil {
		matchID = match.ID.Hex()
	}
	id := fmt.Sprintf("%s:%d:%s:%s", ProviderName, projection.FixtureID, projection.SnapshotHash, kind)
	_, err := s.incidents.UpdateOne(ctx, bson.M{"_id": id}, bson.M{
		"$setOnInsert": bson.M{
			"fixtureId": projection.FixtureID, "matchId": matchID,
			"kind": kind, "status": "open", "snapshotHash": projection.SnapshotHash,
			"createdAt": now.UTC(),
		},
		"$set": bson.M{"message": message, "updatedAt": now.UTC()},
	}, options.Update().SetUpsert(true))
	return err
}

// MarkFeedUnavailable advances the same match/trading gate used by snapshot
// application. When lastSuccessCutoff is supplied, a newer successful poll
// wins and the stale transition becomes a no-op.
func (s *Store) MarkFeedUnavailable(ctx context.Context, fixtureID int64, state, blocker string, now time.Time, lastSuccessCutoff *time.Time) error {
	return s.markFeedConditionally(ctx, fixtureID, state, blocker, now, func(match matches.Match) bool {
		return lastSuccessCutoff == nil || match.LastSuccessfulPollAt == nil || !match.LastSuccessfulPollAt.After(*lastSuccessCutoff)
	})
}

func (s *Store) MarkFeedFrozen(ctx context.Context, fixtureID int64, now, stateChangeCutoff time.Time) error {
	return s.markFeedConditionally(ctx, fixtureID, matches.FeedStateStale, "feed_stale", now, func(match matches.Match) bool {
		return match.Status == matches.StatusLive && match.LastStateChangeAt != nil && !match.LastStateChangeAt.After(stateChangeCutoff)
	})
}

// ResetFinalizationHolds invalidates provisional innings/match finals after
// any failed poll, even when the feed has not yet crossed its stale deadline.
func (s *Store) ResetFinalizationHolds(ctx context.Context, fixtureID int64, owner, token string, now time.Time) error {
	session, err := s.db.Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sessionContext mongo.SessionContext) (any, error) {
		if err := s.touchFixtureLease(sessionContext, fixtureID, now, ApplyOptions{LeaseOwner: owner, LeaseToken: token}); err != nil {
			return nil, err
		}
		var match matches.Match
		err := s.matches.FindOne(sessionContext, bson.M{
			"provider": ProviderName, "providerFixtureId": fixtureID,
		}).Decode(&match)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if match.FeedState == matches.FeedStateTerminal || !hasFinalizationCandidate(match) {
			return nil, nil
		}
		previousVersion := match.StateVersion
		if err := s.resetPendingFinalization(sessionContext, &match, now.UTC()); err != nil {
			return nil, err
		}
		match.StateVersion++
		match.UpdatedAt = now.UTC()
		result, err := s.matches.ReplaceOne(sessionContext, bson.M{
			"_id": match.ID, "stateVersion": previousVersion,
		}, match)
		if err != nil {
			return nil, err
		}
		if result.ModifiedCount != 1 {
			return nil, ErrConcurrentApply
		}
		if err := s.projectMarkets(sessionContext, match, reconcile.Projection{CurrentInnings: match.Innings}); err != nil {
			return nil, err
		}
		if err := s.insertMarketSnapshots(sessionContext, match, now.UTC()); err != nil {
			return nil, err
		}
		return nil, s.insertMatchOutbox(sessionContext, match, "match.finalization_reset", now.UTC())
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()))
	return err
}

func hasFinalizationCandidate(match matches.Match) bool {
	if match.FinalCandidate != nil {
		return true
	}
	for _, innings := range match.InningsSummaries {
		if innings.FinalCandidate != nil {
			return true
		}
	}
	return false
}

func (s *Store) markFeedConditionally(ctx context.Context, fixtureID int64, state, blocker string, now time.Time, condition func(matches.Match) bool) error {
	session, err := s.db.Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sessionContext mongo.SessionContext) (any, error) {
		var match matches.Match
		err := s.matches.FindOne(sessionContext, bson.M{"provider": ProviderName, "providerFixtureId": fixtureID}).Decode(&match)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if !condition(match) {
			return nil, nil
		}
		if err := s.resetPendingFinalization(sessionContext, &match, now.UTC()); err != nil {
			return nil, err
		}
		blockers := providerBlockers(match.TradingBlockers, blocker)
		if match.FeedState == state && match.TradingState == "blocked" && sameStrings(match.TradingBlockers, blockers) {
			return nil, nil
		}
		previousVersion := match.StateVersion
		match.StateVersion++
		match.TradingVersion++
		match.FeedState = state
		match.HealthySnapshotCount = 0
		match.TradingState = "blocked"
		match.TradingBlockers = appendUnique(blockers, "cancellation_pending")
		match.UpdatedAt = now.UTC()
		result, err := s.matches.ReplaceOne(sessionContext, bson.M{"_id": match.ID, "stateVersion": previousVersion}, match)
		if err != nil {
			return nil, err
		}
		if result.ModifiedCount != 1 {
			return nil, ErrConcurrentApply
		}
		if err := s.projectMarkets(sessionContext, match, reconcile.Projection{CurrentInnings: match.Innings}); err != nil {
			return nil, err
		}
		if err := s.insertMarketSnapshots(sessionContext, match, now.UTC()); err != nil {
			return nil, err
		}
		if err := s.enqueueTradingGateJob(sessionContext, match, now.UTC()); err != nil {
			return nil, err
		}
		return nil, s.insertMatchOutbox(sessionContext, match, "match.feed_state", now.UTC())
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()))
	return err
}

func (s *Store) applyProjectionTransaction(ctx mongo.SessionContext, projection reconcile.Projection, receivedAt time.Time, cfg ApplyOptions) (ApplyResult, error) {
	if err := s.touchFixtureLease(ctx, projection.FixtureID, receivedAt, cfg); err != nil {
		return ApplyResult{}, err
	}
	var current matches.Match
	err := s.matches.FindOne(ctx, bson.M{"provider": ProviderName, "providerFixtureId": projection.FixtureID}).Decode(&current)
	newMatch := errors.Is(err, mongo.ErrNoDocuments)
	if err != nil && !newMatch {
		return ApplyResult{}, err
	}
	if newMatch {
		if !cfg.AllowMidMatchAdmission && (projection.Status != matches.StatusUpcoming || !projection.StartTime.After(receivedAt)) {
			return ApplyResult{}, ErrMidMatchPromotion
		}
		current = initialMatch(projection, receivedAt)
	}
	if !newMatch && fixtureIdentityChanged(current, projection) {
		return ApplyResult{}, ErrFixtureIdentity
	}
	if !newMatch && current.FeedState == matches.FeedStateTerminal &&
		projection.Status != current.Status {
		return ApplyResult{}, ErrTerminalRegression
	}
	if projection.ProviderUpdatedAt != nil && current.LastProviderUpdateAt != nil && projection.ProviderUpdatedAt.Before(*current.LastProviderUpdateAt) {
		return ApplyResult{}, ErrStaleSnapshot
	}

	existingEvents, err := s.providerEvents(ctx, projection.FixtureID)
	if err != nil {
		return ApplyResult{}, err
	}
	candidateByID := make(map[string]reconcile.Delivery, len(projection.Deliveries))
	for _, delivery := range projection.Deliveries {
		candidateByID[delivery.ProviderEventID] = delivery
	}
	for providerID, event := range existingEvents {
		if _, present := candidateByID[providerID]; !present || event.MissingPolls == 0 {
			continue
		}
		result, err := s.events.UpdateOne(ctx, bson.M{"_id": event.ID, "revision": event.Revision}, bson.M{"$set": bson.M{"missingPolls": 0}})
		if err != nil {
			return ApplyResult{}, err
		}
		if result.ModifiedCount != 1 {
			return ApplyResult{}, ErrConcurrentApply
		}
		event.MissingPolls = 0
		existingEvents[providerID] = event
	}

	firstMissing := false
	for providerID, event := range existingEvents {
		if event.Tombstoned || !event.Active {
			continue
		}
		if _, present := candidateByID[providerID]; !present && event.MissingPolls < 1 {
			firstMissing = true
			break
		}
	}
	correctionBlocked := false
	if !cfg.AllowCorrections {
		for providerID, delivery := range candidateByID {
			if event, exists := existingEvents[providerID]; exists && (event.PayloadHash != delivery.PayloadHash || event.Tombstoned || !event.Active) {
				correctionBlocked = true
				break
			}
		}
		if !correctionBlocked {
			for providerID, event := range existingEvents {
				if event.Active && !event.Tombstoned && event.MissingPolls >= 1 {
					if _, present := candidateByID[providerID]; !present {
						correctionBlocked = true
						break
					}
				}
			}
		}
	}
	if firstMissing || correctionBlocked {
		if firstMissing {
			for providerID, event := range existingEvents {
				if event.Active && !event.Tombstoned {
					if _, present := candidateByID[providerID]; !present {
						result, err := s.events.UpdateOne(ctx, bson.M{"_id": event.ID, "revision": event.Revision}, bson.M{"$inc": bson.M{"missingPolls": 1}})
						if err != nil {
							return ApplyResult{}, err
						}
						if result.ModifiedCount != 1 {
							return ApplyResult{}, ErrConcurrentApply
						}
					}
				}
			}
		}
		updated, err := s.setReconciling(ctx, current, receivedAt)
		if err != nil {
			return ApplyResult{}, err
		}
		result := ApplyResult{
			MatchID: updated.ID.Hex(), StateVersion: updated.StateVersion,
			TradingVersion: updated.TradingVersion, FeedState: updated.FeedState,
			Applied: true, Reconciling: true,
		}
		return result, nil
	}

	stateChanged := newMatch || current.LastSnapshotHash != projection.SnapshotHash || current.FeedState != matches.FeedStateHealthy || current.HealthySnapshotCount < 2 || projection.Status == matches.StatusCompleted || inningsHoldPending(current, projection) || tradingGateRefreshNeeded(current, projection)
	if !stateChanged {
		now := receivedAt
		validity := cfg.FeedValidity
		if validity <= 0 {
			validity = 50 * time.Second
		}
		validUntil := now.Add(validity)
		tradingState := current.TradingState
		if len(current.TradingBlockers) == 0 {
			tradingState = "open"
		}
		_, err := s.matches.UpdateOne(ctx, bson.M{"_id": current.ID}, bson.M{"$set": bson.M{
			"feedState":            matches.FeedStateHealthy,
			"tradingState":         tradingState,
			"tradingBlockers":      current.TradingBlockers,
			"lastFeedReceivedAt":   &now,
			"lastSuccessfulPollAt": &now,
			"feedValidUntil":       &validUntil,
			"updatedAt":            now,
		}})
		return ApplyResult{
			MatchID: current.ID.Hex(), StateVersion: current.StateVersion,
			TradingVersion: current.TradingVersion, FeedState: matches.FeedStateHealthy,
		}, err
	}

	nextVersion := current.StateVersion + 1
	corrections, tombstones, changedEvents, err := s.applyEvents(ctx, current.ID.Hex(), projection, existingEvents, receivedAt)
	if err != nil {
		return ApplyResult{}, err
	}
	resetCorrectionRecovery(&current, corrections, tombstones)
	next := projectMatch(current, projection, receivedAt, cfg.InningsFinalizationHold, cfg.MatchFinalizationHold, cfg.FeedValidity, nextVersion)
	if err := s.holdInvalidSettlementJobs(ctx, current, next, receivedAt); err != nil {
		return ApplyResult{}, err
	}
	globalKill, err := s.GlobalTradingKilled(ctx)
	if err != nil {
		return ApplyResult{}, err
	}
	if globalKill {
		next.TradingState = "blocked"
		next.TradingBlockers = appendUnique(next.TradingBlockers, "global_kill")
		if !containsValue(current.TradingBlockers, "global_kill") && next.TradingVersion == current.TradingVersion {
			next.TradingVersion++
		}
	}
	needsCancellation := next.TradingState != "open" && (newMatch || next.TradingVersion != current.TradingVersion)
	if needsCancellation {
		next.TradingBlockers = appendUnique(next.TradingBlockers, "cancellation_pending")
		if next.TradingVersion == current.TradingVersion {
			next.TradingVersion++
		}
	}
	if newMatch {
		if _, err := s.matches.InsertOne(ctx, next); err != nil {
			return ApplyResult{}, err
		}
	} else {
		result, err := s.matches.ReplaceOne(ctx, bson.M{"_id": current.ID, "stateVersion": current.StateVersion}, next)
		if err != nil {
			return ApplyResult{}, err
		}
		if result.ModifiedCount != 1 {
			return ApplyResult{}, ErrConcurrentApply
		}
	}

	if err := s.projectMarkets(ctx, next, projection); err != nil {
		return ApplyResult{}, err
	}
	if err := s.insertMarketSnapshots(ctx, next, receivedAt); err != nil {
		return ApplyResult{}, err
	}
	if err := s.enqueueSettlementJobs(ctx, next, receivedAt); err != nil {
		return ApplyResult{}, err
	}
	if projection.Status == matches.StatusAbandoned {
		if err := s.enqueueVoidJobs(ctx, next, receivedAt); err != nil {
			return ApplyResult{}, err
		}
	}
	if needsCancellation {
		if err := s.enqueueTradingGateJob(ctx, next, receivedAt); err != nil {
			return ApplyResult{}, err
		}
	}
	if err := s.insertMatchOutbox(ctx, next, "match.state", receivedAt); err != nil {
		return ApplyResult{}, err
	}
	for _, event := range changedEvents {
		if err := s.insertDeliveryOutbox(ctx, next, event, receivedAt); err != nil {
			return ApplyResult{}, err
		}
	}
	return ApplyResult{
		MatchID: next.ID.Hex(), StateVersion: next.StateVersion,
		TradingVersion: next.TradingVersion, FeedState: next.FeedState,
		Applied: true, CorrectionCount: corrections, TombstoneCount: tombstones,
	}, nil
}

func fixtureIdentityChanged(current matches.Match, projection reconcile.Projection) bool {
	return current.ProviderFixtureID > 0 && current.ProviderFixtureID != projection.FixtureID ||
		current.ProviderLeagueID > 0 && current.ProviderLeagueID != projection.LeagueID ||
		current.ProviderSeasonID > 0 && current.ProviderSeasonID != projection.SeasonID ||
		current.ProviderTeamAID > 0 && current.ProviderTeamAID != projection.LocalTeamID ||
		current.ProviderTeamBID > 0 && current.ProviderTeamBID != projection.VisitorTeamID
}

func (s *Store) touchFixtureLease(ctx context.Context, fixtureID int64, now time.Time, cfg ApplyOptions) error {
	owner := strings.TrimSpace(cfg.LeaseOwner)
	token := strings.TrimSpace(cfg.LeaseToken)
	if fixtureID <= 0 || owner == "" || token == "" {
		return ErrFixtureLeaseLost
	}
	result, err := s.fixtures.UpdateOne(ctx, bson.M{
		"_id": fixtureID, "leaseOwner": owner, "leaseToken": token,
		"leaseUntil": bson.M{"$gt": now.UTC()},
	}, bson.M{
		"$set": bson.M{"applyObservedAt": now.UTC()},
		"$inc": bson.M{"applyGeneration": 1},
	})
	if err != nil {
		return err
	}
	if result.ModifiedCount != 1 {
		return ErrFixtureLeaseLost
	}
	return nil
}

func (s *Store) providerEvents(ctx context.Context, fixtureID int64) (map[string]matches.BallEvent, error) {
	cursor, err := s.events.Find(ctx, bson.M{"provider": ProviderName, "providerFixtureId": fixtureID})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var events []matches.BallEvent
	if err := cursor.All(ctx, &events); err != nil {
		return nil, err
	}
	byID := make(map[string]matches.BallEvent, len(events))
	for _, event := range events {
		byID[event.ProviderEventID] = event
	}
	return byID, nil
}

func (s *Store) applyEvents(ctx context.Context, matchID string, projection reconcile.Projection, existing map[string]matches.BallEvent, receivedAt time.Time) (int, int, []matches.BallEvent, error) {
	corrections := 0
	tombstones := 0
	changed := make([]matches.BallEvent, 0)
	present := make(map[string]struct{}, len(projection.Deliveries))
	for _, delivery := range projection.Deliveries {
		present[delivery.ProviderEventID] = struct{}{}
		old, exists := existing[delivery.ProviderEventID]
		if exists && old.PayloadHash == delivery.PayloadHash && !old.Tombstoned {
			if old.MissingPolls != 0 || !old.Active {
				result, err := s.events.UpdateOne(ctx, bson.M{"_id": old.ID, "revision": old.Revision}, bson.M{"$set": bson.M{
					"missingPolls": 0, "active": true,
				}})
				if err != nil {
					return 0, 0, nil, err
				}
				if result.ModifiedCount != 1 {
					return 0, 0, nil, ErrConcurrentApply
				}
			}
			continue
		}
		revision := int64(1)
		id := primitive.NewObjectID()
		createdAt := receivedAt
		if exists {
			revision = old.Revision + 1
			id = old.ID
			createdAt = old.CreatedAt
			corrections++
		}
		event := deliveryEvent(matchID, projection.FixtureID, delivery, revision, id, createdAt, receivedAt)
		if _, err := s.revisions.InsertOne(ctx, EventRevision{
			Provider: ProviderName, ProviderFixtureID: projection.FixtureID,
			ProviderEventID: delivery.ProviderEventID, Revision: revision,
			Event: event, ObservedAt: receivedAt,
		}); err != nil {
			return 0, 0, nil, err
		}
		if exists {
			result, err := s.events.ReplaceOne(ctx, bson.M{"_id": old.ID, "revision": old.Revision}, event)
			if err != nil {
				return 0, 0, nil, err
			}
			if result.ModifiedCount != 1 {
				return 0, 0, nil, ErrConcurrentApply
			}
		} else if _, err := s.events.InsertOne(ctx, event); err != nil {
			return 0, 0, nil, err
		}
		changed = append(changed, event)
	}
	for providerID, old := range existing {
		if _, found := present[providerID]; found || old.Tombstoned || !old.Active {
			continue
		}
		if old.MissingPolls < 1 {
			return 0, 0, nil, fmt.Errorf("missing event %s was not confirmed", providerID)
		}
		revision := old.Revision + 1
		old.Revision = revision
		old.Active = false
		old.Tombstoned = true
		old.MissingPolls++
		old.SupersededRevision = revision
		old.ReceivedAt = &receivedAt
		if _, err := s.revisions.InsertOne(ctx, EventRevision{
			Provider: ProviderName, ProviderFixtureID: projection.FixtureID,
			ProviderEventID: providerID, Revision: revision, Event: old, ObservedAt: receivedAt,
		}); err != nil {
			return 0, 0, nil, err
		}
		result, err := s.events.ReplaceOne(ctx, bson.M{"_id": old.ID, "revision": revision - 1}, old)
		if err != nil {
			return 0, 0, nil, err
		}
		if result.ModifiedCount != 1 {
			return 0, 0, nil, ErrConcurrentApply
		}
		tombstones++
		changed = append(changed, old)
	}
	return corrections, tombstones, changed, nil
}

func deliveryEvent(matchID string, fixtureID int64, delivery reconcile.Delivery, revision int64, id primitive.ObjectID, createdAt, receivedAt time.Time) matches.BallEvent {
	over, ball := displayBall(delivery.ProviderBall)
	var extra *string
	switch {
	case delivery.Extras.Wides > 0:
		value := matches.ExtraWide
		extra = &value
	case delivery.Extras.NoBalls > 0:
		value := matches.ExtraNoBall
		extra = &value
	case delivery.Extras.Byes > 0:
		value := matches.ExtraBye
		extra = &value
	case delivery.Extras.LegByes > 0:
		value := matches.ExtraLegBye
		extra = &value
	case delivery.Extras.Penalties > 0:
		value := matches.ExtraPenalty
		extra = &value
	}
	return matches.BallEvent{
		ID: id, MatchID: matchID, Innings: delivery.Innings, Over: over, Ball: ball,
		LegalBall: delivery.LegalBall, Runs: delivery.TeamRuns,
		IsWicket: delivery.Dismissal != nil, Extra: extra,
		Provider: ProviderName, ProviderFixtureID: fixtureID,
		ProviderEventID: delivery.ProviderEventID, ProviderScoreID: delivery.ProviderScoreID,
		ProviderBall: delivery.ProviderBall, Sequence: delivery.Sequence,
		Revision: revision, PayloadHash: delivery.PayloadHash,
		TeamRuns: delivery.TeamRuns, BatterRuns: delivery.BatterRuns,
		Extras: delivery.Extras, Dismissal: delivery.Dismissal, Active: true,
		ProviderUpdatedAt: delivery.ProviderUpdatedAt, ReceivedAt: &receivedAt, CreatedAt: createdAt,
	}
}

func initialMatch(projection reconcile.Projection, now time.Time) matches.Match {
	return matches.Match{
		ID: primitive.NewObjectID(), DataSource: ProviderName, Provider: ProviderName,
		ProviderFixtureID: projection.FixtureID, ProviderLeagueID: projection.LeagueID,
		ProviderSeasonID: projection.SeasonID, ProviderTeamAID: projection.LocalTeamID,
		ProviderTeamBID: projection.VisitorTeamID,
		TournamentID:    fmt.Sprintf("sportmonks:%d", projection.LeagueID), Format: projection.Format,
		TeamAID:   fmt.Sprintf("sportmonks:%d", projection.LocalTeamID),
		TeamBID:   fmt.Sprintf("sportmonks:%d", projection.VisitorTeamID),
		TeamAName: teamName(projection.LocalTeamName, projection.LocalTeamID),
		TeamBName: teamName(projection.VisitorTeamName, projection.VisitorTeamID),
		StartTime: projection.StartTime, Status: matches.StatusUpcoming,
		Innings: 1, BallsLeft: projection.ScheduledBalls, ScheduledBalls: projection.ScheduledBalls,
		StateVersion: 0, TradingVersion: 0, FeedState: matches.FeedStateWarming,
		TradingState: "blocked", TradingBlockers: []string{"warming"},
		LastStateChangeAt: timePointer(now), CreatedAt: now, UpdatedAt: now,
	}
}

func projectMatch(current matches.Match, projection reconcile.Projection, receivedAt time.Time, inningsHold, finalHold, feedValidity time.Duration, nextVersion int64) matches.Match {
	next := current
	next.DataSource = ProviderName
	next.Provider = ProviderName
	next.ProviderFixtureID = projection.FixtureID
	next.ProviderLeagueID = projection.LeagueID
	next.ProviderSeasonID = projection.SeasonID
	next.ProviderTeamAID = projection.LocalTeamID
	next.ProviderTeamBID = projection.VisitorTeamID
	next.Format = projection.Format
	next.StartTime = projection.StartTime
	next.ProviderPhase = projection.ProviderStatus
	next.ScheduledBalls = projection.ScheduledBalls
	next.ProviderBattingTeamID = projection.BattingTeamID
	next.Innings = projection.CurrentInnings
	if next.Innings <= 0 {
		next.Innings = max(1, current.Innings)
	}
	next.CurrentScore = projection.CurrentScore
	next.WicketsLost = projection.Wickets
	next.BallsLeft = max(0, projection.ScheduledBalls-projection.LegalBalls)
	next.TargetScore = projection.Target
	next.OversText = fmt.Sprintf("%d.%d", projection.LegalBalls/6, projection.LegalBalls%6)
	next.InningsSummaries = inningsSummaries(projection.Innings, current.InningsSummaries, nextVersion, receivedAt, inningsHold)
	if projection.Status == matches.StatusAbandoned {
		freezeAbandonmentDispositions(current, &next)
	}
	next.LastFeedReceivedAt = timePointer(receivedAt)
	next.LastSuccessfulPollAt = timePointer(receivedAt)
	next.LastProviderUpdateAt = projection.ProviderUpdatedAt
	next.LastSnapshotHash = projection.SnapshotHash
	if feedValidity <= 0 {
		feedValidity = 50 * time.Second
	}
	next.FeedValidUntil = timePointer(receivedAt.Add(feedValidity))
	if current.LastSnapshotHash != projection.SnapshotHash || current.LastStateChangeAt == nil {
		next.LastStateChangeAt = timePointer(receivedAt)
	}
	next.StateVersion = nextVersion
	next.UpdatedAt = receivedAt

	previousTradingState := current.TradingState
	previousBlockers := append([]string(nil), current.TradingBlockers...)
	switch projection.Status {
	case matches.StatusLive:
		if projectionCurrentInningsComplete(projection) {
			next.Status = matches.StatusInningsBreak
			next.FeedState = matches.FeedStateFinalizing
			next.HealthySnapshotCount = current.HealthySnapshotCount + 1
			next.TradingState = "blocked"
			next.TradingBlockers = providerBlockers(current.TradingBlockers, "finalizing")
			break
		}
		next.Status = matches.StatusLive
		next.FinalCandidate = nil
		if projection.CurrentInnings != current.Innings {
			next.HealthySnapshotCount = 1
		} else {
			next.HealthySnapshotCount = current.HealthySnapshotCount + 1
		}
		if next.HealthySnapshotCount >= 2 {
			next.FeedState = matches.FeedStateHealthy
			next.TradingState = "open"
			next.TradingBlockers = providerBlockers(current.TradingBlockers)
		} else {
			next.FeedState = matches.FeedStateWarming
			next.TradingState = "blocked"
			next.TradingBlockers = providerBlockers(current.TradingBlockers, "warming")
		}
	case matches.StatusInningsBreak:
		next.Status = matches.StatusInningsBreak
		next.FeedState = matches.FeedStateHealthy
		next.HealthySnapshotCount = current.HealthySnapshotCount + 1
		next.TradingState = "blocked"
		next.TradingBlockers = providerBlockers(current.TradingBlockers, "innings_break")
	case matches.StatusCompleted:
		next.Status = matches.StatusInningsBreak
		next.FeedState = matches.FeedStateFinalizing
		next.HealthySnapshotCount = current.HealthySnapshotCount + 1
		next.TradingState = "closed"
		next.TradingBlockers = providerBlockers(current.TradingBlockers, "finalizing")
		next.FinalCandidate = advanceFinalCandidate(current.FinalCandidate, projection.SnapshotHash, nextVersion, receivedAt)
		if finalHold <= 0 {
			finalHold = 2 * time.Minute
		}
		if next.FinalCandidate.IdenticalPolls >= 3 && receivedAt.Sub(next.FinalCandidate.FirstSeenAt) >= finalHold {
			next.Status = matches.StatusCompleted
			next.FeedState = matches.FeedStateTerminal
		}
	case matches.StatusAbandoned:
		next.Status = matches.StatusAbandoned
		next.FeedState = matches.FeedStateTerminal
		next.HealthySnapshotCount = 0
		next.TradingState = "closed"
		next.TradingBlockers = providerBlockers(current.TradingBlockers, "not_live")
		if abandonmentSettlementPending(next) {
			next.FeedState = matches.FeedStateFinalizing
			next.TradingBlockers = providerBlockers(current.TradingBlockers, "not_live", "finalizing")
		}
	default:
		next.Status = matches.StatusUpcoming
		next.FeedState = matches.FeedStateWarming
		next.HealthySnapshotCount = 0
		next.TradingState = "blocked"
		next.TradingBlockers = providerBlockers(current.TradingBlockers, "not_live")
	}
	if next.TradingState == "open" && len(next.TradingBlockers) > 0 {
		next.TradingState = "blocked"
	}
	if next.TradingState != previousTradingState || !sameStrings(next.TradingBlockers, previousBlockers) {
		next.TradingVersion = current.TradingVersion + 1
	}
	return next
}

func projectionCurrentInningsComplete(projection reconcile.Projection) bool {
	for _, innings := range projection.Innings {
		if innings.Number == projection.CurrentInnings {
			return innings.Complete
		}
	}
	return false
}

func resetCorrectionRecovery(match *matches.Match, corrections, tombstones int) {
	if match != nil && (corrections > 0 || tombstones > 0) {
		match.HealthySnapshotCount = 0
	}
}

func (s *Store) setReconciling(ctx mongo.SessionContext, current matches.Match, now time.Time) (matches.Match, error) {
	if err := s.resetPendingFinalization(ctx, &current, now); err != nil {
		return matches.Match{}, err
	}
	previousTradingVersion := current.TradingVersion
	current.StateVersion++
	if current.FeedState != matches.FeedStateReconciling || current.TradingState == "open" {
		current.TradingVersion++
	}
	current.FeedState = matches.FeedStateReconciling
	current.HealthySnapshotCount = 0
	current.TradingState = "blocked"
	current.TradingBlockers = providerBlockers(current.TradingBlockers, "reconciling")
	needsCancellation := current.TradingVersion != previousTradingVersion
	if needsCancellation {
		current.TradingBlockers = appendUnique(current.TradingBlockers, "cancellation_pending")
	}
	current.LastFeedReceivedAt = timePointer(now)
	current.UpdatedAt = now
	result, err := s.matches.ReplaceOne(ctx, bson.M{"_id": current.ID, "stateVersion": current.StateVersion - 1}, current)
	if err != nil {
		return matches.Match{}, err
	}
	if result.ModifiedCount != 1 {
		return matches.Match{}, ErrConcurrentApply
	}
	if err := s.projectMarkets(ctx, current, reconcile.Projection{CurrentInnings: current.Innings, Format: current.Format, ScheduledBalls: current.ScheduledBalls}); err != nil {
		return matches.Match{}, err
	}
	if err := s.insertMarketSnapshots(ctx, current, now); err != nil {
		return matches.Match{}, err
	}
	if needsCancellation {
		if err := s.enqueueTradingGateJob(ctx, current, now); err != nil {
			return matches.Match{}, err
		}
	}
	if err := s.insertMatchOutbox(ctx, current, "match.feed_state", now); err != nil {
		return matches.Match{}, err
	}
	return current, nil
}

func (s *Store) projectMarkets(ctx context.Context, match matches.Match, projection reconcile.Projection) error {
	if s.markets == nil || match.Innings < 1 || match.Innings > 2 ||
		match.Status == matches.StatusUpcoming || match.ProviderBattingTeamID == 0 {
		return nil
	}
	battingName := match.TeamAName
	if match.ProviderBattingTeamID == match.ProviderTeamBID {
		battingName = match.TeamBName
	}
	if err := s.markets.EnsureProviderInningsMarket(ctx, markets.ProviderInningsMarketSpec{
		MatchID: match.ID.Hex(), Innings: match.Innings, BattingTeamName: battingName,
		Format: match.Format, ScheduledBalls: match.ScheduledBalls,
		StateVersion: match.StateVersion, TradingVersion: match.TradingVersion,
		FeedState: match.FeedState, Blockers: match.TradingBlockers,
	}); err != nil {
		return err
	}
	marketList, err := s.markets.ListMarketsByMatchID(ctx, match.ID.Hex())
	if err != nil {
		return fmt.Errorf("list provider markets: %w", err)
	}
	if match.Status == matches.StatusAbandoned {
		for _, market := range marketList {
			if market.Kind != markets.MarketKindInningsScore ||
				market.FormulaVersion != markets.FormulaVersionInningsScoreV1 ||
				market.Lifecycle == markets.MarketLifecycleSettled ||
				market.Lifecycle == markets.MarketLifecycleVoid {
				continue
			}
			if innings, settles := settlementDisposition(match, market.Innings); settles {
				var final *int
				finalRevision := int64(0)
				if innings.SettlementReady && innings.FinalCandidate != nil {
					score := innings.Runs
					final = &score
					finalRevision = innings.FinalCandidate.Revision
				}
				if err := s.markets.SetProviderMarketGate(
					ctx, match.ID.Hex(), market.Innings, markets.MarketLifecycleSettling,
					[]string{"finalizing"}, final, finalRevision,
				); err != nil {
					return err
				}
				continue
			}
			if err := s.markets.SetProviderMarketGate(
				ctx, match.ID.Hex(), market.Innings, markets.MarketLifecycleSettling,
				[]string{"voiding"}, nil, 0,
			); err != nil {
				return err
			}
		}
		return nil
	}
	lifecycle := markets.MarketLifecyclePending
	if match.HealthySnapshotCount >= 2 || providerMarketPreviouslyOpened(marketList, match.Innings) {
		lifecycle = markets.MarketLifecycleOpen
	}
	currentSettlementReady := false
	for _, innings := range match.InningsSummaries {
		if innings.Innings == match.Innings {
			currentSettlementReady = innings.SettlementReady &&
				(innings.Innings != 2 || match.Status == matches.StatusCompleted)
			break
		}
	}
	if (match.FeedState == matches.FeedStateFinalizing || match.FeedState == matches.FeedStateTerminal) && currentSettlementReady {
		lifecycle = markets.MarketLifecycleSettling
	}
	if err := s.markets.SetProviderMarketGate(ctx, match.ID.Hex(), match.Innings, lifecycle, match.TradingBlockers, nil, 0); err != nil {
		return err
	}
	for _, innings := range match.InningsSummaries {
		if !innings.SettlementReady || innings.Innings == match.Innings && match.Status == matches.StatusLive {
			continue
		}
		if innings.Innings == 2 && match.Status != matches.StatusCompleted && match.Status != matches.StatusAbandoned {
			continue
		}
		final := innings.Runs
		finalRevision := innings.Revision
		if innings.FinalCandidate != nil {
			finalRevision = innings.FinalCandidate.Revision
		}
		if err := s.markets.SetProviderMarketGate(ctx, match.ID.Hex(), innings.Innings, markets.MarketLifecycleSettling, []string{"finalizing"}, &final, finalRevision); err != nil {
			return err
		}
	}
	return nil
}

func providerMarketPreviouslyOpened(marketsForMatch []markets.Market, innings int) bool {
	for _, market := range marketsForMatch {
		if market.Kind != markets.MarketKindInningsScore || market.Innings != innings ||
			market.FormulaVersion != markets.FormulaVersionInningsScoreV1 {
			continue
		}
		return market.Lifecycle != "" && market.Lifecycle != markets.MarketLifecyclePending
	}
	return false
}

func (s *Store) insertMarketSnapshots(ctx context.Context, match matches.Match, now time.Time) error {
	if s.markets == nil || s.marketSnaps == nil {
		return nil
	}
	marketList, err := s.markets.ListMarketsByMatchID(ctx, match.ID.Hex())
	if err != nil {
		return fmt.Errorf("list provider markets for snapshots: %w", err)
	}
	for _, market := range marketList {
		if market.Kind != markets.MarketKindInningsScore || market.FormulaVersion != markets.FormulaVersionInningsScoreV1 {
			continue
		}
		snapshot := MarketSnapshot{
			ID:      fmt.Sprintf("%s:%d:%d", market.ID.Hex(), match.StateVersion, match.TradingVersion),
			MatchID: match.ID.Hex(), MarketID: market.ID.Hex(), Innings: market.Innings,
			Lifecycle: market.Lifecycle, Blockers: append([]string(nil), market.Blockers...),
			FeedState: match.FeedState, TradingState: match.TradingState,
			StateVersion: match.StateVersion, TradingVersion: match.TradingVersion,
			CurrentScore: match.CurrentScore, WicketsLost: match.WicketsLost,
			BallsLeft: match.BallsLeft, TargetScore: match.TargetScore,
			FinalScore: market.FinalScore, FinalRevision: market.FinalRevision,
			FormulaVersion: market.FormulaVersion, ProviderSnapshotID: match.LastSnapshotHash,
			CreatedAt: now.UTC(),
		}
		if _, err := s.marketSnaps.UpdateOne(ctx, bson.M{"_id": snapshot.ID}, bson.M{"$setOnInsert": snapshot}, options.Update().SetUpsert(true)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) enqueueSettlementJobs(ctx context.Context, match matches.Match, now time.Time) error {
	for _, innings := range match.InningsSummaries {
		if !innings.SettlementReady || innings.FinalCandidate == nil {
			continue
		}
		if innings.Innings == 2 && match.Status != matches.StatusCompleted && match.Status != matches.StatusAbandoned {
			continue
		}
		id := fmt.Sprintf("%s:%d", match.ID.Hex(), innings.Innings)
		job := SettlementJob{
			ID: id, MatchID: match.ID.Hex(), Innings: innings.Innings,
			FinalScore: innings.Runs, FinalRevision: innings.FinalCandidate.Revision,
			SnapshotHash:   innings.FinalCandidate.SnapshotHash,
			FormulaVersion: "innings_score_v1", Status: "pending",
			CreatedAt: now, UpdatedAt: now,
		}
		var existing SettlementJob
		err := s.settlements.FindOne(ctx, bson.M{"_id": id}).Decode(&existing)
		if errors.Is(err, mongo.ErrNoDocuments) {
			if _, err := s.settlements.InsertOne(ctx, job); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if existing.SnapshotHash == job.SnapshotHash && existing.FinalScore == job.FinalScore &&
			existing.Status != "held" && existing.Action != "void" {
			continue
		}
		switch existing.Status {
		case "pending", "failed", "held":
			_, err = s.settlements.UpdateOne(ctx, bson.M{"_id": id, "status": existing.Status}, bson.M{
				"$set": bson.M{
					"finalScore": job.FinalScore, "finalRevision": job.FinalRevision,
					"snapshotHash": job.SnapshotHash, "status": "pending",
					"lastError": "", "updatedAt": now,
				},
				"$unset": bson.M{"action": "", "leaseOwner": "", "leaseUntil": "", "nextAttemptAt": ""},
			})
			if err != nil {
				return err
			}
		case "processing":
			return ErrSettlementInFlight
		case "complete":
			return ErrSettledCorrection
		default:
			return fmt.Errorf("unknown settlement job status %q", existing.Status)
		}
	}
	return nil
}

func (s *Store) holdInvalidSettlementJobs(ctx context.Context, current, next matches.Match, now time.Time) error {
	nextReady := make(map[int]bool, len(next.InningsSummaries))
	for _, summary := range next.InningsSummaries {
		nextReady[summary.Innings] = summary.SettlementReady
	}
	for _, summary := range current.InningsSummaries {
		if !summary.SettlementReady || nextReady[summary.Innings] {
			continue
		}
		if err := s.holdSettlementJob(ctx, current.ID.Hex(), summary.Innings, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) resetPendingFinalization(ctx context.Context, match *matches.Match, now time.Time) error {
	if match == nil || match.FeedState == matches.FeedStateTerminal {
		return nil
	}
	match.FinalCandidate = nil
	for i := range match.InningsSummaries {
		summary := &match.InningsSummaries[i]
		if summary.FinalCandidate == nil {
			continue
		}
		if summary.SettlementReady {
			if err := s.holdSettlementJob(ctx, match.ID.Hex(), summary.Innings, now); err != nil {
				if errors.Is(err, ErrSettlementInFlight) || errors.Is(err, ErrSettledCorrection) {
					continue
				}
				return err
			}
		}
		summary.FinalCandidate = nil
		summary.SettlementReady = false
	}
	return nil
}

func (s *Store) holdSettlementJob(ctx context.Context, matchID string, innings int, now time.Time) error {
	id := fmt.Sprintf("%s:%d", matchID, innings)
	var job SettlementJob
	err := s.settlements.FindOne(ctx, bson.M{"_id": id}).Decode(&job)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil
	}
	if err != nil {
		return err
	}
	switch job.Status {
	case "pending", "failed", "held":
		_, err = s.settlements.UpdateOne(ctx, bson.M{
			"_id": id, "status": bson.M{"$in": []string{"pending", "failed", "held"}},
		}, bson.M{
			"$set":   bson.M{"status": "held", "lastError": "finalization_hold_reset", "updatedAt": now.UTC()},
			"$unset": bson.M{"leaseOwner": "", "leaseUntil": "", "nextAttemptAt": ""},
		})
		return err
	case "processing":
		return ErrSettlementInFlight
	case "complete":
		return ErrSettledCorrection
	default:
		return fmt.Errorf("unknown settlement job status %q", job.Status)
	}
}

func (s *Store) enqueueVoidJobs(ctx context.Context, match matches.Match, now time.Time) error {
	marketList, err := s.markets.ListMarketsByMatchID(ctx, match.ID.Hex())
	if err != nil {
		return fmt.Errorf("list provider markets for void: %w", err)
	}
	for _, market := range marketList {
		if market.Kind != markets.MarketKindInningsScore ||
			market.FormulaVersion != markets.FormulaVersionInningsScoreV1 ||
			market.Lifecycle == markets.MarketLifecycleSettled ||
			market.Lifecycle == markets.MarketLifecycleVoid {
			continue
		}
		if _, settles := settlementDisposition(match, market.Innings); settles {
			continue
		}
		if market.SettlementRevision > 0 {
			return ErrSettlementInFlight
		}
		job := SettlementJob{
			ID:      fmt.Sprintf("%s:%d", match.ID.Hex(), market.Innings),
			MatchID: match.ID.Hex(), Innings: market.Innings,
			FormulaVersion: markets.FormulaVersionInningsScoreV1,
			Action:         "void", Status: "pending", CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
		}
		if err := s.enqueueVoidJob(ctx, job); err != nil {
			return err
		}
	}
	return nil
}

func settlementDisposition(match matches.Match, innings int) (matches.InningsSummary, bool) {
	for _, summary := range match.InningsSummaries {
		if summary.Innings == innings {
			return summary, summary.FinalDisposition == matches.FinalDispositionSettle
		}
	}
	return matches.InningsSummary{}, false
}

func abandonmentSettlementPending(match matches.Match) bool {
	for _, summary := range match.InningsSummaries {
		if summary.FinalDisposition == matches.FinalDispositionSettle && !summary.SettlementReady {
			return true
		}
	}
	return false
}

func freezeAbandonmentDispositions(current matches.Match, next *matches.Match) {
	if next == nil {
		return
	}
	previous := make(map[int]matches.InningsSummary, len(current.InningsSummaries))
	for _, summary := range current.InningsSummaries {
		previous[summary.Innings] = summary
	}
	for i := range next.InningsSummaries {
		summary := &next.InningsSummaries[i]
		prior := previous[summary.Innings]
		disposition := prior.FinalDisposition
		if disposition == "" {
			if prior.SettlementReady && prior.FinalCandidate != nil {
				disposition = matches.FinalDispositionSettle
			} else {
				disposition = matches.FinalDispositionVoid
			}
		}
		summary.FinalDisposition = disposition
		if disposition == matches.FinalDispositionVoid {
			summary.FinalCandidate = nil
			summary.SettlementReady = false
		}
	}
}

// enqueueVoidJob atomically creates a void job or converts an existing
// retryable settlement job. Abandonment must supersede a pending/failed/held
// score settlement; otherwise a duplicate _id would leave that job held
// forever. Processing jobs fail closed and completed jobs remain immutable.
func (s *Store) enqueueVoidJob(ctx context.Context, job SettlementJob) error {
	job.Action = "void"
	job.Status = "pending"
	job.FinalScore = 0
	job.FinalRevision = 0
	job.SnapshotHash = ""
	job.UpdatedAt = job.UpdatedAt.UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = job.UpdatedAt
	}

	result, err := s.settlements.UpdateOne(ctx, bson.M{
		"_id":    job.ID,
		"status": bson.M{"$in": []string{"pending", "failed", "held"}},
	}, voidJobConversionUpdate(job))
	if err != nil {
		return err
	}
	if result.MatchedCount == 1 {
		return nil
	}

	var existing SettlementJob
	err = s.settlements.FindOne(ctx, bson.M{"_id": job.ID}).Decode(&existing)
	if errors.Is(err, mongo.ErrNoDocuments) {
		_, err = s.settlements.InsertOne(ctx, job)
		if mongo.IsDuplicateKeyError(err) {
			return ErrConcurrentApply
		}
		return err
	}
	if err != nil {
		return err
	}
	return classifyUnconvertedVoidJob(existing.Status)
}

func voidJobConversionUpdate(job SettlementJob) bson.M {
	return bson.M{
		"$set": bson.M{
			"matchId": job.MatchID, "innings": job.Innings,
			"formulaVersion": job.FormulaVersion,
			"action":         job.Action, "status": job.Status, "updatedAt": job.UpdatedAt,
		},
		"$unset": bson.M{
			"finalScore": "", "finalRevision": "", "snapshotHash": "",
			"leaseOwner": "", "leaseUntil": "", "nextAttemptAt": "", "lastError": "",
		},
	}
}

func classifyUnconvertedVoidJob(status string) error {
	switch status {
	case "pending", "failed", "held":
		return ErrConcurrentApply
	case "complete":
		return nil
	case "processing":
		return ErrSettlementInFlight
	default:
		return fmt.Errorf("unknown settlement job status %q", status)
	}
}

func (s *Store) enqueueTradingGateJob(ctx context.Context, match matches.Match, now time.Time) error {
	job := TradingGateJob{
		ID:      match.ID.Hex(),
		MatchID: match.ID.Hex(), TradingVersion: match.TradingVersion,
		Status: "pending", CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
	var existing TradingGateJob
	err := s.gateJobs.FindOne(ctx, bson.M{"_id": job.ID}).Decode(&existing)
	if errors.Is(err, mongo.ErrNoDocuments) {
		_, err = s.gateJobs.InsertOne(ctx, job)
		if mongo.IsDuplicateKeyError(err) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	if existing.Status == "pending" || existing.Status == "processing" {
		_, err = s.gateJobs.UpdateOne(ctx, bson.M{"_id": job.ID}, bson.M{
			"$max": bson.M{"tradingVersion": job.TradingVersion},
			"$set": bson.M{"updatedAt": now.UTC()},
		})
		return err
	}
	if existing.Status == "failed" {
		_, err = s.gateJobs.UpdateOne(ctx, bson.M{"_id": job.ID, "status": "failed"}, bson.M{
			"$max":   bson.M{"tradingVersion": job.TradingVersion},
			"$set":   bson.M{"status": "pending", "lastError": "", "updatedAt": now.UTC()},
			"$unset": bson.M{"leaseOwner": "", "leaseUntil": "", "nextAttemptAt": ""},
		})
		return err
	}
	if existing.Status != "complete" {
		return fmt.Errorf("unknown trading gate job status %q", existing.Status)
	}
	result, err := s.gateJobs.UpdateOne(ctx, bson.M{"_id": job.ID, "status": "complete"}, bson.M{
		"$set": bson.M{
			"status": "pending", "tradingVersion": job.TradingVersion,
			"attempts": 0, "lastError": "", "createdAt": now.UTC(), "updatedAt": now.UTC(),
		},
		"$unset": bson.M{"leaseOwner": "", "leaseUntil": "", "nextAttemptAt": ""},
	})
	if err != nil {
		return err
	}
	if result.ModifiedCount != 1 {
		return ErrConcurrentApply
	}
	return nil
}

func (s *Store) insertMatchOutbox(ctx context.Context, match matches.Match, eventType string, now time.Time) error {
	event := OutboxEvent{
		EventID: fmt.Sprintf("sportmonks:%d:%d:%d:%s", match.ProviderFixtureID, match.StateVersion, match.TradingVersion, eventType),
		Topic:   realtime.MatchScoreTopic(match.ID.Hex()), Type: eventType,
		MatchID: match.ID.Hex(), StateVersion: match.StateVersion, TradingVersion: match.TradingVersion,
		Sequence: match.StateVersion, OccurredAt: now, Payload: scorePayload(match), CreatedAt: now,
	}
	_, err := s.outbox.InsertOne(ctx, event)
	return err
}

func (s *Store) insertDeliveryOutbox(ctx context.Context, match matches.Match, event matches.BallEvent, now time.Time) error {
	payload := map[string]any{
		"matchId": match.ID.Hex(), "eventId": event.ProviderEventID,
		"innings": event.Innings, "over": event.Over, "ball": event.Ball,
		"providerBall": event.ProviderBall, "sequence": event.Sequence, "revision": event.Revision,
		"legalBall": event.LegalBall, "runs": event.Runs, "batterRuns": event.BatterRuns,
		"extra": event.Extra, "extras": event.Extras, "isWicket": event.IsWicket, "dismissal": event.Dismissal,
		"tombstoned": event.Tombstoned, "isCorrection": event.Revision > 1,
		"providerModifiedAt": event.ProviderUpdatedAt, "receivedAt": event.ReceivedAt,
		"supersededRevision": event.SupersededRevision,
		"currentScore":       match.CurrentScore, "wicketsLost": match.WicketsLost,
		"ballsLeft": match.BallsLeft, "targetScore": match.TargetScore, "oversText": match.OversText,
		"stateVersion": match.StateVersion, "tradingVersion": match.TradingVersion,
	}
	outbox := OutboxEvent{
		EventID: fmt.Sprintf("sportmonks:%d:%s:%d", match.ProviderFixtureID, event.ProviderEventID, event.Revision),
		Topic:   realtime.MatchCommentaryTopic(match.ID.Hex()), Type: "match.delivery",
		MatchID: match.ID.Hex(), StateVersion: match.StateVersion, TradingVersion: match.TradingVersion,
		Sequence: event.Sequence, OccurredAt: now, Payload: payload, CreatedAt: now,
	}
	_, err := s.outbox.InsertOne(ctx, outbox)
	return err
}

func scorePayload(match matches.Match) map[string]any {
	return map[string]any{
		"matchId": match.ID.Hex(), "innings": match.Innings,
		"currentScore": match.CurrentScore, "wicketsLost": match.WicketsLost,
		"ballsLeft": match.BallsLeft, "targetScore": match.TargetScore,
		"oversText": match.OversText, "status": match.Status,
		"providerPhase": match.ProviderPhase, "feedState": match.FeedState,
		"tradingState": match.TradingState, "tradingBlockers": match.TradingBlockers,
		"stateVersion": match.StateVersion, "tradingVersion": match.TradingVersion,
		"inningsSummaries":     match.InningsSummaries,
		"lastSuccessfulPollAt": match.LastSuccessfulPollAt, "feedValidUntil": match.FeedValidUntil,
	}
}

func inningsSummaries(input []reconcile.Innings, current []matches.InningsSummary, revision int64, now time.Time, hold time.Duration) []matches.InningsSummary {
	if hold <= 0 {
		hold = time.Minute
	}
	previous := make(map[int]matches.InningsSummary, len(current))
	for _, innings := range current {
		previous[innings.Innings] = innings
	}
	out := make([]matches.InningsSummary, 0, len(input))
	for _, innings := range input {
		summary := matches.InningsSummary{
			Innings: innings.Number, BattingTeamID: innings.BattingTeamID,
			Runs: innings.Runs, Wickets: innings.Wickets, LegalBalls: innings.LegalBalls,
			ScheduledBalls: innings.ScheduledBalls, Target: innings.Target,
			Complete: innings.Complete, Revision: revision,
		}
		if summary.Complete {
			hash := innings.SnapshotHash
			if hash == "" {
				hash = inningsSnapshotHash(summary)
			}
			summary.FinalCandidate = advanceFinalCandidate(previous[summary.Innings].FinalCandidate, hash, revision, now)
			summary.SettlementReady = summary.FinalCandidate.IdenticalPolls >= 3 && now.Sub(summary.FinalCandidate.FirstSeenAt) >= hold
		}
		out = append(out, summary)
	}
	return out
}

func inningsHoldPending(current matches.Match, projection reconcile.Projection) bool {
	ready := make(map[int]bool, len(current.InningsSummaries))
	for _, innings := range current.InningsSummaries {
		ready[innings.Innings] = innings.SettlementReady
	}
	for _, innings := range projection.Innings {
		if innings.Complete && !ready[innings.Number] {
			return true
		}
	}
	return false
}

func tradingGateRefreshNeeded(current matches.Match, projection reconcile.Projection) bool {
	return projection.Status == matches.StatusLive &&
		current.FeedState == matches.FeedStateHealthy &&
		current.HealthySnapshotCount >= 2 &&
		current.TradingState != "open" &&
		len(providerBlockers(current.TradingBlockers)) == 0
}

func inningsSnapshotHash(summary matches.InningsSummary) string {
	value := fmt.Sprintf("%d:%d:%d:%d:%d:%d", summary.Innings, summary.BattingTeamID, summary.Runs, summary.Wickets, summary.LegalBalls, summary.ScheduledBalls)
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func advanceFinalCandidate(current *matches.FinalCandidate, hash string, revision int64, now time.Time) *matches.FinalCandidate {
	if current == nil || current.SnapshotHash != hash {
		return &matches.FinalCandidate{Revision: revision, SnapshotHash: hash, IdenticalPolls: 1, FirstSeenAt: now, LastSeenAt: now}
	}
	next := *current
	next.IdenticalPolls++
	next.LastSeenAt = now
	return &next
}

var automatedBlockers = map[string]struct{}{
	"warming": {}, "not_live": {}, "feed_stale": {}, "reconciling": {},
	"innings_break": {}, "quota_limited": {}, "unsupported": {}, "finalizing": {},
	"league_disabled": {},
}

func providerBlockers(existing []string, desired ...string) []string {
	seen := make(map[string]struct{}, len(existing)+len(desired))
	out := make([]string, 0, len(existing)+len(desired))
	for _, blocker := range existing {
		blocker = strings.TrimSpace(blocker)
		if blocker == "" {
			continue
		}
		if _, automated := automatedBlockers[blocker]; automated {
			continue
		}
		if _, duplicate := seen[blocker]; !duplicate {
			seen[blocker] = struct{}{}
			out = append(out, blocker)
		}
	}
	for _, blocker := range desired {
		if _, duplicate := seen[blocker]; blocker != "" && !duplicate {
			seen[blocker] = struct{}{}
			out = append(out, blocker)
		}
	}
	return out
}

func displayBall(value string) (int, int) {
	parts := strings.SplitN(strings.TrimSpace(value), ".", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	over, _ := strconv.Atoi(parts[0])
	ball, _ := strconv.Atoi(parts[1])
	return over, ball
}

func teamName(name string, id int64) string {
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	return fmt.Sprintf("Team %d", id)
}

func timePointer(value time.Time) *time.Time {
	copy := value
	return &copy
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
