package store

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/reconcile"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
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
// CricLive reports a terminal phase but delivery reconciliation cannot run.
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
			"criclive fixture %d match %s: closed terminal provider status=%q",
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

// RepairUpcomingUnsupportedMatches upgrades NS CricLive matches that were
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
		log.Printf("criclive repaired %d unsupported upcoming matches", updated)
	}
	return updated, nil
}

// CompleteStuckTerminalMatches promotes admitted matches that CricLive already
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
		log.Printf("criclive completed %d stuck terminal matches", updated)
	}
	return updated, nil
}

// ClosePhaseTerminalMatches closes provider matches that are still presented as
// in play while the provider's own phase text says the match is over.
//
// The other reapers only catch a *dead* feed. This is the opposite shape: the
// feed stays healthy and keeps reporting a terminal phrase, but the status
// never flips — for instance when the adapter's in-memory abandonment
// confirmation (cricketline.Provider.confirmAbandon) never latches, so every
// poll re-emits the held "Rain" state and the card sits on the home feed
// indefinitely. Nothing else closes these.
//
// Two conditions must both hold, because providerPhase is raw provider text and
// not a canonical state: the phrase must be one that states the match was
// called off (reconcile.IsConfirmedAbandonmentPhase, which is anchored and
// ignores review lines), and it must have stood unchanged for stuckAfter. The
// second is the real guard — a live ticker shows an abandonment-looking line
// for seconds, never for half an hour.
//
// Abandonment only. This reaper voids, so a phrase claiming the match merely
// finished is left for reconciliation to settle; voiding a genuine result on
// phase text alone would be worse than leaving the card up.
func (s *Store) ClosePhaseTerminalMatches(ctx context.Context, now time.Time, stuckAfter time.Duration) (int64, error) {
	now = now.UTC()
	if stuckAfter <= 0 {
		stuckAfter = 30 * time.Minute
	}
	phaseCutoff := now.Add(-stuckAfter)

	// providerPhaseSince is absent on matches written before the field existed,
	// and a $lte date predicate does not match a missing field. Those are
	// stamped by their next poll and become eligible one stuckAfter later,
	// which is the conservative way round.
	cursor, err := s.matches.Find(ctx, bson.M{
		"provider":  ProviderName,
		"feedState": bson.M{"$ne": matches.FeedStateTerminal},
		"status": bson.M{"$in": []string{
			matches.StatusLive, matches.StatusInningsBreak,
		}},
		"providerPhaseSince": bson.M{"$lte": phaseCutoff},
	})
	if err != nil {
		return 0, err
	}
	defer cursor.Close(ctx)

	var rows []matches.Match
	if err := cursor.All(ctx, &rows); err != nil {
		return 0, err
	}

	var closed int64
	for i := range rows {
		match := rows[i]
		if match.ProviderFixtureID <= 0 || !reconcile.IsConfirmedAbandonmentPhase(match.ProviderPhase) {
			continue
		}
		ok, err := s.closeStuckPhaseMatch(ctx, match.ID, match.ProviderPhase, now, stuckAfter)
		if err != nil {
			log.Printf(
				"criclive stuck-phase reaper: fixture %d match %s phase %q: %v",
				match.ProviderFixtureID, match.ID.Hex(), match.ProviderPhase, err,
			)
			continue
		}
		if ok {
			closed++
		}
	}
	if closed > 0 {
		log.Printf("criclive closed %d matches whose provider phase stayed terminal for %s", closed, stuckAfter)
	}
	return closed, nil
}

// closeStuckPhaseMatch transitions one match whose provider phase has been
// terminal for too long.
//
// It mirrors abandonDeadMatch rather than reusing ApplyProviderTerminalClosure:
// that path neither freezes the abandonment dispositions nor parks the fixture,
// and it acts on a phase string read outside the transaction.
func (s *Store) closeStuckPhaseMatch(ctx context.Context, id primitive.ObjectID, phase string, now time.Time, stuckAfter time.Duration) (bool, error) {
	session, err := s.db.Client().StartSession()
	if err != nil {
		return false, err
	}
	defer session.EndSession(ctx)

	closed := false
	_, err = session.WithTransaction(ctx, func(sessionContext mongo.SessionContext) (any, error) {
		var match matches.Match
		err := s.matches.FindOne(sessionContext, bson.M{"_id": id}).Decode(&match)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return false, nil
		}
		if err != nil {
			return nil, err
		}
		// Every reason for closing is re-proved inside the transaction. The
		// scan ran outside it and without the fixture lease, so a poll may have
		// moved the match on in the meantime: stateVersion would catch a lost
		// write, but not a phase that has since stopped being terminal.
		if match.FeedState == matches.FeedStateTerminal {
			return false, nil
		}
		switch match.Status {
		case matches.StatusLive, matches.StatusInningsBreak:
		default:
			return false, nil
		}
		if strings.TrimSpace(match.ProviderPhase) != strings.TrimSpace(phase) {
			return false, nil
		}
		if !reconcile.IsConfirmedAbandonmentPhase(match.ProviderPhase) {
			return false, nil
		}
		if match.ProviderPhaseSince == nil || match.ProviderPhaseSince.After(now.Add(-stuckAfter)) {
			return false, nil
		}
		// The matcher admits abandonment phrases only; this checks that the
		// looser status normalisation agrees before anything is written.
		if reconcile.NormalizeProviderStatus(match.ProviderPhase) != matches.StatusAbandoned {
			return false, nil
		}

		if err := s.resetPendingFinalization(sessionContext, &match, now); err != nil {
			return nil, err
		}
		previousVersion := match.StateVersion
		previousTradingVersion := match.TradingVersion
		match.StateVersion++

		current := match
		match.Status = matches.StatusAbandoned
		match.FeedState = matches.FeedStateTerminal
		match.TradingState = "closed"
		match.TradingBlockers = providerBlockers(match.TradingBlockers, "not_live")
		match.LiveContext = nil
		match.MatchPulse = nil
		match.ThisOver = nil
		match.ProviderReconcilePolls = 0
		match.HealthySnapshotCount = 0
		match.FinalCandidate = nil
		// resetPendingFinalization above has already cleared the pending
		// candidates, so this keeps a settle disposition only where an earlier
		// pass had recorded one. In practice that means every open market on
		// this match is voided and refunded, which is the intended outcome for
		// an abandonment and matches what the dead-feed reaper does.
		freezeAbandonmentDispositions(current, &match)
		if match.TradingVersion == previousTradingVersion {
			match.TradingVersion++
		}
		match.UpdatedAt = now

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
		if err := s.insertMarketSnapshots(sessionContext, match, now); err != nil {
			return nil, err
		}
		if err := s.enqueueSettlementJobs(sessionContext, match, now); err != nil {
			return nil, err
		}
		if err := s.enqueueVoidJobs(sessionContext, match, now); err != nil {
			return nil, err
		}
		if err := s.enqueueTradingGateJob(sessionContext, match, now); err != nil {
			return nil, err
		}
		if err := s.insertMatchOutbox(sessionContext, match, "match.state", now); err != nil {
			return nil, err
		}
		// The match is closed, so its target must stop costing requests. The
		// feed here is healthy, so without this discovery keeps it armed.
		if match.ProviderFixtureID > 0 {
			if _, err := s.fixtures.UpdateOne(sessionContext, bson.M{"_id": match.ProviderFixtureID},
				parkUpdate(now, ParkReasonTerminalPhase)); err != nil {
				return nil, err
			}
		}
		closed = true
		log.Printf(
			"criclive match %s (fixture %d): abandoned — provider phase %q unchanged since %s",
			match.ID.Hex(), match.ProviderFixtureID, match.ProviderPhase,
			current.ProviderPhaseSince.UTC().Format(time.RFC3339),
		)
		return true, nil
	}, options.Transaction().
		SetReadConcern(readconcern.Snapshot()).
		SetWriteConcern(writeconcern.Majority()))
	if err != nil {
		if errors.Is(err, ErrConcurrentApply) || errors.Is(err, ErrSettlementInFlight) {
			return false, nil
		}
		return false, err
	}
	return closed, nil
}

// AbandonDeadLiveMatches closes provider matches that still claim to be in play
// but whose feed died without CricLive ever reporting a terminal phase.
//
// CompleteStuckTerminalMatches cannot reach these: it keys off providerPhase,
// which only a successful poll can set. When polling stops first (fixture drops
// off livescores, quota exhausted, key expired, league disabled) the match is
// orphaned at live/innings_break forever.
//
// Such a match is closed as ABANDONED, not completed: we never saw a result, so
// settling on a frozen scoreboard would be wrong. Abandonment freezes the
// dispositions — innings that genuinely finalised still settle, everything else
// voids and refunds.
func (s *Store) AbandonDeadLiveMatches(ctx context.Context, now time.Time, deadAfter, maxDuration time.Duration) (int64, error) {
	now = now.UTC()
	if deadAfter <= 0 {
		deadAfter = 2 * time.Hour
	}
	if maxDuration <= 0 {
		maxDuration = matches.DeadLiveMatchAfter
	}
	pollCutoff := now.Add(-deadAfter)
	startCutoff := now.Add(-maxDuration)

	cursor, err := s.matches.Find(ctx, bson.M{
		"provider":  ProviderName,
		"feedState": bson.M{"$ne": matches.FeedStateTerminal},
		"status": bson.M{"$in": []string{
			matches.StatusLive, matches.StatusInningsBreak,
		}},
		// Both guards must hold: a long-running match with a healthy feed is fine,
		// and a brief outage on a match that started an hour ago is fine.
		"startTime": bson.M{"$lte": startCutoff},
		"$or": bson.A{
			bson.M{"lastSuccessfulPollAt": bson.M{"$lte": pollCutoff}},
			bson.M{"lastSuccessfulPollAt": bson.M{"$exists": false}},
			bson.M{"lastSuccessfulPollAt": nil},
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
		closed, err := s.abandonDeadMatch(ctx, match.ID, now)
		if err != nil {
			return updated, err
		}
		if closed {
			updated++
		}
	}
	if updated > 0 {
		log.Printf("criclive abandoned %d dead live matches (no poll for %s)", updated, deadAfter)
	}
	return updated, nil
}

// abandonDeadMatch transitions one orphaned match to abandoned and enqueues the
// resulting settlement/void work, mirroring ApplyProviderTerminalClosure but
// driven by our own wall clock rather than a provider phase.
func (s *Store) abandonDeadMatch(ctx context.Context, id primitive.ObjectID, now time.Time) (bool, error) {
	session, err := s.db.Client().StartSession()
	if err != nil {
		return false, err
	}
	defer session.EndSession(ctx)

	closed := false
	_, err = session.WithTransaction(ctx, func(sessionContext mongo.SessionContext) (any, error) {
		var match matches.Match
		err := s.matches.FindOne(sessionContext, bson.M{"_id": id}).Decode(&match)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return false, nil
		}
		if err != nil {
			return nil, err
		}
		if match.FeedState == matches.FeedStateTerminal {
			return false, nil
		}
		switch match.Status {
		case matches.StatusLive, matches.StatusInningsBreak:
		default:
			return false, nil
		}
		if err := s.resetPendingFinalization(sessionContext, &match, now); err != nil {
			return nil, err
		}
		previousVersion := match.StateVersion
		previousTradingVersion := match.TradingVersion
		match.StateVersion++

		current := match
		match.Status = matches.StatusAbandoned
		match.FeedState = matches.FeedStateTerminal
		match.TradingState = "closed"
		match.TradingBlockers = providerBlockers(match.TradingBlockers, "not_live", "feed_dead")
		match.LiveContext = nil
		match.MatchPulse = nil
		match.ThisOver = nil
		match.ProviderReconcilePolls = 0
		match.HealthySnapshotCount = 0
		match.FinalCandidate = nil
		// Settle only innings that genuinely finalised; void the rest.
		freezeAbandonmentDispositions(current, &match)
		if match.TradingVersion == previousTradingVersion {
			match.TradingVersion++
		}
		match.UpdatedAt = now

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
		if err := s.insertMarketSnapshots(sessionContext, match, now); err != nil {
			return nil, err
		}
		if err := s.enqueueSettlementJobs(sessionContext, match, now); err != nil {
			return nil, err
		}
		if err := s.enqueueVoidJobs(sessionContext, match, now); err != nil {
			return nil, err
		}
		if err := s.enqueueTradingGateJob(sessionContext, match, now); err != nil {
			return nil, err
		}
		if err := s.insertMatchOutbox(sessionContext, match, "match.state", now); err != nil {
			return nil, err
		}
		// The match is closed; its target must stop costing requests too.
		// Without this the fixture kept its live status and was re-armed by
		// the next discovery pass.
		if match.ProviderFixtureID > 0 {
			if _, err := s.fixtures.UpdateOne(sessionContext, bson.M{"_id": match.ProviderFixtureID},
				parkUpdate(now, ParkReasonAbandoned)); err != nil {
				return nil, err
			}
		}
		closed = true
		log.Printf(
			"criclive match %s (fixture %d): abandoned — feed dead since %s, no terminal provider phase",
			match.ID.Hex(), match.ProviderFixtureID, lastPollLabel(current),
		)
		return true, nil
	}, options.Transaction().
		SetReadConcern(readconcern.Snapshot()).
		SetWriteConcern(writeconcern.Majority()))
	if err != nil {
		if errors.Is(err, ErrConcurrentApply) || errors.Is(err, ErrSettlementInFlight) {
			return false, nil
		}
		return false, err
	}
	return closed, nil
}

func lastPollLabel(match matches.Match) string {
	if match.LastSuccessfulPollAt == nil {
		return "never"
	}
	return match.LastSuccessfulPollAt.UTC().Format(time.RFC3339)
}
