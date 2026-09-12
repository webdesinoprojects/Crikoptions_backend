package worker

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
)

type quotaWindow struct {
	mu      sync.Mutex
	limit   int
	reserve int
	states  map[string]*quotaState
}

type quotaState struct {
	requests     []time.Time
	blockedUntil time.Time
}

func newQuotaWindow(hourlyLimit, reservePercent int) *quotaWindow {
	usable := hourlyLimit * (100 - reservePercent) / 100
	if usable < 1 {
		usable = 1
	}
	return &quotaWindow{limit: usable, reserve: reservePercent, states: make(map[string]*quotaState)}
}

func (q *quotaWindow) take(endpoint string, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	state := q.state(endpoint)
	state.prune(now)
	if now.Before(state.blockedUntil) || len(state.requests) >= q.limit {
		return false
	}
	state.requests = append(state.requests, now)
	return true
}

func (q *quotaWindow) observe(endpoint string, now time.Time, rate client.RateLimit) {
	q.mu.Lock()
	defer q.mu.Unlock()
	state := q.state(endpoint)
	if rate.RetryAfter > 0 {
		state.blockedUntil = now.Add(rate.RetryAfter)
	}
	if rate.Remaining != nil && rate.Limit != nil && *rate.Limit > 0 {
		reserveCount := *rate.Limit * q.reserve / 100
		if *rate.Remaining <= reserveCount {
			switch {
			case rate.ResetAt != nil:
				state.blockedUntil = rate.ResetAt.UTC()
			case rate.ResetAfter > 0:
				state.blockedUntil = now.Add(rate.ResetAfter)
			default:
				state.blockedUntil = now.Add(time.Minute)
			}
		}
	}
}

func (q *quotaWindow) state(endpoint string) *quotaState {
	state := q.states[endpoint]
	if state == nil {
		state = &quotaState{}
		q.states[endpoint] = state
	}
	return state
}

func (q *quotaState) prune(now time.Time) {
	cutoff := now.Add(-time.Hour)
	first := 0
	for first < len(q.requests) && q.requests[first].Before(cutoff) {
		first++
	}
	if first > 0 {
		q.requests = append(q.requests[:0], q.requests[first:]...)
	}
}

// providerBreaker halts every outbound CricLive request while the provider is
// refusing us as a whole. Per-fixture backoff cannot do this job: with ~50
// fixtures each retrying on its own timer, a provider-level 429 or 401 turned
// into ~1,500 billed-and-rejected requests an hour — the retry storm was
// spending the daily allowance faster than anything that actually worked.
type providerBreaker struct {
	mu           sync.Mutex
	blockedUntil time.Time
	reason       string
}

// open reports whether calls are currently suspended.
func (b *providerBreaker) open(now time.Time) (bool, string, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Before(b.blockedUntil) {
		return true, b.reason, b.blockedUntil
	}
	return false, "", time.Time{}
}

// trip extends the suspension. It reports whether this call lengthened it, so
// the caller logs once per incident rather than once per fixture.
func (b *providerBreaker) trip(until time.Time, reason string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !until.After(b.blockedUntil) {
		return false
	}
	b.blockedUntil = until
	b.reason = reason
	return true
}

// credentialRejectedBackoff is how long to stand down after a 401/403. The
// token will not fix itself; this only stops the bleeding until an operator
// replaces it and restarts.
const credentialRejectedBackoff = 30 * time.Minute

// providerOutage classifies an error into how long the provider should be left
// alone, or reports that it is an ordinary per-fixture failure.
func providerOutage(err error, now time.Time) (until time.Time, reason string, ok bool) {
	var limited *client.RateLimitError
	if errors.As(err, &limited) {
		if limited.RetryAfter > 0 {
			return now.Add(limited.RetryAfter), "rate limited", true
		}
		// CricLive sends no Retry-After: a bare 429 means the day's allowance
		// is spent, and it comes back at the UTC day boundary.
		return nextUTCDay(now), "daily allowance exhausted (HTTP 429)", true
	}
	var httpErr *client.HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return now.Add(credentialRejectedBackoff),
				fmt.Sprintf("credentials rejected (HTTP %d) — check CRICLIVE_API_TOKEN", httpErr.StatusCode), true
		}
	}
	return time.Time{}, "", false
}

func nextUTCDay(now time.Time) time.Time {
	return now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
}
