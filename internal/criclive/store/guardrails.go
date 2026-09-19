package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Polling guardrails.
//
// On 19 Sep 2026 the whole 5,000-request daily allowance was spent by 06:45
// UTC on four fixtures that had finished days earlier. CricLive had stopped
// listing them on /cricket/live without ever reporting a terminal state, so
// their stored providerStatus stayed "In Progress" and each was re-read every
// 15 seconds (the finalizing cadence) around the clock. Nothing bounded how
// long, or how often, a fixture could be polled once it was believed live.
//
// Every guard below is a hard bound that does not depend on the provider
// behaving: a fixture is parked when its scheduled start is too far in the
// past for the match to still be running, when it has cost more requests than
// its format could justify, or when the live feed itself has stopped listing
// it. Parking is recorded as parkedReason so no other code path re-arms it.

// ErrQuotaReserved is returned when the internal request guard refuses a poll.
// It lives here rather than in the worker so FailTargetPoll can tell a request
// that was refused (costs nothing) from one that was spent.
var ErrQuotaReserved = errors.New("CricLive quota reserve reached")

const (
	// MaxLiveWindowT20 and MaxLiveWindowODI bound how long after its
	// scheduled start a fixture may still be polled at live cadence. A T20 is
	// ~3.5h and an ODI ~8h; the windows leave room for a rain delay and a
	// late start but not for a match that never ended. The ODI window equals
	// matches.DeadLiveMatchAfter so the target and the public match age out
	// together.
	MaxLiveWindowT20 = 8 * time.Hour
	MaxLiveWindowODI = matches.DeadLiveMatchAfter

	// MaxRequestsT20 and MaxRequestsODI cap the provider requests one fixture
	// may cost over its lifetime. They are set close to what a full match
	// costs, with a fifth in hand for extras, a super over or a short delay:
	//
	//   T20: ~255 deliveries over ~3.5h at the 6s live cadence ≈ 2,100
	//   ODI: ~630 deliveries over ~7.75h at the 10s ODI cadence ≈ 2,850
	//        (an ODI is never read at 6s — see worker.formatPollInterval —
	//        because at 6s one ODI alone costs ~4,650 of a 5,000 day)
	//
	// A rain break costs almost nothing against these: the quiet-feed damper
	// drops an unchanged scoreboard to one read a minute after five minutes.
	// A fixture past its figure is being re-read for nothing and is parked;
	// this is the backstop the start-time window cannot give, since it holds
	// even when the schedule reported the wrong start. An operator resync
	// resets the count for the rare match that genuinely needs more.
	MaxRequestsT20 = 2500
	MaxRequestsODI = 3600

	// AbsentProbeInterval is how often a fixture that /cricket/live has
	// stopped listing is re-read. One shared discovery request already
	// reports every match in play, so a fixture missing from it is almost
	// certainly over; a slow probe keeps a provider hiccup recoverable
	// without paying the live cadence for a match nobody is playing.
	AbsentProbeInterval = 5 * time.Minute
)

// Reasons a target is parked. A parked target is skipped by discovery,
// dispatch and the boot-time reschedule until the reason is cleared: by the
// live feed listing it again (absent_from_live_feed only) or by an operator
// resync.
const (
	ParkReasonLiveWindow     = "live_window_exceeded"
	ParkReasonRequestBudget  = "request_budget_exceeded"
	ParkReasonAbsentFromLive = "absent_from_live_feed"
	ParkReasonAbandoned      = "match_abandoned"
)

// MaxLiveWindow is the longest a fixture of the given format may be polled at
// live cadence after its scheduled start.
func MaxLiveWindow(format string) time.Duration {
	if strings.EqualFold(strings.TrimSpace(format), "T20") {
		return MaxLiveWindowT20
	}
	return MaxLiveWindowODI
}

// MaxRequestsPerFixture is the lifetime request budget of one fixture.
func MaxRequestsPerFixture(format string) int {
	if strings.EqualFold(strings.TrimSpace(format), "T20") {
		return MaxRequestsT20
	}
	return MaxRequestsODI
}

// liveWindowAnchor is the moment the live window is measured from: the
// scheduled start, or the first live sighting if that came later (a delayed
// start). A fixture never seen live is judged on its schedule alone.
func liveWindowAnchor(start time.Time, liveSince *time.Time) time.Time {
	anchor := start.UTC()
	if liveSince != nil && !liveSince.IsZero() && liveSince.UTC().After(anchor) {
		anchor = liveSince.UTC()
	}
	return anchor
}

// liveWindowExceededAt reports whether a fixture of the given format, with
// the given schedule and first live sighting, can no longer be in play.
func liveWindowExceededAt(format string, start time.Time, liveSince *time.Time, now time.Time) bool {
	anchor := liveWindowAnchor(start, liveSince)
	if anchor.IsZero() {
		return false
	}
	return now.UTC().Sub(anchor) > MaxLiveWindow(format)
}

// LiveWindowExceeded reports whether the fixture has been "live" for longer
// than its format allows.
func LiveWindowExceeded(target FixtureTarget, now time.Time) bool {
	return liveWindowExceededAt(target.Format, target.StartTime, target.LiveSince, now)
}

// RequestBudgetExceeded reports whether the fixture has cost more provider
// requests than its format could justify.
func RequestBudgetExceeded(target FixtureTarget) bool {
	return target.RequestCount >= MaxRequestsPerFixture(target.Format)
}

// OverrunReason is the park reason a target has earned, or "" if none. Only
// fixtures believed live are judged: a not-started fixture is already parked
// by the pre-match schedule and a finished one by the terminal recheck.
func OverrunReason(target FixtureTarget, now time.Time) string {
	if target.ParkedReason != "" || !client.IsLiveState(target.ProviderStatus) {
		return ""
	}
	switch {
	case LiveWindowExceeded(target, now):
		return ParkReasonLiveWindow
	case RequestBudgetExceeded(target):
		return ParkReasonRequestBudget
	}
	return ""
}

func parkUpdate(now time.Time, reason string) bson.M {
	return bson.M{
		"$set": bson.M{
			"nextPollAt": now.UTC().Add(TerminalFixtureRecheck), "parkedReason": reason,
			"lastError": reason, "updatedAt": now.UTC(),
		},
		"$unset": bson.M{"leaseOwner": "", "leaseToken": "", "leaseUntil": ""},
	}
}

// armUnparkedTarget makes a fixture pollable now unless it is parked, creating
// the target from insertOnly when it does not exist. It is two statements
// rather than one upsert because a filter that excludes parked targets would
// make an upsert try to insert a duplicate _id for one that is parked.
func (s *Store) armUnparkedTarget(ctx context.Context, fixtureID int64, now time.Time, insertOnly bson.M) (bool, error) {
	now = now.UTC()
	result, err := s.fixtures.UpdateOne(ctx, bson.M{
		"_id": fixtureID, "parkedReason": bson.M{"$exists": false},
	}, bson.M{"$set": bson.M{"eligible": true, "nextPollAt": now, "updatedAt": now}})
	if err != nil {
		return false, err
	}
	if result.MatchedCount > 0 {
		return result.ModifiedCount > 0, nil
	}
	// Either missing or parked: $setOnInsert leaves a parked target alone.
	fields := bson.M{"eligible": true, "nextPollAt": now, "updatedAt": now}
	for key, value := range insertOnly {
		fields[key] = value
	}
	result, err = s.fixtures.UpdateOne(ctx, bson.M{"_id": fixtureID},
		bson.M{"$setOnInsert": fields}, options.Update().SetUpsert(true))
	if err != nil {
		return false, err
	}
	return result.UpsertedCount > 0, nil
}

// ParkTarget stops polling one fixture until an operator resyncs it.
func (s *Store) ParkTarget(ctx context.Context, fixtureID int64, now time.Time, reason string) error {
	if fixtureID <= 0 || strings.TrimSpace(reason) == "" {
		return errors.New("park target requires a fixture id and a reason")
	}
	_, err := s.fixtures.UpdateOne(ctx, bson.M{"_id": fixtureID}, parkUpdate(now, reason))
	return err
}

// ParkOverrunLiveTargets parks every fixture still marked live whose start is
// outside its format's live window or whose request budget is spent. It is a
// bulk sweep for discovery and boot; dispatch applies the same test per
// target so a fixture cannot slip through between sweeps.
func (s *Store) ParkOverrunLiveTargets(ctx context.Context, now time.Time) (int64, error) {
	now = now.UTC()
	live := func() bson.M {
		return bson.M{
			"eligible":       true,
			"providerStatus": bson.M{"$in": client.LiveStates},
			"parkedReason":   bson.M{"$exists": false},
		}
	}
	// Both the schedule and the first live sighting must be outside the
	// window, which is the query form of liveWindowAnchor.
	outsideWindow := func(format bson.M, window time.Duration) bson.M {
		cutoff := now.Add(-window)
		clause := bson.M{
			"startTime": bson.M{"$lte": cutoff},
			"$or": bson.A{
				bson.M{"liveSince": bson.M{"$exists": false}},
				bson.M{"liveSince": bson.M{"$lte": cutoff}},
			},
		}
		for key, value := range format {
			clause[key] = value
		}
		return clause
	}
	window := live()
	window["$or"] = bson.A{
		outsideWindow(bson.M{"format": "T20"}, MaxLiveWindowT20),
		outsideWindow(bson.M{"format": bson.M{"$ne": "T20"}}, MaxLiveWindowODI),
	}
	result, err := s.fixtures.UpdateMany(ctx, window, parkUpdate(now, ParkReasonLiveWindow))
	if err != nil {
		return 0, err
	}
	parked := result.ModifiedCount

	budget := live()
	budget["$or"] = bson.A{
		bson.M{"format": "T20", "requestCount": bson.M{"$gte": MaxRequestsT20}},
		bson.M{"format": bson.M{"$ne": "T20"}, "requestCount": bson.M{"$gte": MaxRequestsODI}},
	}
	result, err = s.fixtures.UpdateMany(ctx, budget, parkUpdate(now, ParkReasonRequestBudget))
	if err != nil {
		return parked, err
	}
	return parked + result.ModifiedCount, nil
}

// MarkLiveTargetsMissing reconciles the stored live fixtures against the ids
// the /cricket/live response just listed. A live fixture the feed no longer
// carries is stamped missingFromLiveSince on its first absence and, once it
// has been absent for `timeout`, demoted to the slow probe cadence. A fixture
// the feed lists again is restored immediately. It returns how many fixtures
// were demoted on this pass.
func (s *Store) MarkLiveTargetsMissing(ctx context.Context, seen []int64, now time.Time, timeout time.Duration) (int64, error) {
	now = now.UTC()
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if seen == nil {
		seen = []int64{}
	}
	live := func() bson.M {
		return bson.M{"eligible": true, "providerStatus": bson.M{"$in": client.LiveStates}}
	}

	// Listed again: forget the absence.
	present := live()
	present["_id"] = bson.M{"$in": seen}
	present["missingFromLiveSince"] = bson.M{"$exists": true}
	if _, err := s.fixtures.UpdateMany(ctx, present, bson.M{
		"$unset": bson.M{"missingFromLiveSince": ""}, "$set": bson.M{"updatedAt": now},
	}); err != nil {
		return 0, err
	}
	restore := live()
	restore["_id"] = bson.M{"$in": seen}
	restore["parkedReason"] = ParkReasonAbsentFromLive
	if _, err := s.fixtures.UpdateMany(ctx, restore, bson.M{
		"$unset": bson.M{"parkedReason": ""},
		"$set":   bson.M{"nextPollAt": now, "lastError": "", "updatedAt": now},
	}); err != nil {
		return 0, err
	}

	// First absence: start the clock.
	absent := live()
	absent["_id"] = bson.M{"$nin": seen}
	absent["missingFromLiveSince"] = bson.M{"$exists": false}
	if _, err := s.fixtures.UpdateMany(ctx, absent, bson.M{
		"$set": bson.M{"missingFromLiveSince": now, "updatedAt": now},
	}); err != nil {
		return 0, err
	}

	// Absent past the timeout: demote to the probe cadence. Targets parked for
	// a harder reason are left on that schedule.
	expired := live()
	expired["_id"] = bson.M{"$nin": seen}
	expired["missingFromLiveSince"] = bson.M{"$lte": now.Add(-timeout)}
	expired["parkedReason"] = bson.M{"$exists": false}
	result, err := s.fixtures.UpdateMany(ctx, expired, bson.M{
		"$set": bson.M{
			"parkedReason": ParkReasonAbsentFromLive, "lastError": ParkReasonAbsentFromLive,
			"updatedAt": now,
		},
		"$max":   bson.M{"nextPollAt": now.Add(AbsentProbeInterval)},
		"$unset": bson.M{"leaseOwner": "", "leaseToken": "", "leaseUntil": ""},
	})
	if err != nil {
		return 0, err
	}
	return result.ModifiedCount, nil
}

// DailyQuotaUsed reports how many provider requests the shared day bucket has
// recorded for the UTC day containing now.
func (s *Store) DailyQuotaUsed(ctx context.Context, now time.Time) (int, error) {
	day := now.UTC().Truncate(24 * time.Hour)
	var row struct {
		Count int `bson:"count"`
	}
	err := s.quota.FindOne(ctx, bson.M{"_id": "day:" + day.Format("20060102")}).Decode(&row)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return row.Count, nil
}
