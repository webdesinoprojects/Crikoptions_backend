package database

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

type Mongo struct {
	Client *mongo.Client
	DB     *mongo.Database
}

// ConnectMongo dials Atlas/local Mongo with retries. Free/shared Atlas clusters
// often flap through ReplicaSetNoPrimary or slow pings during elections.
func ConnectMongo(ctx context.Context, uri, dbName string) (*Mongo, error) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return nil, errors.New("MONGO_DB is required")
	}
	dbName = strings.TrimSpace(dbName)
	if dbName == "" {
		dbName = "crikoptions"
	}

	const (
		attempts     = 5
		connectWait  = 20 * time.Second
		pingWait     = 15 * time.Second
		retryBackoff = 3 * time.Second
	)

	opts := options.Client().
		ApplyURI(uri).
		SetServerSelectionTimeout(15 * time.Second).
		SetConnectTimeout(15 * time.Second)

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		connectCtx, cancel := context.WithTimeout(ctx, connectWait)
		client, err := mongo.Connect(connectCtx, opts)
		cancel()
		if err != nil {
			lastErr = err
			log.Printf("MongoDB connect attempt %d/%d failed: %v", attempt, attempts, err)
			if attempt < attempts {
				time.Sleep(retryBackoff)
			}
			continue
		}

		pingCtx, pingCancel := context.WithTimeout(ctx, pingWait)
		err = client.Ping(pingCtx, readpref.Primary())
		pingCancel()
		if err != nil {
			lastErr = err
			_ = client.Disconnect(context.Background())
			log.Printf("MongoDB ping attempt %d/%d failed: %v", attempt, attempts, err)
			if attempt < attempts {
				time.Sleep(retryBackoff)
			}
			continue
		}

		return &Mongo{Client: client, DB: client.Database(dbName)}, nil
	}

	return nil, fmt.Errorf("after %d attempts: %w", attempts, lastErr)
}

func (m *Mongo) Close(ctx context.Context) error {
	if m == nil || m.Client == nil {
		return nil
	}
	closeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return m.Client.Disconnect(closeCtx)
}

func (m *Mongo) Ping(ctx context.Context) error {
	if m == nil || m.Client == nil {
		return errors.New("MongoDB client is unavailable")
	}
	return m.Client.Ping(ctx, readpref.Primary())
}

// RequireTransactions rejects standalone MongoDB. Replica sets and mongos
// deployments support the transactions required by financial writes and the
// live provider feed; live mode additionally uses change streams.
func (m *Mongo) RequireTransactions(ctx context.Context) error {
	if m == nil || m.DB == nil {
		return errors.New("MongoDB database is unavailable")
	}
	var hello bson.M
	if err := m.DB.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		return fmt.Errorf("MongoDB hello: %w", err)
	}
	if setName, _ := hello["setName"].(string); strings.TrimSpace(setName) != "" {
		return nil
	}
	if message, _ := hello["msg"].(string); message == "isdbgrid" {
		return nil
	}
	return errors.New("live feed requires a MongoDB replica set or sharded cluster")
}
