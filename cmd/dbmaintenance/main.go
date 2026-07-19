package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/config"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/database"
	"go.mongodb.org/mongo-driver/bson"
)

type collectionStats struct {
	Name           string `bson:"ns"`
	Count          int64  `bson:"count"`
	Size           int64  `bson:"size"`
	StorageSize    int64  `bson:"storageSize"`
	TotalIndexSize int64  `bson:"totalIndexSize"`
}

func main() {
	mode := flag.String("mode", "stats", "operation to run: stats or purge-provider-payloads")
	apply := flag.Bool("apply", false, "apply a destructive maintenance operation")
	flag.Parse()

	if *mode != "stats" && *mode != "purge-provider-payloads" {
		log.Fatalf("unsupported mode %q", *mode)
	}
	if *mode == "purge-provider-payloads" && !*apply {
		log.Fatal("purge-provider-payloads requires -apply")
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config load: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	mongoDB, err := database.ConnectMongo(ctx, cfg.MongoURI, cfg.MongoDB)
	if err != nil {
		log.Fatalf("mongo connect: %v", err)
	}
	defer func() { _ = mongoDB.Close(context.Background()) }()

	if *mode == "purge-provider-payloads" {
		result, err := mongoDB.DB.Collection("provider_payloads").DeleteMany(ctx, bson.D{})
		if err != nil {
			log.Fatalf("purge provider payloads: %v", err)
		}
		fmt.Fprintf(os.Stdout, "deleted %d provider payload diagnostics\n", result.DeletedCount)
		return
	}

	collections, err := mongoDB.DB.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		log.Fatalf("list collections: %v", err)
	}
	stats := make([]collectionStats, 0, len(collections))
	for _, name := range collections {
		var row collectionStats
		if err := mongoDB.DB.RunCommand(ctx, bson.D{{Key: "collStats", Value: name}}).Decode(&row); err != nil {
			log.Printf("collection %s stats unavailable: %v", name, err)
			continue
		}
		row.Name = strings.TrimPrefix(row.Name, cfg.MongoDB+".")
		stats = append(stats, row)
	}
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Size+stats[i].TotalIndexSize > stats[j].Size+stats[j].TotalIndexSize
	})

	fmt.Fprintf(os.Stdout, "%-32s %12s %12s %12s %12s\n", "COLLECTION", "DOCUMENTS", "DATA", "STORAGE", "INDEXES")
	for _, row := range stats {
		fmt.Fprintf(os.Stdout, "%-32s %12d %12s %12s %12s\n", row.Name, row.Count, bytes(row.Size), bytes(row.StorageSize), bytes(row.TotalIndexSize))
	}
}

func bytes(value int64) string {
	const unit = int64(1024)
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	div, exp := unit, 0
	for n := value / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(value)/float64(div), "KMGTPE"[exp])
}
