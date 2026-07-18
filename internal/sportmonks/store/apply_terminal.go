package store

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/reconcile"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

func applyTerminalMatchState(match *matches.Match, providerStatus string) bool {
	if match == nil {
		return false
	}
	localStatus := reconcile.NormalizeProviderStatus(providerStatus)
	if localStatus != matches.StatusCompleted && localStatus != matches.StatusAbandoned {
		return false
	}
	match.Status = localStatus
	match.ProviderPhase = strings.TrimSpace(providerStatus)
	match.FeedState = matches.FeedStateTerminal
	match.TradingState = "closed"
	match.TradingBlockers = providerBlockers(match.TradingBlockers, "not_live")
	match.LiveContext = nil
	match.MatchPulse = nil
	match.ThisOver = nil
	match.ProviderReconcilePolls = 0
	match.HealthySnapshotCount = 0
	match.FinalCandidate = nil
	return true
}

// ApplyProviderTerminalClosure marks a provider fixture as completed/abandoned when
// Sportmonks reports a terminal phase but delivery reconciliation cannot run.
func (s *Store) ApplyProviderTerminalClosure(
	ctx context.Context,
	fixtureID int64,
	providerStatus string,
	receivedAt time.Time,
	cfg ApplyOptions,
) (bool, error) {
	if fixtureID <= 0 || !reconcile.IsTerminalProviderStatus(providerStatus) {
		return false, nil
	}
	receivedAt = receivedAt.UTC()

	session, err := s.db.Client().StartSession()
	if err != nil {
		return false, err
	}
	defer session.EndSession(ctx)

	closed := false
	_, err = session.WithTransaction(ctx, func(sessionContext mongo.SessionContext) (any, error) {
		if strings.TrimSpace(cfg.LeaseOwner) != "" && strings.TrimSpace(cfg.LeaseToken) != "" {
			if err := s.touchFixtureLease(sessionContext, fixtureID, receivedAt, cfg); err != nil {
				return nil, err
			}
		}
		var match matches.Match
		err := s.matches.FindOne(sessionContext, bson.M{
			"provider": ProviderName, "providerFixtureId": fixtureID,
		}).Decode(&match)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return false, nil
		}
		if err != nil {
			return nil, err
		}
		if match.FeedState == matches.FeedStateTerminal &&
			reconcile.NormalizeProviderStatus(match.ProviderPhase) == reconcile.NormalizeProviderStatus(providerStatus) {
			return false, nil
		}
		if err := s.resetPendingFinalization(sessionContext, &match, receivedAt); err != nil {
			return nil, err
		}
		previousVersion := match.StateVersion
		previousTradingVersion := match.TradingVersion
		match.StateVersion++
		if !applyTerminalMatchState(&match, providerStatus) {
			return false, nil
		}
		if match.TradingVersion == previousTradingVersion {
			match.TradingVersion++
		}
		match.LastFeedReceivedAt = timePointer(receivedAt)
		match.LastSuccessfulPollAt = timePointer(receivedAt)
		match.UpdatedAt = receivedAt
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
		if err := s.insertMarketSnapshots(sessionContext, match, receivedAt); err != nil {
			return nil, err
		}
		if err := s.enqueueSettlementJobs(sessionContext, match, receivedAt); err != nil {
			return nil, err
		}
		if match.Status == matches.StatusAbandoned {
			if err := s.enqueueVoidJobs(sessionContext, match, receivedAt); err != nil {
				return nil, err
			}
		}
		if err := s.enqueueTradingGateJob(sessionContext, match, receivedAt); err != nil {
			return nil, err
		}
		if err := s.insertMatchOutbox(sessionContext, match, "match.state", receivedAt); err != nil {
			return nil, err
		}
		closed = true
		log.Printf(
			"sportmonks fixture %d match %s: closed terminal provider status=%q",
			fixtureID, match.ID.Hex(), providerStatus,
		)
		return true, nil
	}, options.Transaction().
		SetReadConcern(readconcern.Snapshot()).
		SetWriteConcern(writeconcern.Majority()))
	if err != nil {
		if errors.Is(err, ErrFixtureLeaseLost) || errors.Is(err, ErrConcurrentApply) {
			return false, nil
		}
		return false, err
	}
	return closed, nil
}

// RepairUpcomingUnsupportedMatches upgrades NS Sportmonks matches that were
// incorrectly marked unsupported (for example ODI fixtures without structured
// overs before kickoff) so they appear on the upcoming home feed.
func (s *Store) RepairUpcomingUnsupportedMatches(ctx context.Context, now time.Time) (int64, error) {
	now = now.UTC()
	cursor, err := s.matches.Find(ctx, bson.M{
		"provider":  ProviderName,
		"status":    matches.StatusUpcoming,
		"feedState": matches.FeedStateUnsupported,
		"hidden":    bson.M{"$ne": true},
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
		format, scheduledBalls, err := reconcile.ClassifyFormat(match.Format)
		if err != nil {
			continue
		}
		result, err := s.matches.UpdateOne(ctx, bson.M{
			"_id": match.ID, "status": matches.StatusUpcoming, "feedState": matches.FeedStateUnsupported,
		}, bson.M{"$set": bson.M{
			"format": format, "scheduledBalls": scheduledBalls, "ballsLeft": scheduledBalls,
			"oversText": "0.0", "feedState": matches.FeedStateWarming,
			"tradingState": "blocked", "tradingBlockers": []string{"not_live"},
			"updatedAt": now,
		}})
		if err != nil {
			return updated, err
		}
		if result.ModifiedCount > 0 {
			updated++
		}
	}
	if updated > 0 {
		log.Printf("sportmonks repaired %d unsupported upcoming matches", updated)
	}
	return updated, nil
}

// CompleteStuckTerminalMatches promotes admitted matches that Sportmonks already
// finished but still appear live locally (for example after reducer failures).
func (s *Store) CompleteStuckTerminalMatches(ctx context.Context, now time.Time) (int64, error) {
	now = now.UTC()
	cursor, err := s.matches.Find(ctx, bson.M{
		"provider":  ProviderName,
		"feedState": bson.M{"$ne": matches.FeedStateTerminal},
		"status": bson.M{"$in": []string{
			matches.StatusLive, matches.StatusInningsBreak,
		}},
		"providerPhase": bson.M{"$in": []string{
			"Finished", "Aban.", "Cancl.", "finished", "Abandoned", "Cancelled",
		}},
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
		if match.ProviderFixtureID <= 0 || !reconcile.IsTerminalProviderStatus(match.ProviderPhase) {
			continue
		}
		closed, err := s.ApplyProviderTerminalClosure(ctx, match.ProviderFixtureID, match.ProviderPhase, now, ApplyOptions{Mode: "live"})
		if err != nil {
			return updated, err
		}
		if closed {
			updated++
		}
	}
	if updated > 0 {
		log.Printf("sportmonks completed %d stuck terminal matches", updated)
	}
	return updated, nil
}
