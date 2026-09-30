package cricketline

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/reconcile"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

const testKey = "crx_test_key"

func newTestProvider(t *testing.T, handler http.HandlerFunc) (*Provider, *int64) {
	t.Helper()
	var requests int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
		if r.Header.Get("X-API-Key") != testKey {
			t.Errorf("request %s without the key header", r.URL.Path)
		}
		if strings.Contains(r.URL.RawQuery, testKey) || r.URL.Query().Has("api_key") {
			t.Errorf("key leaked into the query string: %s", r.URL.RawQuery)
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	provider, err := New(client.Config{CricketLineBaseURL: server.URL, CricketLineAPIKey: testKey}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	provider.now = func() time.Time { return time.Date(2026, 9, 29, 19, 0, 0, 0, time.UTC) }
	return provider, &requests
}

func serveFile(t *testing.T, w http.ResponseWriter, name string) {
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func TestProviderDiscoveryUsesScheduleIdentity(t *testing.T) {
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/schedule/range":
			if r.URL.Query().Get("start") != "2026-09-28" || r.URL.Query().Get("days") != "17" {
				t.Errorf("schedule query = %s", r.URL.RawQuery)
			}
			serveFile(t, w, "schedule_range.json")
		case "/api/live":
			serveFile(t, w, "live_list_misaligned.json")
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	schedule, _, err := provider.Schedule(ctx)
	if err != nil || len(schedule.Data) == 0 {
		t.Fatalf("Schedule() = %d days, %v", len(schedule.Data), err)
	}
	live, _, err := provider.LiveScores(ctx)
	if err != nil {
		t.Fatal(err)
	}
	match14F7, _ := EncodeKey("14F7")
	jg, _ := EncodeKey("JG")
	e6, _ := EncodeKey("6E")
	var found *client.MatchItem
	for i := range live.Data {
		if live.Data[i].MatchID == match14F7 {
			found = &live.Data[i]
		}
	}
	if found == nil {
		t.Fatalf("14F7 missing from discovery: %+v", live.Data)
	}
	// /api/live printed Rocks' id beside Knights' short name; identity comes
	// from the schedule instead.
	if found.FirstTeam.ID != jg || found.FirstTeam.Name != "Rocks" || found.SecondTeam.ID != e6 || found.SecondTeam.Name != "KNG" {
		t.Fatalf("teams = %+v / %+v", found.FirstTeam, found.SecondTeam)
	}
	if found.State != client.StateInProgress || found.StartTime.IsZero() {
		t.Fatalf("state %q start %s", found.State, found.StartTime)
	}
	fixture := client.FixtureFromMatchItem(*found, provider.now())
	if fixture.LocalTeamImageURL == "" || !strings.HasPrefix(fixture.LocalTeamImageURL, "https://img.cricketlineapi.com/") {
		t.Fatalf("team image = %q", fixture.LocalTeamImageURL)
	}
}

func TestProviderSnapshotReducesInOneRequest(t *testing.T) {
	provider, requests := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/match/14F7/live" {
			http.NotFound(w, r)
			return
		}
		serveFile(t, w, "match_live_14F7.json")
	})
	fixture := testFixture(t, "14F7", "2MT", "T20", knights, rocks)
	snapshot, _, err := provider.MatchSnapshot(context.Background(), fixture)
	if err != nil {
		t.Fatal(err)
	}
	if *requests != 1 {
		t.Fatalf("snapshot cost %d requests, want 1", *requests)
	}
	projection, err := reconcile.ReduceSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Status != matches.StatusLive || projection.CurrentScore != 148 {
		t.Fatalf("projection = %s %d", projection.Status, projection.CurrentScore)
	}
}

func TestProviderClassifiesErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		check  func(error) bool
	}{
		{"plan restricted", http.StatusForbidden,
			`{"status":"error","code":"plan_endpoint_restricted","message":"This plan can access ODI and T20 matches, series, and match info only."}`,
			client.IsPlanRestricted},
		{"bad key", http.StatusUnauthorized,
			`{"detail":{"status":"error","message":"Invalid or missing API key. Pass X-API-Key header or ?api_key= param."}}`,
			func(err error) bool {
				var httpErr *client.HTTPError
				return errors.As(err, &httpErr) && httpErr.StatusCode == 401 && !client.IsPlanRestricted(err) &&
					strings.Contains(httpErr.Message, "Invalid or missing API key")
			}},
		{"quota", http.StatusTooManyRequests, `{"status":"error","message":"quota exceeded"}`,
			func(err error) bool { var limited *client.RateLimitError; return errors.As(err, &limited) }},
		{"error in a 200", http.StatusOK, `{"status":"error","code":"not_found","message":"match not found"}`,
			func(err error) bool {
				var httpErr *client.HTTPError
				return errors.As(err, &httpErr) && httpErr.Code == "not_found"
			}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			})
			_, _, err := provider.MatchSnapshot(context.Background(), testFixture(t, "14F7", "2MT", "T20", knights, rocks))
			if err == nil || !test.check(err) {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Fatalf("error leaks the key: %v", err)
			}
		})
	}
}

func TestProviderRefusesForeignFixtureWithoutARequest(t *testing.T) {
	provider, requests := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL.Path)
	})
	_, _, err := provider.MatchSnapshot(context.Background(), client.Fixture{ID: 110234})
	if !errors.Is(err, client.ErrForeignFixture) || *requests != 0 {
		t.Fatalf("err = %v after %d requests", err, *requests)
	}
}

func TestProviderListsFixturesAboutToStart(t *testing.T) {
	provider, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/schedule/range":
			serveFile(t, w, "schedule_range.json")
		case "/api/live":
			_, _ = w.Write([]byte(`{"status":"success","count":0,"matches":[]}`))
		}
	})
	if _, _, err := provider.Schedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 14ND starts 2026-09-30 05:00 UTC.
	provider.now = func() time.Time { return time.Date(2026, 9, 30, 4, 45, 0, 0, time.UTC) }
	live, _, err := provider.LiveScores(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	match14ND, _ := EncodeKey("14ND")
	for _, item := range live.Data {
		if item.MatchID == match14ND {
			if item.State != client.StatePreview {
				t.Fatalf("14ND state = %q, want Preview", item.State)
			}
			return
		}
	}
	t.Fatalf("14ND not listed in the warm-up window: %+v", live.Data)
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
