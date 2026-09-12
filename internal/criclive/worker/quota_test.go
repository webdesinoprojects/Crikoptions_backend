package worker

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/store"
)

// A bare 429 from CricLive carries no Retry-After and means the day's
// allowance is gone. Retrying it every two minutes across fifty fixtures is
// how a spent quota stayed spent; it must suspend everything until the UTC day
// turns.
func TestProviderOutageClassifiesDailyExhaustion(t *testing.T) {
	now := time.Date(2026, 9, 12, 7, 30, 0, 0, time.UTC)
	until, reason, ok := providerOutage(&client.RateLimitError{Endpoint: "/cricket/commentary"}, now)
	if !ok {
		t.Fatal("bare 429 was not treated as a provider outage")
	}
	if want := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC); !until.Equal(want) {
		t.Fatalf("until = %s, want next UTC day %s", until, want)
	}
	if reason == "" {
		t.Fatal("outage reason is empty")
	}
}

func TestProviderOutageHonoursRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 12, 7, 30, 0, 0, time.UTC)
	until, _, ok := providerOutage(&client.RateLimitError{RetryAfter: 45 * time.Second}, now)
	if !ok || !until.Equal(now.Add(45*time.Second)) {
		t.Fatalf("until = %s ok=%v, want now+45s", until, ok)
	}
}

func TestProviderOutageStandsDownOnRejectedCredentials(t *testing.T) {
	now := time.Date(2026, 9, 12, 7, 30, 0, 0, time.UTC)
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		until, _, ok := providerOutage(&client.HTTPError{StatusCode: code}, now)
		if !ok || !until.Equal(now.Add(credentialRejectedBackoff)) {
			t.Fatalf("HTTP %d: until=%s ok=%v", code, until, ok)
		}
	}
	// A 500 or a timeout is a per-fixture problem, not a provider-wide one.
	if _, _, ok := providerOutage(&client.HTTPError{StatusCode: 500}, now); ok {
		t.Fatal("HTTP 500 was treated as a provider outage")
	}
	if _, _, ok := providerOutage(errors.New("dial tcp: i/o timeout"), now); ok {
		t.Fatal("transport error was treated as a provider outage")
	}
}

func TestProviderBreakerBlocksAndLogsOncePerIncident(t *testing.T) {
	now := time.Date(2026, 9, 12, 7, 30, 0, 0, time.UTC)
	var b providerBreaker
	if open, _, _ := b.open(now); open {
		t.Fatal("new breaker should be closed")
	}
	if !b.trip(now.Add(time.Hour), "first") {
		t.Fatal("first trip should extend the suspension")
	}
	if open, reason, _ := b.open(now); !open || reason != "first" {
		t.Fatalf("breaker open=%v reason=%q", open, reason)
	}
	// A shorter or equal suspension does not re-log the same incident.
	if b.trip(now.Add(30*time.Minute), "second") {
		t.Fatal("shorter trip should not report as new")
	}
	if !b.trip(now.Add(2*time.Hour), "longer") {
		t.Fatal("longer trip should extend")
	}
	if open, _, _ := b.open(now.Add(3 * time.Hour)); open {
		t.Fatal("breaker should clear after the suspension")
	}
}

// The single gate every request passes must refuse while suspended — and
// must not touch any quota counter doing so.
func TestTakeProviderQuotaRefusesWhileBreakerOpen(t *testing.T) {
	now := time.Date(2026, 9, 12, 7, 30, 0, 0, time.UTC)
	w := &Worker{
		cfg:    client.Config{HourlyRequestLimit: 900, DailyRequestLimit: 5000, QuotaReservePercent: 20},
		quota:  newQuotaWindow(900, 20),
		logger: discardLogger{},
		now:    func() time.Time { return now },
	}
	w.breaker.trip(now.Add(time.Hour), "test")
	if w.takeProviderQuotaN(nil, client.EndpointLive, 1) {
		t.Fatal("request allowed while provider suspended")
	}
	if len(w.quota.state(client.EndpointLive).requests) != 0 {
		t.Fatal("a refused request consumed in-memory quota")
	}
}

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

// pathProbeProvider records which endpoint a poll spent.
type pathProbeProvider struct{ overs, commentary int }

func (p *pathProbeProvider) LiveScores(context.Context) (client.LiveResponse, client.RateLimit, error) {
	return client.LiveResponse{}, client.RateLimit{}, nil
}
func (p *pathProbeProvider) Schedule(context.Context) (client.ScheduleResponse, client.RateLimit, error) {
	return client.ScheduleResponse{}, client.RateLimit{}, nil
}
func (p *pathProbeProvider) Commentary(context.Context, int64) (client.CommentaryResponse, client.RateLimit, error) {
	p.commentary++
	return client.CommentaryResponse{}, client.RateLimit{}, nil
}
func (p *pathProbeProvider) Overs(context.Context, int64) (client.OversResponse, client.RateLimit, error) {
	p.overs++
	return client.OversResponse{}, client.RateLimit{}, nil
}

// A poll spends exactly one request: the ball feed while play is on, a single
// commentary read otherwise. Fetching both on every tick doubled live spend.
func TestFetchSnapshotSpendsOneEndpointByState(t *testing.T) {
	probe := &pathProbeProvider{}
	w := &Worker{provider: probe, quota: newQuotaWindow(900, 20), logger: discardLogger{}, now: time.Now}

	if _, _, err := w.fetchSnapshot(context.Background(), store.FixtureTarget{ID: 1, ProviderStatus: client.StateInProgress}); err != nil {
		t.Fatalf("live fetch: %v", err)
	}
	if probe.overs != 1 || probe.commentary != 0 {
		t.Fatalf("live poll spent overs=%d commentary=%d, want 1/0", probe.overs, probe.commentary)
	}

	probe.overs, probe.commentary = 0, 0
	if _, _, err := w.fetchSnapshot(context.Background(), store.FixtureTarget{ID: 2, ProviderStatus: client.StatePreview}); err != nil {
		t.Fatalf("preview fetch: %v", err)
	}
	if probe.overs != 0 || probe.commentary != 1 {
		t.Fatalf("preview poll spent overs=%d commentary=%d, want 0/1", probe.overs, probe.commentary)
	}

	// A stored display phrase must never be mistaken for a state; it falls to
	// the cheap confirmation read rather than starting a ball-feed loop.
	if pollEndpointFor(store.FixtureTarget{ProviderStatus: "RSA need 45 runs in 42 balls"}) != client.EndpointCommentary {
		t.Fatal("display phrase classified as live")
	}
	if pollEndpointFor(store.FixtureTarget{ProviderStatus: client.StateInnings}) != client.EndpointOvers {
		t.Fatal("innings break should stay on the ball feed")
	}
}
