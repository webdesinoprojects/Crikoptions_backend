package store

import (
	"context"
	"errors"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"go.mongodb.org/mongo-driver/bson"
)

// ParkReasonFeedChanged parks a target that belongs to a feed the pipeline no
// longer reads.
const ParkReasonFeedChanged = "feed_changed"

// FixtureIDRange is the half-open [Min, Max) range of provider fixture ids one
// feed owns. Every feed writes under the same provider label and collections,
// so the id range is what tells their fixtures apart.
type FixtureIDRange struct {
	Min int64
	Max int64
}

// RetireForeignFixtures runs at startup, after a feed switch, so the pipeline
// stops acting on fixtures the configured feed cannot read:
//
//   - targets outside the range are parked and made ineligible, so dispatch
//     never spends a request on an id the adapter cannot resolve;
//   - not-started public matches outside the range are hidden, because the new
//     feed publishes the same real-world fixtures under its own ids.
//
// Live and finished matches are left alone. A live one stops updating and is
// closed by the dead-feed reaper, which voids its markets; a finished one is
// history. Targets this feed owns that an earlier switch parked are released,
// so switching back and forth does not strand them.
func (s *Store) RetireForeignFixtures(ctx context.Context, own FixtureIDRange, now time.Time) (parked, hidden int64, err error) {
	if own.Min <= 0 || own.Max <= own.Min {
		return 0, 0, errors.New("retire foreign fixtures requires a non-empty id range")
	}
	now = now.UTC()
	outside := func(field string) bson.A {
		return bson.A{
			bson.M{field: bson.M{"$lt": own.Min}},
			bson.M{field: bson.M{"$gte": own.Max}},
		}
	}

	// Targets already parked for another reason (budget, abandonment) stay as
	// they are: re-parking them as feed_changed would let a switch back
	// release a target that must stay parked.
	park := parkUpdate(now, ParkReasonFeedChanged)
	park["$set"].(bson.M)["eligible"] = false
	result, err := s.fixtures.UpdateMany(ctx, bson.M{
		"parkedReason": bson.M{"$in": bson.A{nil, ""}},
		"$or":          outside("_id"),
	}, park)
	if err != nil {
		return 0, 0, err
	}
	parked = result.ModifiedCount

	// The next discovery or schedule pass recomputes eligibility for these.
	if _, err := s.fixtures.UpdateMany(ctx, bson.M{
		"parkedReason": ParkReasonFeedChanged,
		"_id":          bson.M{"$gte": own.Min, "$lt": own.Max},
	}, bson.M{
		"$set":   bson.M{"nextPollAt": now, "updatedAt": now},
		"$unset": bson.M{"parkedReason": "", "lastError": ""},
	}); err != nil {
		return parked, 0, err
	}

	result, err = s.matches.UpdateMany(ctx, bson.M{
		"provider": ProviderName, "status": matches.StatusUpcoming, "hidden": bson.M{"$ne": true},
		"$or": outside("providerFixtureId"),
	}, bson.M{"$set": bson.M{"hidden": true, "updatedAt": now}})
	if err != nil {
		return parked, 0, err
	}
	return parked, result.ModifiedCount, nil
}

// FixtureTargetsBetween lists the targets in a feed's id range whose start
// falls in [from, to).
func (s *Store) FixtureTargetsBetween(ctx context.Context, own FixtureIDRange, from, to time.Time) ([]FixtureTarget, error) {
	cursor, err := s.fixtures.Find(ctx, bson.M{
		"_id":       bson.M{"$gte": own.Min, "$lt": own.Max},
		"startTime": bson.M{"$gte": from.UTC(), "$lt": to.UTC()},
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var targets []FixtureTarget
	if err := cursor.All(ctx, &targets); err != nil {
		return nil, err
	}
	return targets, nil
}
