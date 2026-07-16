package worker

import (
	"sync"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/client"
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
