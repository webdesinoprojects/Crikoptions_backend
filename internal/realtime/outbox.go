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

		for stream.Next(ctx) {
			var change struct {
				Document outboxDocument `bson:"fullDocument"`
			}
			if err := stream.Decode(&change); err != nil {
				log.Printf("realtime outbox decode: %v", err)
				continue
			}
			if topic := strings.TrimSpace(change.Document.Topic); topic != "" {
				w.publisher.Publish(topic, outboxPayload(change.Document))
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
