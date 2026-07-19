package realtime

import (
	"context"
	"testing"
	"time"
)

func TestOutboxPayloadAddsMonotonicEnvelope(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	got := outboxPayload(outboxDocument{
		EventID: "evt-1", Type: "match.state", MatchID: "match-1",
		StateVersion: 8, TradingVersion: 3, Sequence: 8, OccurredAt: now,
		Payload: map[string]any{"currentScore": 144},
	})
	if got["eventId"] != "evt-1" || got["stateVersion"] != int64(8) || got["currentScore"] != 144 {
		t.Fatalf("payload = %#v", got)
	}
	got = outboxPayload(outboxDocument{EventID: "outbox-1", Payload: map[string]any{"eventId": "ball-7"}})
	if got["eventId"] != "ball-7" || got["outboxEventId"] != "outbox-1" {
		t.Fatalf("stable delivery identity overwritten: %#v", got)
	}
}

func TestPaceBallUpdateOnlyBetweenDifferentBalls(t *testing.T) {
	ctx := context.Background()
	var lastMatch, lastBall string
	var lastAt time.Time

	first := outboxDocument{
		Type: "match.delivery", MatchID: "m1", EventID: "d1",
		Payload: map[string]any{"eventId": "ball-1"},
	}
	start := time.Now()
	if !paceBallUpdate(ctx, first, &lastMatch, &lastBall, &lastAt) {
		t.Fatal("first ball blocked")
	}
	if time.Since(start) >= ballUpdatePace/2 {
		t.Fatal("first ball should not wait")
	}

	sameBallScore := outboxDocument{
		Type: "match.state", MatchID: "m1",
		EventID: "sportmonks:1:2:3:match.ball:ball-1",
	}
	start = time.Now()
	if !paceBallUpdate(ctx, sameBallScore, &lastMatch, &lastBall, &lastAt) {
		t.Fatal("same-ball score blocked")
	}
	if time.Since(start) >= ballUpdatePace/2 {
		t.Fatal("same ball score tick should not wait")
	}

	nextBall := outboxDocument{
		Type: "match.delivery", MatchID: "m1", EventID: "d2",
		Payload: map[string]any{"eventId": "ball-2"},
	}
	start = time.Now()
	if !paceBallUpdate(ctx, nextBall, &lastMatch, &lastBall, &lastAt) {
		t.Fatal("next ball blocked")
	}
	if time.Since(start) < ballUpdatePace/2 {
		t.Fatal("next ball should pace after previous ball")
	}
}

func TestOutboxWatcherReadinessTracksConnection(t *testing.T) {
	watcher := &OutboxWatcher{}
	if err := watcher.Ready(context.Background()); err == nil {
		t.Fatal("disconnected watcher reported ready")
	}
	watcher.connected.Store(true)
	if err := watcher.Ready(context.Background()); err != nil {
		t.Fatalf("connected watcher readiness: %v", err)
	}
}
