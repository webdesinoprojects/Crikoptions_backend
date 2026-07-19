package matches

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestGetHomeMatches_SportmonksLivePreferred(t *testing.T) {
	now := time.Now().UTC()
	repo := NewMemoryRepository()
	svc := NewService(repo, NewMemoryEventRepository(), nil)

	manualID, _ := primitive.ObjectIDFromHex("0000000000000000000000aa")
	sportmonksLiveID := primitive.NewObjectID()
	sportmonksUpcomingID := primitive.NewObjectID()

	repo.matches = []Match{
		{
			ID: manualID, DataSource: DataSourceManual,
			TeamAName: "CSK", TeamBName: "MI", Status: StatusLive,
			Format: "T20", BallsLeft: 42, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: sportmonksLiveID, DataSource: DataSourceSportmonks,
			TeamAName: "WI", TeamBName: "NZ", Status: StatusLive,
			Format: "T20", BallsLeft: 60, CreatedAt: now, UpdatedAt: now, StartTime: now,
		},
		{
			ID: sportmonksUpcomingID, DataSource: DataSourceSportmonks,
			TeamAName: "ENG", TeamBName: "IND", Status: StatusUpcoming,
			Format: "ODI", BallsLeft: BallsODI, CreatedAt: now, UpdatedAt: now,
			StartTime: now.Add(2 * time.Hour),
		},
	}

	home := svc.GetHomeMatches(context.Background())
	if len(home) != 1 {
		t.Fatalf("expected 1 home match, got %d", len(home))
	}
	if home[0].ID != sportmonksLiveID {
		t.Fatalf("expected Sportmonks live match %s, got %s", sportmonksLiveID.Hex(), home[0].ID.Hex())
	}
}

func TestGetHomeMatches_UpcomingWhenNoLive(t *testing.T) {
	now := time.Now().UTC()
	repo := NewMemoryRepository()
	svc := NewService(repo, NewMemoryEventRepository(), nil)

	upcomingSoon := primitive.NewObjectID()
	upcomingLater := primitive.NewObjectID()
	completed := primitive.NewObjectID()

	repo.matches = []Match{
		{
			ID: completed, DataSource: DataSourceSportmonks,
			TeamAName: "WI", TeamBName: "NZ", Status: StatusCompleted,
			Format: "ODI", CreatedAt: now, UpdatedAt: now, StartTime: now.Add(-24 * time.Hour),
		},
		{
			ID: upcomingLater, DataSource: DataSourceSportmonks,
			TeamAName: "AUS", TeamBName: "PAK", Status: StatusUpcoming,
			Format: "ODI", BallsLeft: BallsODI, CreatedAt: now, UpdatedAt: now,
			StartTime: now.Add(48 * time.Hour),
		},
		{
			ID: upcomingSoon, DataSource: DataSourceSportmonks,
			TeamAName: "ENG", TeamBName: "IND", Status: StatusUpcoming,
			Format: "ODI", BallsLeft: BallsODI, CreatedAt: now, UpdatedAt: now,
			StartTime: now.Add(2 * time.Hour),
		},
		{
			ID: primitive.NewObjectID(), DataSource: DataSourceManual,
			TeamAName: "CSK", TeamBName: "MI", Status: StatusUpcoming,
			Format: "T20", CreatedAt: now, UpdatedAt: now, StartTime: now.Add(time.Hour),
		},
	}

	home := svc.GetHomeMatches(context.Background())
	if len(home) != 2 {
		t.Fatalf("expected 2 upcoming Sportmonks matches, got %d", len(home))
	}
	if home[0].ID != upcomingSoon {
		t.Fatalf("expected soonest upcoming first, got %s (%s)", home[0].ID.Hex(), home[0].TeamAName)
	}
	if home[1].ID != upcomingLater {
		t.Fatalf("expected later upcoming second, got %s", home[1].ID.Hex())
	}
}

func TestGetUpcomingMatches_OnlyUpcomingSorted(t *testing.T) {
	now := time.Now().UTC()
	repo := NewMemoryRepository()
	svc := NewService(repo, NewMemoryEventRepository(), nil)

	liveID := primitive.NewObjectID()
	soonID := primitive.NewObjectID()
	laterID := primitive.NewObjectID()

	repo.matches = []Match{
		{
			ID: liveID, DataSource: DataSourceSportmonks,
			TeamAName: "WI", TeamBName: "NZ", Status: StatusLive,
			Format: "T20", BallsLeft: 60, CreatedAt: now, UpdatedAt: now, StartTime: now,
		},
		{
			ID: laterID, DataSource: DataSourceSportmonks,
			TeamAName: "AUS", TeamBName: "PAK", Status: StatusUpcoming,
			Format: "ODI", BallsLeft: BallsODI, CreatedAt: now, UpdatedAt: now,
			StartTime: now.Add(48 * time.Hour),
		},
		{
			ID: soonID, DataSource: DataSourceSportmonks,
			TeamAName: "ENG", TeamBName: "IND", Status: StatusUpcoming,
			Format: "ODI", BallsLeft: BallsODI, CreatedAt: now, UpdatedAt: now,
			StartTime: now.Add(2 * time.Hour),
		},
		{
			ID: primitive.NewObjectID(), DataSource: DataSourceManual,
			TeamAName: "CSK", TeamBName: "MI", Status: StatusUpcoming,
			Format: "T20", CreatedAt: now, UpdatedAt: now, StartTime: now.Add(time.Hour),
		},
	}

	upcoming := svc.GetUpcomingMatches(context.Background())
	if len(upcoming) != 2 {
		t.Fatalf("expected 2 upcoming Sportmonks matches, got %d", len(upcoming))
	}
	if upcoming[0].ID != soonID || upcoming[1].ID != laterID {
		t.Fatalf("order=%s then %s", upcoming[0].TeamAName, upcoming[1].TeamAName)
	}
	for _, match := range upcoming {
		if match.Status != StatusUpcoming {
			t.Fatalf("unexpected status %q", match.Status)
		}
	}
}
