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
	if liveContextReady(projection.LiveContext) {
		match.FeedState = matches.FeedStateHealthy
		match.TradingBlockers = providerBlockers(match.TradingBlockers)
		if len(match.TradingBlockers) == 0 {
			match.TradingState = "open"
		} else {
			match.TradingState = "blocked"
		}
		return
	}
	// Match/innings ended at provider — leave reconciling for a finished fixture with no batters.
	switch projection.Status {
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
	previousTradingVersion := current.TradingVersion
	previousFeedState := current.FeedState
	current.ProviderReconcilePolls++
	current.StateVersion++
	if previousFeedState != matches.FeedStateReconciling || current.TradingState == "open" {
		current.TradingVersion++
	}

	applyProjectionOverlay(&current, projection, receivedAt, cfg.FeedValidity)
	if current.FeedState == matches.FeedStateTerminal {
		// Terminal provider phase applied during overlay.
	} else if liveContextReady(projection.LiveContext) {
		current.FeedState = matches.FeedStateHealthy
		current.TradingBlockers = providerBlockers(current.TradingBlockers)
		if len(current.TradingBlockers) == 0 {
			current.TradingState = "open"
		} else {
			current.TradingState = "blocked"
		}
	} else if projection.Status == matches.StatusCompleted || projection.Status == matches.StatusAbandoned {
		current.FeedState = matches.FeedStateFinalizing
		current.TradingState = "closed"
		current.TradingBlockers = providerBlockers(current.TradingBlockers, "finalizing")
	} else if projection.Status == matches.StatusInningsBreak {
		current.FeedState = matches.FeedStateHealthy
		current.TradingState = "blocked"
		current.TradingBlockers = providerBlockers(current.TradingBlockers, "innings_break")
	} else {
		current.FeedState = matches.FeedStateReconciling
		current.HealthySnapshotCount = 0
		current.TradingState = "blocked"
		current.TradingBlockers = providerBlockers(current.TradingBlockers, "reconciling")
	}
	needsCancellation := current.TradingVersion != previousTradingVersion
	if needsCancellation {
		current.TradingBlockers = appendUnique(current.TradingBlockers, "cancellation_pending")
	}
	current.UpdatedAt = receivedAt

	log.Printf(
		"sportmonks fixture %d match %s: reconciling poll %d reason=%s feed=%s liveContext=%t",
		projection.FixtureID, current.ID.Hex(), current.ProviderReconcilePolls, reason,
		current.FeedState, liveContextReady(projection.LiveContext),
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
