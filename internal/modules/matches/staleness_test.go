package matches

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func timePtr(t time.Time) *time.Time { return &t }

func TestLiveFeedExpired(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name  string
		match Match
		want  bool
	}{
		{
			name: "healthy live match is not expired",
			match: Match{
				DataSource: DataSourceSportmonks, Status: StatusLive,
				LastSuccessfulPollAt: timePtr(now.Add(-5 * time.Second)),
			},
			want: false,
		},
		{
			name: "frozen live feed is expired",
			match: Match{
				DataSource: DataSourceSportmonks, Status: StatusLive,
				LastSuccessfulPollAt: timePtr(now.Add(-3 * time.Hour)),
			},
			want: true,
		},
		{
			name: "frozen innings break is expired",
			match: Match{
				DataSource: DataSourceSportmonks, Status: StatusInningsBreak,
				LastSuccessfulPollAt: timePtr(now.Add(-3 * time.Hour)),
			},
			want: true,
		},
		{
			name: "long innings break with live polling is not expired",
			match: Match{
				DataSource: DataSourceSportmonks, Status: StatusInningsBreak,
				StartTime:            now.Add(-6 * time.Hour),
				LastSuccessfulPollAt: timePtr(now.Add(-10 * time.Second)),
			},
			want: false,
		},
		{
			name: "demo replay never expires",
			match: Match{
				DataSource: DataSourceSimulator, Status: StatusLive,
				LastSuccessfulPollAt: timePtr(now.Add(-3 * time.Hour)),
			},
			want: false,
		},
		{
			name: "upcoming match is not affected",
			match: Match{
				DataSource: DataSourceSportmonks, Status: StatusUpcoming,
				LastSuccessfulPollAt: timePtr(now.Add(-3 * time.Hour)),
			},
			want: false,
		},
		{
			name: "unknown feed clock fails open",
			match: Match{
				DataSource: DataSourceSportmonks, Status: StatusLive,
				StartTime: now.Add(-6 * time.Hour),
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LiveFeedExpired(&tc.match, now); got != tc.want {
				t.Fatalf("LiveFeedExpired = %v, want %v", got, tc.want)
			}
		})
	}
}

// The reported bug: a fixture whose feed died stayed at the top of the home feed
// with a frozen scoreboard, and also suppressed the demo fallback replays.
func TestZombieMatchLeavesHomeFeedAndReleasesFallback(t *testing.T) {
	now := time.Now().UTC()
	zombie := Match{
		ID: primitive.NewObjectID(), DataSource: DataSourceSportmonks,
		Status: StatusInningsBreak, StartTime: now.Add(-30 * time.Hour),
		LastSuccessfulPollAt: timePtr(now.Add(-28 * time.Hour)),
	}
	demo := Match{
		ID: primitive.NewObjectID(), DataSource: DataSourceSimulator,
		Status: StatusLive, StartTime: now.Add(-10 * time.Minute),
	}

	repo := NewMemoryRepository()
	repo.matches = []Match{zombie, demo}
	svc := NewService(repo, NewMemoryEventRepository(), nil)

	home := svc.GetHomeMatches(context.Background())
	for _, m := range home {
		if m.ID == zombie.ID {
			t.Fatalf("dead provider match still on home feed: %+v", m)
		}
	}
	if len(home) != 1 || home[0].ID != demo.ID {
		t.Fatalf("expected demo fallback to surface, got %d matches", len(home))
	}

	imminent, err := svc.ProviderMatchImminent(context.Background(), 30*time.Minute)
	if err != nil {
		t.Fatalf("ProviderMatchImminent: %v", err)
	}
	if imminent {
		t.Fatal("dead provider match must not count as a real fixture in play")
	}
}
