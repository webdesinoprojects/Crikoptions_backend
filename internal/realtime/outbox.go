package realtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// OutboxWatcher broadcasts committed provider events from this API replica.
// The change stream is intentionally insert-only: outbox records are immutable.
type OutboxWatcher struct {
	collection *mongo.Collection
	publisher  TopicPublisher
	connected  atomic.Bool
}

type TopicPublisher interface {
	Publish(topic string, data any)
}

func NewOutboxWatcher(db *mongo.Database, publisher TopicPublisher) *OutboxWatcher {
	return &OutboxWatcher{collection: db.Collection("realtime_outbox"), publisher: publisher}
}

func (w *OutboxWatcher) Run(ctx context.Context) error {
	if w == nil || w.collection == nil || w.publisher == nil {
		return errors.New("realtime outbox watcher is not configured")
	}
	defer w.connected.Store(false)

	var resumeToken bson.Raw
	for ctx.Err() == nil {
		watchOptions := options.ChangeStream().SetFullDocument(options.UpdateLookup).SetMaxAwaitTime(10 * time.Second)
		if len(resumeToken) != 0 {
			watchOptions.SetResumeAfter(resumeToken)
		}
		stream, err := w.collection.Watch(ctx, mongo.Pipeline{
			{{Key: "$match", Value: bson.M{"operationType": "insert"}}},
		}, watchOptions)
		if err != nil {
			w.connected.Store(false)
			if !waitOutboxRetry(ctx) {
				return nil
			}
			log.Printf("realtime outbox watch: %v", err)
			continue
		}
		w.connected.Store(true)

		var lastBallMatch string
		var lastBallID string
		var lastBallAt time.Time
		for stream.Next(ctx) {
			var change struct {
				Document outboxDocument `bson:"fullDocument"`
			}
			if err := stream.Decode(&change); err != nil {
				log.Printf("realtime outbox decode: %v", err)
				continue
			}
			doc := change.Document
			if topic := strings.TrimSpace(doc.Topic); topic != "" {
				if paceBallUpdate(ctx, doc, &lastBallMatch, &lastBallID, &lastBallAt) {
					w.publisher.Publish(topic, outboxPayload(doc))
				}
			}
			resumeToken = append(resumeToken[:0], stream.ResumeToken()...)
		}
		err = stream.Err()
		w.connected.Store(false)
		_ = stream.Close(context.Background())
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			log.Printf("realtime outbox stream: %v", err)
		}
		if !waitOutboxRetry(ctx) {
			return nil
		}
	}
	return nil
}

// Ready is a live-mode readiness check. A replica must not receive traffic
// until its change stream is connected, otherwise committed outbox events
// would not reach clients attached to that replica.
func (w *OutboxWatcher) Ready(context.Context) error {
	if w == nil || !w.connected.Load() {
		return errors.New("realtime outbox change stream is not connected")
	}
	return nil
}

// WaitReady provides a bounded startup barrier for live API replicas.
func (w *OutboxWatcher) WaitReady(ctx context.Context) error {
	if w == nil || w.collection == nil || w.publisher == nil {
		return errors.New("realtime outbox watcher is not configured")
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if w.connected.Load() {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for realtime outbox change stream: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

type outboxDocument struct {
	EventID        string         `bson:"eventId"`
	Topic          string         `bson:"topic"`
	Type           string         `bson:"type"`
	MatchID        string         `bson:"matchId"`
	StateVersion   int64          `bson:"stateVersion"`
	TradingVersion int64          `bson:"tradingVersion"`
	Sequence       int64          `bson:"sequence"`
	OccurredAt     time.Time      `bson:"occurredAt"`
	Payload        map[string]any `bson:"payload"`
}

func outboxPayload(document outboxDocument) map[string]any {
	payload := make(map[string]any, len(document.Payload)+8)
	for key, value := range document.Payload {
		payload[key] = value
	}
	if _, exists := payload["eventId"]; !exists {
		payload["eventId"] = document.EventID
	}
	payload["outboxEventId"] = document.EventID
	payload["eventType"] = document.Type
	payload["matchId"] = document.MatchID
	payload["stateVersion"] = document.StateVersion
	payload["tradingVersion"] = document.TradingVersion
	payload["sequence"] = document.Sequence
	payload["timestamp"] = document.OccurredAt.UTC().Format(time.RFC3339Nano)
	return payload
}

// ballUpdatePace spaces catch-up balls so the UI can show one delivery at a time
// when Sportmonks returns several new balls in a single poll.
const ballUpdatePace = 400 * time.Millisecond

func ballUpdateIdentity(doc outboxDocument) (matchID, ballID string, ok bool) {
	matchID = strings.TrimSpace(doc.MatchID)
	switch {
	case doc.Type == "match.delivery":
		if id, _ := doc.Payload["eventId"].(string); strings.TrimSpace(id) != "" {
			return matchID, strings.TrimSpace(id), true
		}
		return matchID, fmt.Sprintf("seq:%d", doc.Sequence), true
	case doc.Type == "match.state" && strings.Contains(doc.EventID, ":match.ball:"):
		parts := strings.Split(doc.EventID, ":match.ball:")
		return matchID, parts[len(parts)-1], true
	default:
		return "", "", false
	}
}

// paceBallUpdate delays the first event of each new ball for the same match.
// Delivery + progressive score for the same ball stay back-to-back.
func paceBallUpdate(ctx context.Context, doc outboxDocument, lastMatch, lastBall *string, lastAt *time.Time) bool {
	matchID, ballID, ok := ballUpdateIdentity(doc)
	if !ok {
		return true
	}
	sameBall := matchID != "" && matchID == *lastMatch && ballID == *lastBall
	if !sameBall && matchID != "" && matchID == *lastMatch && !lastAt.IsZero() {
		if wait := ballUpdatePace - time.Since(*lastAt); wait > 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return false
			case <-timer.C:
			}
		}
	}
	*lastMatch = matchID
	*lastBall = ballID
	*lastAt = time.Now()
	return true
}

func waitOutboxRetry(ctx context.Context) bool {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
