package store

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/reconcile"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const maxReconcilingPolls = 5

func liveContextReady(ctx *matches.LiveMatchContext) bool {
	if ctx == nil {
		return false
	}
	return strings.TrimSpace(ctx.Striker.Name) != "" &&
		strings.TrimSpace(ctx.NonStriker.Name) != "" &&
		strings.TrimSpace(ctx.Bowler.Name) != ""
}

func applyProjectionOverlay(match *matches.Match, projection reconcile.Projection, receivedAt time.Time, feedValidity time.Duration) {
	if match == nil {
		return
	}
	if feedValidity <= 0 {
		feedValidity = 50 * time.Second
	}
	if projection.CurrentInnings > 0 {
		match.Innings = projection.CurrentInnings
	}
	if projection.BattingTeamID > 0 {
		match.ProviderBattingTeamID = projection.BattingTeamID
	}
	match.ProviderPhase = projection.ProviderStatus
	if applyTerminalMatchState(match, projection.ProviderStatus) {
		return
	}
	match.CurrentScore = projection.CurrentScore
	match.WicketsLost = projection.Wickets
	match.BallsLeft = max(0, projection.ScheduledBalls-projection.LegalBalls)
	match.TargetScore = projection.Target
	match.OversText = fmt.Sprintf("%d.%d", projection.LegalBalls/6, projection.LegalBalls%6)
	if projection.LiveContext != nil {
		match.LiveContext = projection.LiveContext
	}
	if projection.MatchPulse != nil {
		match.MatchPulse = projection.MatchPulse
	}
	match.ThisOver = projection.ThisOver
	match.LastFeedReceivedAt = timePointer(receivedAt)
	match.LastSuccessfulPollAt = timePointer(receivedAt)
	match.FeedValidUntil = timePointer(receivedAt.Add(feedValidity))
	if projection.ProviderUpdatedAt != nil {
		match.LastProviderUpdateAt = projection.ProviderUpdatedAt
	}
}

func applyFeedHealth(match *matches.Match, projection reconcile.Projection) {
	if match == nil {
		return
	}
	if match.FeedState == matches.FeedStateTerminal {
		return
	}
	switch projection.Status {
	case matches.StatusLive:
		// Successful live scoreboard polls keep trading open even when on-field
		// batting/bowling names are incomplete (UI can still show "waiting").
		match.FeedState = matches.FeedStateHealthy
		match.TradingBlockers = providerBlockers(match.TradingBlockers)
		if len(match.TradingBlockers) == 0 {
			match.TradingState = "open"
		} else {
			match.TradingState = "blocked"
		}
		return
	case matches.StatusCompleted, matches.StatusAbandoned:
		if reconcile.IsExplicitTerminalProviderStatus(projection.ProviderStatus) {
			applyTerminalMatchState(match, projection.ProviderStatus)
		} else {
			match.FeedState = matches.FeedStateFinalizing
			match.TradingState = "closed"
			match.TradingBlockers = providerBlockers(match.TradingBlockers, "finalizing")
		}
	case matches.StatusInningsBreak:
		match.FeedState = matches.FeedStateHealthy
		match.TradingState = "blocked"
		match.TradingBlockers = providerBlockers(match.TradingBlockers, "innings_break")
	default:
		// Overlay polls may omit Status while still carrying a full on-field matrix.
		if liveContextReady(projection.LiveContext) {
			match.FeedState = matches.FeedStateHealthy
			match.TradingBlockers = providerBlockers(match.TradingBlockers)
			if len(match.TradingBlockers) == 0 {
				match.TradingState = "open"
			} else {
				match.TradingState = "blocked"
			}
		}
	}
}

func (s *Store) persistMatchSnapshot(
	ctx mongo.SessionContext,
	current matches.Match,
	receivedAt time.Time,
	eventType string,
) (matches.Match, error) {
	current.UpdatedAt = receivedAt
	result, err := s.matches.ReplaceOne(ctx, bson.M{"_id": current.ID, "stateVersion": current.StateVersion}, current)
	if err != nil {
		return matches.Match{}, err
	}
	if result.ModifiedCount != 1 {
		return matches.Match{}, ErrConcurrentApply
	}
	if err := s.insertMatchOutbox(ctx, current, eventType, receivedAt); err != nil {
		return matches.Match{}, err
	}
	return current, nil
}

func (s *Store) applyReconcilingPoll(
	ctx mongo.SessionContext,
	current matches.Match,
	projection reconcile.Projection,
	receivedAt time.Time,
	cfg ApplyOptions,
	reason string,
) (matches.Match, error) {
	if err := s.resetPendingFinalization(ctx, &current, receivedAt); err != nil {
		return matches.Match{}, err
	}
	previousTradingState := current.TradingState
	previousTradingVersion := current.TradingVersion
	previousBlockers := append([]string(nil), current.TradingBlockers...)
	current.ProviderReconcilePolls++
	current.StateVersion++

	applyProjectionOverlay(&current, projection, receivedAt, cfg.FeedValidity)
	needsCancellation := decideReconcilingTradingGate(&current, projection, previousTradingState, previousTradingVersion, previousBlockers)
	current.UpdatedAt = receivedAt

	log.Printf(
		"sportmonks fixture %d match %s: reconciling poll %d reason=%s feed=%s trading=%s blockers=%v cancel=%t",
		projection.FixtureID, current.ID.Hex(), current.ProviderReconcilePolls, reason,
		current.FeedState, current.TradingState, current.TradingBlockers, needsCancellation,
	)

	result, err := s.matches.ReplaceOne(ctx, bson.M{"_id": current.ID, "stateVersion": current.StateVersion - 1}, current)
	if err != nil {
		return matches.Match{}, err
	}
	if result.ModifiedCount != 1 {
		return matches.Match{}, ErrConcurrentApply
	}
	if err := s.projectMarkets(ctx, current, projection); err != nil {
		return matches.Match{}, err
	}
	if err := s.insertMarketSnapshots(ctx, current, receivedAt); err != nil {
		return matches.Match{}, err
	}
	if current.FeedState == matches.FeedStateTerminal {
		if err := s.enqueueSettlementJobs(ctx, current, receivedAt); err != nil {
			return matches.Match{}, err
		}
		if current.Status == matches.StatusAbandoned {
			if err := s.enqueueVoidJobs(ctx, current, receivedAt); err != nil {
				return matches.Match{}, err
			}
		}
	}
	if needsCancellation {
		if err := s.enqueueTradingGateJob(ctx, current, receivedAt); err != nil {
			return matches.Match{}, err
		}
	}
	if err := s.insertMatchOutbox(ctx, current, "match.state", receivedAt); err != nil {
		return matches.Match{}, err
	}
	return current, nil
}

// decideReconcilingTradingGate updates feed/trading during a soft reconcile poll.
// Live scoreboard polls keep buy/sell open; only a real gate change (open → blocked)
// bumps tradingVersion and schedules order cancellation.
func decideReconcilingTradingGate(
	match *matches.Match,
	projection reconcile.Projection,
	previousTradingState string,
	previousTradingVersion int64,
	previousBlockers []string,
) bool {
	if match == nil {
		return false
	}
	if match.FeedState == matches.FeedStateTerminal {
		return false
	}
	switch projection.Status {
	case matches.StatusLive:
		match.FeedState = matches.FeedStateHealthy
		match.TradingBlockers = providerBlockers(match.TradingBlockers)
		match.TradingBlockers = removeValue(match.TradingBlockers, "cancellation_pending")
		if !matches.HasHardTradingBlockers(match.TradingBlockers) {
			match.TradingState = "open"
			match.TradingBlockers = matches.HardTradingBlockers(match.TradingBlockers)
		} else {
			match.TradingState = "blocked"
		}
	case matches.StatusCompleted, matches.StatusAbandoned:
		match.FeedState = matches.FeedStateFinalizing
		match.TradingState = "closed"
		match.TradingBlockers = providerBlockers(match.TradingBlockers, "finalizing")
	case matches.StatusInningsBreak:
		match.FeedState = matches.FeedStateHealthy
		match.TradingState = "blocked"
		match.TradingBlockers = providerBlockers(match.TradingBlockers, "innings_break")
	default:
		// Incomplete projection during soft sync: keep buy/sell open if the match
		// is already live. Only mark feed as reconciling for the SYNC badge.
		if strings.EqualFold(strings.TrimSpace(match.Status), matches.StatusLive) ||
			strings.EqualFold(strings.TrimSpace(previousTradingState), "open") {
			match.Status = matches.StatusLive
			match.FeedState = matches.FeedStateReconciling
			match.TradingBlockers = matches.HardTradingBlockers(providerBlockers(match.TradingBlockers))
			match.TradingBlockers = removeValue(match.TradingBlockers, "cancellation_pending")
			if len(match.TradingBlockers) == 0 {
				match.TradingState = "open"
			} else {
				match.TradingState = "blocked"
			}
		} else {
			match.FeedState = matches.FeedStateReconciling
			match.HealthySnapshotCount = 0
			match.TradingState = "blocked"
			match.TradingBlockers = providerBlockers(match.TradingBlockers, "reconciling")
		}
	}

	wasTradable := strings.EqualFold(strings.TrimSpace(previousTradingState), "open") &&
		!matches.HasHardTradingBlockers(previousBlockers) &&
		!containsValue(previousBlockers, "cancellation_pending")
	nowTradable := strings.EqualFold(strings.TrimSpace(match.TradingState), "open") &&
		!matches.HasHardTradingBlockers(match.TradingBlockers)
	if nowTradable {
		// Soft sync while live: keep the same trading fence so buy/sell stay available.
		match.TradingVersion = previousTradingVersion
		return false
	}
	gateChanged := match.TradingState != previousTradingState ||
		!sameStrings(providerBlockers(match.TradingBlockers), providerBlockers(previousBlockers)) ||
		containsValue(previousBlockers, "cancellation_pending") != containsValue(match.TradingBlockers, "cancellation_pending")
	if gateChanged && match.TradingVersion == previousTradingVersion {
		match.TradingVersion++
	}
	if wasTradable && !nowTradable {
		match.TradingBlockers = appendUnique(match.TradingBlockers, "cancellation_pending")
		if match.TradingVersion == previousTradingVersion {
			match.TradingVersion++
		}
		return true
	}
	return false
}

// HealFalselyStaleLiveMatches reopens live matches stuck in feed_stale / warming
// while polls are still fresh, so the UI does not linger on SYNCING.
func (s *Store) HealFalselyStaleLiveMatches(ctx context.Context, now time.Time, freshWithin time.Duration) (int, error) {
	if freshWithin <= 0 {
		freshWithin = 2 * time.Minute
	}
	now = now.UTC()
	cutoff := now.Add(-freshWithin)
	cursor, err := s.matches.Find(ctx, bson.M{
		"provider": ProviderName,
		"status":   matches.StatusLive,
		"$or": bson.A{
			bson.M{"feedState": matches.FeedStateStale},
			bson.M{"feedState": matches.FeedStateWarming},
			bson.M{"feedState": matches.FeedStateReconciling},
			bson.M{"tradingBlockers": "feed_stale"},
			bson.M{"tradingBlockers": "warming"},
			bson.M{"tradingBlockers": "reconciling"},
		},
		"lastSuccessfulPollAt": bson.M{"$gt": cutoff},
	})
	if err != nil {
		return 0, err
	}
	defer cursor.Close(ctx)
	var rows []matches.Match
	if err := cursor.All(ctx, &rows); err != nil {
		return 0, err
	}
	healed := 0
	for _, match := range rows {
		previousVersion := match.StateVersion
		match.StateVersion++
		match.TradingVersion++
		match.FeedState = matches.FeedStateHealthy
		if match.HealthySnapshotCount < 1 {
			match.HealthySnapshotCount = 1
		}
		match.TradingState = "open"
		match.TradingBlockers = providerBlockers(match.TradingBlockers)
		match.FeedValidUntil = timePointer(now.Add(freshWithin))
		match.UpdatedAt = now
		result, err := s.matches.ReplaceOne(ctx, bson.M{"_id": match.ID, "stateVersion": previousVersion}, match)
		if err != nil {
			return healed, err
		}
		if result.ModifiedCount != 1 {
			continue
		}
		if err := s.projectMarkets(ctx, match, reconcile.Projection{CurrentInnings: match.Innings}); err != nil {
			return healed, err
		}
		_ = s.insertMatchOutbox(ctx, match, "match.feed_state", now)
		healed++
	}
	return healed, nil
}

// EnsureAdmittedFixturesPollable marks every admitted non-terminal Sportmonks match
// as eligible for polling. Finished fixtures drop off livescores and can otherwise
// become ineligible, which leaves matches stuck in reconciling forever.
func (s *Store) EnsureAdmittedFixturesPollable(ctx context.Context, now time.Time) (int64, error) {
	now = now.UTC()
	cursor, err := s.matches.Find(ctx, bson.M{
		"provider": ProviderName,
		"feedState": bson.M{"$ne": matches.FeedStateTerminal},
		"$or": bson.A{
			bson.M{"status": bson.M{"$in": []string{matches.StatusLive, matches.StatusInningsBreak}}},
			bson.M{"feedState": bson.M{"$in": []string{
				matches.FeedStateReconciling, matches.FeedStateStale, matches.FeedStateWarming,
			}}},
		},
	})
	if err != nil {
		return 0, err
	}
	defer cursor.Close(ctx)
	var rows []matches.Match
	if err := cursor.All(ctx, &rows); err != nil {
		return 0, err
	}
	var updated int64
	for _, match := range rows {
		if match.ProviderFixtureID <= 0 {
			continue
		}
		result, err := s.fixtures.UpdateOne(ctx, bson.M{"_id": match.ProviderFixtureID}, bson.M{
			"$set": bson.M{
				"eligible": true, "nextPollAt": now, "updatedAt": now,
			},
			"$setOnInsert": bson.M{
				"leagueId": match.ProviderLeagueID, "seasonId": match.ProviderSeasonID,
				"localTeamId": match.ProviderTeamAID, "visitorTeamId": match.ProviderTeamBID,
				"supported": true, "createdAt": now,
			},
		}, options.Update().SetUpsert(true))
		if err != nil {
			return updated, err
		}
		if result.ModifiedCount > 0 || result.UpsertedCount > 0 {
			updated++
		}
	}
	if completed, err := s.CompleteStuckTerminalMatches(ctx, now); err != nil {
		return updated, err
	} else {
		updated += completed
	}
	return updated, nil
}

// RescheduleStaleTargets forces an immediate poll for eligible fixtures (used on worker boot).
func (s *Store) RescheduleStaleTargets(ctx context.Context, now time.Time) (int64, error) {
	now = now.UTC()
	if admitted, err := s.EnsureAdmittedFixturesPollable(ctx, now); err != nil {
		return 0, err
	} else if admitted > 0 {
		log.Printf("sportmonks ensured %d admitted fixtures pollable", admitted)
	}
	result, err := s.fixtures.UpdateMany(ctx, bson.M{"eligible": true}, bson.M{
		"$set": bson.M{"nextPollAt": now, "updatedAt": now},
	})
	if err != nil {
		return 0, err
	}
	cursor, err := s.matches.Find(ctx, bson.M{
		"provider": ProviderName,
		"feedState": bson.M{"$in": []string{
			matches.FeedStateReconciling, matches.FeedStateStale, matches.FeedStateWarming,
		}},
		"status": bson.M{"$in": []string{matches.StatusLive, matches.StatusInningsBreak}},
	})
	if err != nil {
		return result.ModifiedCount, err
	}
	defer cursor.Close(ctx)
	var stuck []matches.Match
	if err := cursor.All(ctx, &stuck); err != nil {
		return result.ModifiedCount, err
	}
	for _, match := range stuck {
		if match.ProviderFixtureID <= 0 {
			continue
		}
		_, _ = s.fixtures.UpdateOne(ctx, bson.M{"_id": match.ProviderFixtureID}, bson.M{
			"$set": bson.M{"eligible": true, "nextPollAt": now, "updatedAt": now},
		}, options.Update().SetUpsert(true))
	}
	return result.ModifiedCount, nil
}
