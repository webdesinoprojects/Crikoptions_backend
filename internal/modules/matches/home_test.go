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

func TestGetHomeMatches_DemoFallbackWhenNoRealLive(t *testing.T) {
	now := time.Now().UTC()
	repo := NewMemoryRepository()
	svc := NewService(repo, NewMemoryEventRepository(), nil)

	cskID, _ := primitive.ObjectIDFromHex("0000000000000000000000aa")
	rcbID, _ := primitive.ObjectIDFromHex("0000000000000000000000bb")
	hiddenDemo := primitive.NewObjectID()
	upcomingProvider := primitive.NewObjectID()

	repo.matches = []Match{
		// Revealed demo fallback games (visible because no real match is live).
		{
			ID: cskID, DataSource: DataSourceManual,
			TeamAName: "CSK", TeamBName: "MI", Status: StatusLive,
			Format: "T20", BallsLeft: 42, CreatedAt: now, UpdatedAt: now, StartTime: now.Add(-30 * time.Minute),
		},
		{
			ID: rcbID, DataSource: DataSourceManual,
			TeamAName: "RCB", TeamBName: "KKR", Status: StatusLive,
			Format: "T20", BallsLeft: 78, CreatedAt: now, UpdatedAt: now, StartTime: now.Add(-20 * time.Minute),
		},
		// A demo match left hidden must never surface.
		{
			ID: hiddenDemo, DataSource: DataSourceSimulator, Hidden: true,
			TeamAName: "DC", TeamBName: "SRH", Status: StatusLive,
			Format: "T20", BallsLeft: 60, CreatedAt: now, UpdatedAt: now,
		},
		// Upcoming provider match should be outranked by the live demo fallback.
		{
			ID: upcomingProvider, DataSource: DataSourceSportmonks,
			TeamAName: "ENG", TeamBName: "IND", Status: StatusUpcoming,
			Format: "ODI", BallsLeft: BallsODI, CreatedAt: now, UpdatedAt: now,
			StartTime: now.Add(2 * time.Hour),
		},
	}

	home := svc.GetHomeMatches(context.Background())
	if len(home) != 2 {
		t.Fatalf("expected 2 demo fallback matches, got %d", len(home))
	}
	for _, m := range home {
		if m.DataSource == DataSourceSportmonks {
			t.Fatalf("provider upcoming match should not appear while demo fallback is live")
		}
		if !m.Tradable {
			t.Fatalf("demo fallback match %s should be tradable", m.TeamAName)
		}
	}
}

func TestGetHomeMatches_RealLiveHidesDemoFallback(t *testing.T) {
	now := time.Now().UTC()
	repo := NewMemoryRepository()
	svc := NewService(repo, NewMemoryEventRepository(), nil)

	cskID, _ := primitive.ObjectIDFromHex("0000000000000000000000aa")
	providerLive := primitive.NewObjectID()

	repo.matches = []Match{
		{
			ID: cskID, DataSource: DataSourceManual,
			TeamAName: "CSK", TeamBName: "MI", Status: StatusLive,
			Format: "T20", BallsLeft: 42, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: providerLive, DataSource: DataSourceSportmonks,
			TeamAName: "SL", TeamBName: "PAK", Status: StatusLive,
			Format: "ODI", BallsLeft: 120, CreatedAt: now, UpdatedAt: now, StartTime: now,
		},
	}

	home := svc.GetHomeMatches(context.Background())
	if len(home) != 1 || home[0].ID != providerLive {
		t.Fatalf("expected only the real provider match, got %d matches", len(home))
	}
}

func TestCountLiveProviderMatchesAndSetHidden(t *testing.T) {
	now := time.Now().UTC()
	repo := NewMemoryRepository()

	cskID, _ := primitive.ObjectIDFromHex("0000000000000000000000aa")
	providerLive := primitive.NewObjectID()
	providerBreak := primitive.NewObjectID()

	repo.matches = []Match{
		{ID: cskID, DataSource: DataSourceManual, Status: StatusLive, UpdatedAt: now},
		{ID: providerLive, DataSource: DataSourceSportmonks, Status: StatusLive, UpdatedAt: now},
		{ID: providerBreak, DataSource: DataSourceSportmonks, Status: StatusInningsBreak, UpdatedAt: now},
	}

	ctx := context.Background()
	if n, err := repo.CountLiveProviderMatches(ctx); err != nil || n != 2 {
		t.Fatalf("CountLiveProviderMatches = %d, err=%v; want 2", n, err)
	}

	// Hiding the live provider match drops the in-play count; the demo match is
	// never counted as a provider match.
	if err := repo.SetHidden(ctx, true, providerLive); err != nil {
		t.Fatalf("SetHidden: %v", err)
	}
	if n, _ := repo.CountLiveProviderMatches(ctx); n != 1 {
		t.Fatalf("after hiding provider live, count = %d; want 1", n)
	}
}

func TestProviderMatchImminent(t *testing.T) {
	now := time.Now().UTC()
	lead := 30 * time.Minute

	cases := []struct {
		name  string
		match Match
		want  bool
	}{
		{
			name:  "provider live",
			match: Match{DataSource: DataSourceSportmonks, Status: StatusLive, StartTime: now.Add(-time.Hour)},
			want:  true,
		},
		{
			name:  "provider innings break",
			match: Match{DataSource: DataSourceSportmonks, Status: StatusInningsBreak, StartTime: now.Add(-time.Hour)},
			want:  true,
		},
		{
			name:  "provider upcoming within 30m",
			match: Match{DataSource: DataSourceSportmonks, Status: StatusUpcoming, StartTime: now.Add(25 * time.Minute)},
			want:  true,
		},
		{
			name:  "provider upcoming beyond 30m",
			match: Match{DataSource: DataSourceSportmonks, Status: StatusUpcoming, StartTime: now.Add(90 * time.Minute)},
			want:  false,
		},
		{
			name:  "hidden provider upcoming soon does not count",
			match: Match{DataSource: DataSourceSportmonks, Status: StatusUpcoming, StartTime: now.Add(5 * time.Minute), Hidden: true},
			want:  false,
		},
		{
			name:  "demo match starting soon never triggers",
			match: Match{DataSource: DataSourceManual, Status: StatusUpcoming, StartTime: now.Add(5 * time.Minute)},
			want:  false,
		},
		{
			name:  "live demo match never triggers",
			match: Match{DataSource: DataSourceSimulator, Status: StatusLive, StartTime: now.Add(-time.Hour)},
			want:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := NewMemoryRepository()
			tc.match.ID = primitive.NewObjectID()
			repo.matches = []Match{tc.match}
			svc := NewService(repo, NewMemoryEventRepository(), nil)

			got, err := svc.ProviderMatchImminent(context.Background(), lead)
			if err != nil {
				t.Fatalf("ProviderMatchImminent: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ProviderMatchImminent = %v, want %v", got, tc.want)
			}
		})
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
