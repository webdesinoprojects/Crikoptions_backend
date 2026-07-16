package main

import (
	"context"
	"flag"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/config"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/database"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/positions"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "count executions without mutating position projections")
	skipClear := flag.Bool("skip-clear", false, "append/replay into existing projections instead of clearing first")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config load: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	mongo, err := database.ConnectMongo(ctx, cfg.MongoURI, cfg.MongoDB)
	if err != nil {
		log.Fatalf("mongo connect: %v", err)
	}
	defer func() { _ = mongo.Close(context.Background()) }()

	execs := mongo.DB.Collection("executions")
	projections := positions.NewMongoProjectionRepository(mongo.DB)
	if err := projections.EnsureIndexes(ctx); err != nil {
		log.Fatalf("ensure projection indexes: %v", err)
	}

	cur, err := execs.Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}}))
	if err != nil {
		log.Fatalf("find executions: %v", err)
	}
	defer cur.Close(ctx)

	if *dryRun {
		count := 0
		for cur.Next(ctx) {
			count++
		}
		if err := cur.Err(); err != nil {
			log.Fatalf("scan executions: %v", err)
		}
		log.Printf("dry run complete; executions=%d", count)
		return
	}

	if !*skipClear {
		if err := projections.Clear(ctx); err != nil {
			log.Fatalf("clear projections: %v", err)
		}
	}

	count := 0
	for cur.Next(ctx) {
		var exec executions.Execution
		if err := cur.Decode(&exec); err != nil {
			log.Fatalf("decode execution %d: %v", count, err)
		}
		if _, err := projections.ApplyExecution(ctx, exec, positions.ProjectionConstraint{}); err != nil {
			log.Fatalf("apply execution %s: %v", exec.ID.Hex(), err)
		}
		count++
	}
	if err := cur.Err(); err != nil {
		log.Fatalf("scan executions: %v", err)
	}
	log.Printf("position projection backfill complete; executions=%d", count)
}
