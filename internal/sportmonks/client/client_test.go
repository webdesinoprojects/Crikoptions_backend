package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testToken = "very-secret-token"

func newTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client, err := New(Config{APIToken: testToken, BaseURL: server.URL + "/api/v2.0"}, server.Client())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client
}

func assertAuth(t *testing.T, request *http.Request) {
	t.Helper()
	if got := request.URL.Query().Get("api_token"); got != testToken {
		t.Errorf("api_token = %q, want test token", got)
	}
	if strings.Contains(request.Header.Get("User-Agent"), testToken) {
		t.Error("User-Agent leaked token")
	}
}

func TestLeaguesSupportsPaginationAndRateMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request)
		if request.URL.Path != "/api/v2.0/leagues" {
			t.Errorf("path = %q", request.URL.Path)
		}
		query := request.URL.Query()
		if query.Get("page") != "2" || query.Get("include") != "country,seasons" || query.Get("sort") != "name" {
			t.Errorf("unexpected query: %s", request.URL.RawQuery)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("X-RateLimit-Limit", "2000")
		writer.Header().Set("X-RateLimit-Remaining", "1499")
		writer.Header().Set("X-RateLimit-Reset", "30")
		fmt.Fprint(writer, `{"data":[{"resource":"leagues","id":5,"name":"World Cup","future_field":"kept"}],"meta":{"pagination":{"total":3,"count":1,"per_page":1,"current_page":2,"total_pages":3,"links":{"next":"next-url"}}}}`)
	}))
	defer server.Close()

	response, err := newTestClient(t, server).Leagues(context.Background(), PageOptions{
		Page: 2, Includes: []string{"country", " seasons ", "country"}, Sort: "name",
	})
	if err != nil {
		t.Fatalf("Leagues() error = %v", err)
	}
	if len(response.Data) != 1 || response.Data[0].ID != 5 || response.Data[0].Name != "World Cup" {
		t.Fatalf("unexpected data: %+v", response.Data)
	}
	if !strings.Contains(string(response.Data[0].Raw), "future_field") || !strings.Contains(string(response.Raw), "pagination") {
		t.Fatal("raw payload was not preserved")
	}
	if response.Meta.Pagination == nil {
		t.Fatal("pagination was not decoded")
	}
	if next, ok := response.Meta.Pagination.NextPage(); !ok || next != 3 {
		t.Fatalf("NextPage() = %d, %v", next, ok)
	}
	if response.RateLimit.Limit == nil || *response.RateLimit.Limit != 2000 || response.RateLimit.Remaining == nil || *response.RateLimit.Remaining != 1499 {
		t.Fatalf("unexpected rate limit: %+v", response.RateLimit)
	}
	if response.RateLimit.ResetAfter != 30*time.Second {
		t.Fatalf("ResetAfter = %s", response.RateLimit.ResetAfter)
	}
}

func TestScoresDecodesTaxonomy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request)
		if request.URL.Path != "/api/v2.0/scores" {
			t.Errorf("path = %q", request.URL.Path)
		}
		fmt.Fprint(writer, `{"data":[{"id":17,"name":"Leg bye","runs":1,"leg_bye":1,"ball":true}]}`)
	}))
	defer server.Close()

	response, err := newTestClient(t, server).Scores(context.Background())
	if err != nil {
		t.Fatalf("Scores() error = %v", err)
	}
	if len(response.Data) != 1 || response.Data[0].ID != 17 || response.Data[0].LegBye != 1 || !response.Data[0].Ball {
		t.Fatalf("unexpected score data: %+v", response.Data)
	}
}

func TestFixtureEndpointsBuildDocumentedQueries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertAuth(t, request)
		query := request.URL.Query()
		switch request.URL.Path {
		case "/api/v2.0/livescores":
			if query.Get("filter[status]") != "1st Innings" || query.Get("page") != "3" || query.Get("include") != "balls,runs" {
				t.Errorf("unexpected livescores query: %s", request.URL.RawQuery)
			}
			fmt.Fprint(writer, `{"data":[{"id":41,"league_id":7,"season_id":8,"stage_id":9,"localteam_id":10,"visitorteam_id":11,"starting_at":"2026-07-16T10:00:00Z","type":"T20","status":"1st Innings"}]}`)
		case "/api/v2.0/fixtures/41":
			if query.Get("include") != "balls,runs,scoreboards,batting,bowling" {
				t.Errorf("unexpected fixture query: %s", request.URL.RawQuery)
			}
			fmt.Fprint(writer, `{"data":{"id":41,"league_id":7,"season_id":8,"stage_id":9,"localteam_id":10,"visitorteam_id":11,"starting_at":"2026-07-16T10:00:00Z","type":"T20","status":"1st Innings","balls":{"data":[{"id":99}]}}}`)
		case "/api/v2.0/fixtures":
			if query.Get("filter[starts_between]") != "2026-07-14,2026-07-30" || query.Get("filter[league_id]") != "7" || query.Get("filter[status]") != "NS" || query.Get("page") != "2" || query.Get("sort") != "starting_at" {
				t.Errorf("unexpected fixtures query: %s", request.URL.RawQuery)
			}
			fmt.Fprint(writer, `{"data":[],"meta":{"pagination":{"current_page":2,"total_pages":2}}}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server)

	live, err := client.LiveScores(context.Background(), LiveScoresOptions{Page: 3, Status: "1st Innings", Includes: []string{"balls", "runs"}})
	if err != nil || len(live.Data) != 1 || live.Data[0].ID != 41 {
		t.Fatalf("LiveScores() = %+v, %v", live.Data, err)
	}

	fixture, err := client.FixtureByID(context.Background(), 41, FixtureOptions{Includes: []string{"balls", "runs", "scoreboards", "batting", "bowling"}})
	if err != nil {
		t.Fatalf("FixtureByID() error = %v", err)
	}
	balls, err := DecodeRelation[[]struct {
		ID int64 `json:"id"`
	}](fixture.Data.Balls)
	if err != nil || len(balls) != 1 || balls[0].ID != 99 {
		t.Fatalf("DecodeRelation() = %+v, %v", balls, err)
	}

	fixtures, err := client.Fixtures(context.Background(), FixturesOptions{
		From: time.Date(2026, 7, 14, 23, 30, 0, 0, time.FixedZone("IST", 5*60*60+30*60)),
		To:   time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC), LeagueID: 7, Status: "NS", Page: 2, Sort: "starting_at",
	})
	if err != nil || fixtures.Meta.Pagination == nil || fixtures.Meta.Pagination.CurrentPage != 2 {
		t.Fatalf("Fixtures() = %+v, %v", fixtures, err)
	}
}

func TestRateLimitErrorIsTypedAndRedacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Retry-After", "12")
		writer.Header().Set("X-RateLimit-Limit", "2000")
		writer.Header().Set("X-RateLimit-Remaining", "0")
		writer.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(writer, `{"message":"limit for api_token=%s"}`, testToken)
	}))
	defer server.Close()

	_, err := newTestClient(t, server).LiveScores(context.Background(), LiveScoresOptions{})
	var rateError *RateLimitError
	if !errors.As(err, &rateError) {
		t.Fatalf("error = %T %v, want *RateLimitError", err, err)
	}
	if rateError.RetryAfter != 12*time.Second || rateError.RateLimit.Remaining == nil || *rateError.RateLimit.Remaining != 0 {
		t.Fatalf("unexpected rate error: %+v", rateError)
	}
	if strings.Contains(err.Error(), testToken) || strings.Contains(rateError.Message, testToken) {
		t.Fatalf("error leaked token: %v / %q", err, rateError.Message)
	}
}

func TestHTTPAndTransportErrorsDoNotLeakToken(t *testing.T) {
	t.Run("HTTP error body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(writer, `{"error":{"code":"provider_failure","message":"request api_token=%s failed"}}`, testToken)
		}))
		defer server.Close()

		_, err := newTestClient(t, server).Scores(context.Background())
		var httpError *HTTPError
		if !errors.As(err, &httpError) || httpError.StatusCode != http.StatusInternalServerError {
			t.Fatalf("error = %T %v", err, err)
		}
		if strings.Contains(err.Error(), testToken) || strings.Contains(httpError.Message, testToken) {
			t.Fatalf("HTTP error leaked token: %v", err)
		}
	})

	t.Run("transport error URL", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		client := newTestClient(t, server)
		server.Close()

		_, err := client.Scores(context.Background())
		if err == nil {
			t.Fatal("Scores() error = nil")
		}
		if strings.Contains(err.Error(), testToken) {
			t.Fatalf("transport error leaked token: %v", err)
		}
	})

	t.Run("redirect", func(t *testing.T) {
		redirectReached := make(chan struct{}, 1)
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			redirectReached <- struct{}{}
		}))
		defer target.Close()
		source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			http.Redirect(writer, request, target.URL, http.StatusFound)
		}))
		defer source.Close()

		_, err := newTestClient(t, source).Scores(context.Background())
		if err == nil || strings.Contains(err.Error(), testToken) {
			t.Fatalf("redirect error = %v", err)
		}
		select {
		case <-redirectReached:
			t.Fatal("client followed provider redirect")
		default:
		}
	})
}

func TestRequestHonorsContextCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()
	client := newTestClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := client.Scores(ctx)
	<-started
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %T %v, want context deadline", err, err)
	}
}

func TestInvalidInputsFailBeforeRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("unexpected HTTP request")
	}))
	defer server.Close()
	client := newTestClient(t, server)

	if _, err := client.FixtureByID(context.Background(), 0, FixtureOptions{}); err == nil {
		t.Error("FixtureByID() accepted zero ID")
	}
	if _, err := client.Fixtures(context.Background(), FixturesOptions{From: time.Now(), To: time.Now().Add(-time.Hour)}); err == nil {
		t.Error("Fixtures() accepted reversed date window")
	}
	if _, err := client.Leagues(context.Background(), PageOptions{Page: -1}); err == nil {
		t.Error("Leagues() accepted negative page")
	}
}
