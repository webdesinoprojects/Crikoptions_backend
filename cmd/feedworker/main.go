package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/config"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/database"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/store"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/worker"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mongoConfig, err := config.LoadMongo()
	if err != nil {
		log.Fatalf("config load: %v", err)
	}
	feedConfig, err := client.LoadConfigFromEnv()
	if err != nil {
		log.Fatalf("CricLive config: %v", err)
	}
	if feedConfig.Mode == client.ModeOff {
		log.Printf("CricLive feedworker disabled (CRICLIVE_MODE=off)")
		<-ctx.Done()
		return
	}

	mongoDB, err := database.ConnectMongo(ctx, mongoConfig.URI, mongoConfig.Database)
	if err != nil {
		log.Fatalf("MongoDB connection: %v", err)
	}
	defer func() { _ = mongoDB.Close(context.Background()) }()
	capabilityCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = mongoDB.RequireTransactions(capabilityCtx)
	cancel()
	if err != nil {
		log.Fatalf("feed persistence safety check: %v", err)
	}

	marketRepository := markets.NewMongoRepository(mongoDB.DB)
	if err := marketRepository.EnsureIndexes(ctx); err != nil {
		log.Fatalf("market indexes: %v", err)
	}
	marketService := markets.NewService(marketRepository)
	feedStore := store.New(mongoDB.DB, marketService)
	if err := feedStore.EnsureIndexes(ctx); err != nil {
		log.Fatalf("feed indexes: %v", err)
	}

	provider, err := client.New(feedConfig, &http.Client{Timeout: feedConfig.HTTPTimeout})
	if err != nil {
		log.Fatalf("CricLive client: %v", err)
	}
	feedWorker, err := worker.New(feedConfig, provider, feedStore, instanceID(), log.Default())
	if err != nil {
		log.Fatalf("CricLive worker: %v", err)
	}
	log.Printf("CricLive feedworker started mode=%s fastPolling=%t corrections=%t", feedConfig.Mode, feedConfig.FastPollingEnabled, feedConfig.AllowLiveCorrections)
	if err := feedWorker.Run(ctx); err != nil {
		log.Fatalf("CricLive feedworker stopped: %v", err)
	}
}

func instanceID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}
