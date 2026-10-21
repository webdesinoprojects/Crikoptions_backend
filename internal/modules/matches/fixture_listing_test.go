package matches

import (
	"context"
	"reflect"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func duplicateListingFixtures(now time.Time) (Match, Match) {
	oldID, _ := primitive.ObjectIDFromHex("6ad1b318d38ede6bc2a05d80")
	newID, _ := primitive.ObjectIDFromHex("6ad62798694df1ec23388bb7")
	old := Match{
		ID: oldID, Provider: DataSourceCricLive, DataSource: DataSourceCricLive,
		ProviderFixtureID: 1000000089396376, ProviderLeagueID: 1000000000012988,
		ProviderSeasonID: 1000000000012988,
		ProviderTeamAID:  1000000000001249, ProviderTeamBID: 1000000000001250,
		TeamAName: "Northern Cape Heat", TeamBName: "Garden Route Badgers",
		Status: StatusUpcoming, FeedState: FeedStateWarming, TradingState: "blocked",
		Format: "T20", StartTime: now.Add(time.Hour), BallsLeft: 120,
		CreatedAt: now.Add(-5 * 24 * time.Hour), UpdatedAt: now.Add(-2 * 24 * time.Hour),
	}
	current := old
	current.ID, current.ProviderFixtureID = newID, 1000000000497134
	current.CreatedAt, current.UpdatedAt = now.Add(-2*24*time.Hour), now
	return old, current
}

func TestUpcomingFixtureAliasesAppearOnceInBothLists(t *testing.T) {
	now := time.Now().UTC()
	old, current := duplicateListingFixtures(now)
	for _, input := range [][]Match{{old, current}, {current, old}} {
		before := append([]Match(nil), input...)
		repo := &MemoryRepository{matches: input}
		svc := NewService(repo, NewMemoryEventRepository(), nil)
		for name, list := range map[string][]Match{
			"home":     svc.GetHomeMatches(context.Background()),
			"upcoming": svc.GetUpcomingMatches(context.Background()),
		} {
			if len(list) != 1 || list[0].ID != current.ID {
				t.Fatalf("%s list=%+v, want the refreshed upcoming fixture only", name, list)
			}
			if list[0].Tradable {
				t.Fatalf("%s upcoming fixture became tradable", name)
			}
		}
		if !reflect.DeepEqual(repo.matches, before) {
			t.Fatal("listing changed stored matches")
		}
		for _, id := range []primitive.ObjectID{old.ID, current.ID} {
			match, err := svc.GetMatchByID(context.Background(), id.Hex())
			if err != nil || match == nil || match.ID != id {
				t.Fatalf("direct match/history access changed for %s: %v", id.Hex(), err)
			}
		}
	}
}

func TestLiveAliasReplacesUpcomingRegardlessOfWhichKeyGoesLive(t *testing.T) {
	for _, state := range []string{StatusLive, StatusInningsBreak} {
		for _, oldGoesLive := range []bool{false, true} {
			now := time.Now().UTC()
			old, current := duplicateListingFixtures(now)
			live := &current
			if oldGoesLive {
				live = &old
			}
			live.Status, live.FeedState = state, FeedStateHealthy
			live.LastSuccessfulPollAt = &now
			svc := NewService(&MemoryRepository{matches: []Match{old, current}}, NewMemoryEventRepository(), nil)
			if upcoming := svc.GetUpcomingMatches(context.Background()); len(upcoming) != 0 {
				t.Fatalf("upcoming alias survived %s transition: %+v", state, upcoming)
			}
			if home := svc.GetHomeMatches(context.Background()); len(home) != 1 || home[0].ID != live.ID {
				t.Fatalf("home did not follow live key %d: %+v", live.ProviderFixtureID, home)
			}
		}
	}
}

func TestUpcomingAliasIdentityDoesNotMergeDistinctFixtures(t *testing.T) {
	for name, change := range map[string]func(*Match){
		"different kickoff":                  func(m *Match) { m.StartTime = m.StartTime.Add(time.Minute) },
		"different league":                   func(m *Match) { m.ProviderLeagueID++ },
		"different season":                   func(m *Match) { m.ProviderSeasonID++ },
		"different team IDs with same names": func(m *Match) { m.ProviderTeamAID++ },
		"different format":                   func(m *Match) { m.Format = "ODI" },
		"different provider":                 func(m *Match) { m.Provider = "other" },
		"missing league":                     func(m *Match) { m.ProviderLeagueID = 0 },
		"missing team":                       func(m *Match) { m.ProviderTeamAID = 0 },
		"missing kickoff":                    func(m *Match) { m.StartTime = time.Time{} },
		"missing provider":                   func(m *Match) { m.Provider = "" },
		"missing fixture":                    func(m *Match) { m.ProviderFixtureID = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now().UTC()
			old, current := duplicateListingFixtures(now)
			change(&current)
			if got := deduplicateUpcomingFixtures([]Match{old, current}, now); len(got) != 2 {
				t.Fatalf("distinct/incomplete identities were merged: %+v", got)
			}
		})
	}
}

func TestUpcomingAliasMatchingAllowsReversedTeams(t *testing.T) {
	now := time.Now().UTC()
	old, current := duplicateListingFixtures(now)
	current.ProviderTeamAID, current.ProviderTeamBID = current.ProviderTeamBID, current.ProviderTeamAID
	current.StartTime = current.StartTime.In(time.FixedZone("IST", 19800))
	current.Format = " t20 "
	if got := deduplicateUpcomingFixtures([]Match{old, current}, now); len(got) != 1 || got[0].ID != current.ID {
		t.Fatalf("same fixture with reversed teams/time zone was not deduplicated: %+v", got)
	}
}

func TestUpcomingAliasPrefersObservedFixtureAndStableTies(t *testing.T) {
	now := time.Now().UTC()
	old, current := duplicateListingFixtures(now)
	poll := now.Add(-time.Minute)
	old.LastSuccessfulPollAt = &poll
	if got := deduplicateUpcomingFixtures([]Match{current, old}, now); len(got) != 1 || got[0].ID != old.ID {
		t.Fatal("schedule refresh displaced a successfully observed upcoming fixture")
	}
	old.LastSuccessfulPollAt = nil
	old.CreatedAt, old.UpdatedAt = current.CreatedAt, current.UpdatedAt
	for _, input := range [][]Match{{old, current}, {current, old}} {
		if got := deduplicateUpcomingFixtures(input, now); len(got) != 1 || got[0].ID != old.ID {
			t.Fatal("selection changed with database result order")
		}
	}
}

func TestUpcomingAliasSelectionIgnoresHiddenAndExpiredLiveRecords(t *testing.T) {
	now := time.Now().UTC()
	for _, hidden := range []bool{true, false} {
		old, current := duplicateListingFixtures(now)
		old.Status, old.FeedState = StatusLive, FeedStateStale
		old.Hidden = hidden
		lastPoll := now.Add(-time.Hour)
		old.LastSuccessfulPollAt = &lastPoll
		svc := NewService(&MemoryRepository{matches: []Match{old, current}}, NewMemoryEventRepository(), nil)
		if got := svc.GetUpcomingMatches(context.Background()); len(got) != 1 || got[0].ID != current.ID {
			t.Fatal("hidden or expired live record suppressed visible upcoming fixture")
		}
	}
}

func TestUpcomingAliasDedupLeavesLiveRecordsAndDemoFallbackAlone(t *testing.T) {
	now := time.Now().UTC()
	old, current := duplicateListingFixtures(now)
	old.Status, current.Status = StatusLive, StatusInningsBreak
	old.FeedState, current.FeedState = FeedStateHealthy, FeedStateHealthy
	old.LastSuccessfulPollAt, current.LastSuccessfulPollAt = &now, &now
	svc := NewService(&MemoryRepository{matches: []Match{old, current}}, NewMemoryEventRepository(), nil)
	if got := svc.GetHomeMatches(context.Background()); len(got) != 2 {
		t.Fatal("independent live records were collapsed")
	}
	old.Status = StatusUpcoming
	current.Status, current.DataSource = StatusLive, DataSourceSimulator
	svc = NewService(&MemoryRepository{matches: []Match{old, current}}, NewMemoryEventRepository(), nil)
	if got := svc.GetHomeMatches(context.Background()); len(got) != 1 || got[0].ID != current.ID {
		t.Fatal("demo fallback behavior changed")
	}
	if got := svc.GetUpcomingMatches(context.Background()); len(got) != 1 || got[0].ID != old.ID {
		t.Fatal("demo match suppressed real upcoming fixture")
	}
}
