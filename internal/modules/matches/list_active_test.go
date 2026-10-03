package matches

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// listActiveCase is one stored match and whether the home, upcoming and
// fallback views may ever show it.
type listActiveCase struct {
	name   string
	doc    bson.M
	active bool
}

func listActiveCases(now time.Time) []listActiveCase {
	soon, past := now.Add(time.Hour), now.Add(-2*time.Hour)
	doc := func(status string, start time.Time, hidden bool) bson.M {
		return bson.M{"_id": primitive.NewObjectID(), "status": status, "startTime": start, "hidden": hidden, "teamAName": "A", "teamBName": "B"}
	}
	noStatus := bson.M{"_id": primitive.NewObjectID(), "startTime": soon, "teamAName": "A", "teamBName": "B"}
	return []listActiveCase{
		{"live", doc(StatusLive, past, false), true},
		{"LIVE", doc("LIVE", past, false), true},
		{"innings_break", doc(StatusInningsBreak, past, false), true},
		{"innings break", doc("innings break", past, false), true},
		{"suspended", doc(StatusSuspended, past, false), true},
		{"upcoming soon", doc(StatusUpcoming, soon, false), true},
		{"upcoming no start time", doc(StatusUpcoming, time.Time{}, false), true},
		{"blank status soon", doc("", soon, false), true},
		{"missing status soon", noStatus, true},
		{"upcoming within grace", doc(StatusUpcoming, now.Add(-10*time.Minute), false), true},
		{"upcoming long past start", doc(StatusUpcoming, past, false), false},
		{"blank status long past start", doc("", past, false), false},
		{"scheduled long past start", doc("scheduled", past, false), false},
		{"completed", doc(StatusCompleted, past, false), false},
		{"COMPLETED", doc("COMPLETED", past, false), false},
		{"Completed", doc("Completed", past, false), false},
		{"finished", doc("finished", past, false), false},
		{"abandoned", doc(StatusAbandoned, past, false), false},
		{"hidden live", doc(StatusLive, past, true), false},
		{"hidden upcoming", doc(StatusUpcoming, soon, true), false},
	}
}

func TestMemoryListActiveKeepsOnlyShowableMatches(t *testing.T) {
	now := time.Now().UTC()
	repo := &MemoryRepository{}
	want := map[primitive.ObjectID]string{}
	for _, c := range listActiveCases(now) {
		var m Match
		raw, _ := bson.Marshal(c.doc)
		if err := bson.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		repo.matches = append(repo.matches, m)
		if c.active {
			want[m.ID] = c.name
		}
	}
	got, err := repo.ListActive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertSameMatches(t, got, want)
}

// The query must never drop a match the Go filter keeps: the views filter
// again in Go, but they cannot see what the query did not return.
func TestMongoListActiveAgreesWithTheGoFilter(t *testing.T) {
	uri := strings.TrimSpace(os.Getenv("MONGO_INTEGRATION_URI"))
	if uri == "" {
		t.Skip("MONGO_INTEGRATION_URI is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	db := client.Database("matches_it_" + primitive.NewObjectID().Hex())
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		_ = db.Drop(cleanup)
		_ = client.Disconnect(cleanup)
	})

	now := time.Now().UTC()
	want := map[primitive.ObjectID]string{}
	for _, c := range listActiveCases(now) {
		if _, err := db.Collection("matches").InsertOne(ctx, c.doc); err != nil {
			t.Fatal(err)
		}
		if c.active {
			want[c.doc["_id"].(primitive.ObjectID)] = c.name
		}
	}
	repo := NewMongoRepository(db)
	got, err := repo.ListActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertSameMatches(t, got, want)

	// A write through the repository is visible on the next read, not after
	// the shared read expires.
	var hide primitive.ObjectID
	for id, name := range want {
		if name == "live" {
			hide = id
		}
	}
	if err := repo.SetHidden(ctx, true, hide); err != nil {
		t.Fatal(err)
	}
	delete(want, hide)
	got, err = repo.ListActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertSameMatches(t, got, want)

	// Callers own what they are given: changing it must not leak into the
	// shared read.
	got[0].Status = "tampered"
	again, err := repo.ListActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range again {
		if m.Status == "tampered" {
			t.Fatal("a caller's edit leaked into the shared read")
		}
	}
}

func assertSameMatches(t *testing.T, got []Match, want map[primitive.ObjectID]string) {
	t.Helper()
	seen := map[primitive.ObjectID]bool{}
	var extra []string
	for _, m := range got {
		seen[m.ID] = true
		if _, ok := want[m.ID]; !ok {
			extra = append(extra, m.Status)
		}
	}
	var missing []string
	for id, name := range want {
		if !seen[id] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("missing %v, unexpected statuses %v", missing, extra)
	}
}
