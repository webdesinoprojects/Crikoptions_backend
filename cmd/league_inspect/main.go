// Command league_inspect inspects and optionally toggles Sportmonks league
// entitlement flags. Without arguments it is read-only.
//
//	go run ./cmd/league_inspect                  # dump league state
//	go run ./cmd/league_inspect -enable 3,258    # turn leagues on
//	go run ./cmd/league_inspect -disable 3       # turn leagues off
//
// Toggling routes through store.SetLeagueEnabled so the transactional cascade
// (fixtures -> eligible, upcoming matches -> unhidden) matches the admin API.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/config"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/store"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type leagueRow struct {
	ID         int64     `bson:"_id"`
	Name       string    `bson:"name"`
	Code       string    `bson:"code"`
	Entitled   bool      `bson:"entitled"`
	Enabled    bool      `bson:"enabled"`
	LastSeenAt time.Time `bson:"lastSeenAt"`
}

func parseIDs(raw string) ([]int64, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var ids []int64
	for _, part := range strings.Split(raw, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid league id %q", part)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// runSync mirrors worker.syncFixtures without claiming the scheduler lease, so
// a catalog refresh can be forced while another instance holds it.
func runSync(ctx context.Context, db *mongo.Database, days int) {
	cfg, err := client.LoadConfigFromEnv()
	if err != nil {
		log.Fatalf("sportmonks config: %v", err)
	}
	api, err := client.New(cfg, nil)
	if err != nil {
		log.Fatalf("sportmonks client: %v", err)
	}
	s := store.New(db, nil)

	leagueIDs, err := s.EnabledLeagueIDs(ctx)
	if err != nil {
		log.Fatalf("enabled leagues: %v", err)
	}
	sort.Slice(leagueIDs, func(i, j int) bool { return leagueIDs[i] < leagueIDs[j] })

	now := time.Now().UTC()
	from, to := now.Add(-2*24*time.Hour), now.Add(time.Duration(days)*24*time.Hour)
	fmt.Printf("SYNC window %s .. %s over %d leagues\n",
		from.Format("2006-01-02"), to.Format("2006-01-02"), len(leagueIDs))

	totalFixtures, totalPublished, failures := 0, 0, 0
	for _, leagueID := range leagueIDs {
		count := 0
		for page := 1; ; page++ {
			envelope, err := api.Fixtures(ctx, client.FixturesOptions{
				From: from, To: to, LeagueID: leagueID, Page: page,
				Sort: "starting_at", Includes: []string{"localteam", "visitorteam"},
			})
			if err != nil {
				fmt.Printf("  league %-4d FETCH FAILED: %v\n", leagueID, err)
				failures++
				break
			}
			if len(envelope.Data) == 0 {
				break
			}
			count += len(envelope.Data)
			if err := s.UpsertFixtureTargets(ctx, envelope.Data, now, true, cfg.AllowMidMatchLiveAdmission); err != nil {
				fmt.Printf("  league %-4d UPSERT FAILED: %v\n", leagueID, err)
				failures++
				break
			}
			if err := s.PublishFixtureMatches(ctx, envelope.Data, now, cfg.AllowMidMatchLiveAdmission); err != nil {
				fmt.Printf("  league %-4d PUBLISH FAILED: %v\n", leagueID, err)
				failures++
				break
			}
			totalPublished += len(envelope.Data)
			if envelope.Meta.Pagination == nil {
				break
			}
			if _, more := envelope.Meta.Pagination.NextPage(); !more {
				break
			}
		}
		if count > 0 {
			fmt.Printf("  league %-4d %d fixtures\n", leagueID, count)
		}
		totalFixtures += count
	}
	fmt.Printf("\nSYNC done: %d fixtures seen, %d published, %d failures\n\n",
		totalFixtures, totalPublished, failures)
}

func main() {
	enableRaw := flag.String("enable", "", "comma-separated league IDs to enable")
	disableRaw := flag.String("disable", "", "comma-separated league IDs to disable")
	clearLease := flag.String("clear-lease", "", "scheduler lease name to release (e.g. fixtures-catalog)")
	enableAll := flag.Bool("enable-all", false, "enable every entitled league")
	syncNow := flag.Bool("sync", false, "run a one-off fixture catalog sync, bypassing the scheduler lease")
	syncDays := flag.Int("sync-days", 14, "forward window in days for -sync")
	flag.Parse()

	enableIDs, err := parseIDs(*enableRaw)
	if err != nil {
		log.Fatal(err)
	}
	disableIDs, err := parseIDs(*disableRaw)
	if err != nil {
		log.Fatal(err)
	}

	cfg, err := config.LoadMongo()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(cfg.URI))
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer client.Disconnect(context.Background())
	db := client.Database(cfg.Database)

	if name := strings.TrimSpace(*clearLease); name != "" {
		res, err := db.Collection("provider_scheduler_leases").DeleteOne(ctx, bson.M{"_id": name})
		if err != nil {
			log.Fatalf("clear lease %q: %v", name, err)
		}
		fmt.Printf("lease %q released (deleted=%d)\n\n", name, res.DeletedCount)
	}

	if *enableAll {
		var rows []leagueRow
		cur, err := db.Collection("provider_leagues").Find(ctx, bson.M{"entitled": true})
		if err != nil {
			log.Fatalf("find entitled leagues: %v", err)
		}
		if err := cur.All(ctx, &rows); err != nil {
			log.Fatalf("decode entitled leagues: %v", err)
		}
		for _, r := range rows {
			if !r.Enabled {
				enableIDs = append(enableIDs, r.ID)
			}
		}
		fmt.Printf("enable-all: %d entitled, %d already on, %d to enable\n\n",
			len(rows), len(rows)-len(enableIDs), len(enableIDs))
	}

	if len(enableIDs) > 0 || len(disableIDs) > 0 {
		s := store.New(db, nil)
		apply := func(ids []int64, enabled bool) {
			for _, id := range ids {
				updated, err := s.SetLeagueEnabled(ctx, id, enabled)
				switch {
				case err != nil:
					fmt.Printf("league %-4d -> enabled=%-5t FAILED: %v\n", id, enabled, err)
				case !updated:
					fmt.Printf("league %-4d -> enabled=%-5t SKIPPED (not entitled / not found)\n", id, enabled)
				default:
					fmt.Printf("league %-4d -> enabled=%-5t ok\n", id, enabled)
				}
			}
		}
		apply(enableIDs, true)
		apply(disableIDs, false)
		fmt.Println()
	}

	if *syncNow {
		runSync(ctx, db, *syncDays)
	}

	var leagues []leagueRow
	cur, err := db.Collection("provider_leagues").Find(ctx, bson.M{})
	if err != nil {
		log.Fatalf("find leagues: %v", err)
	}
	if err := cur.All(ctx, &leagues); err != nil {
		log.Fatalf("decode leagues: %v", err)
	}
	sort.Slice(leagues, func(i, j int) bool { return leagues[i].ID < leagues[j].ID })

	fmt.Println("ENABLED LEAGUES")
	enabled, entitled := 0, 0
	for _, l := range leagues {
		if l.Entitled {
			entitled++
		}
		if l.Enabled {
			enabled++
			fmt.Printf("  %-4d %-38s %s\n", l.ID, l.Name, l.Code)
		}
	}
	fmt.Printf("\ntotal=%d entitled=%d enabled=%d\n", len(leagues), entitled, enabled)

	now := time.Now().UTC()
	upcoming, err := db.Collection("matches").CountDocuments(ctx, bson.M{
		"status": "upcoming", "hidden": bson.M{"$ne": true},
	})
	if err == nil {
		fmt.Printf("\npublic matches status=upcoming hidden!=true : %d", upcoming)
	}
	future, err := db.Collection("provider_fixtures").CountDocuments(ctx, bson.M{
		"startTime": bson.M{"$gte": now},
	})
	if err == nil {
		fmt.Printf("\nfixture targets with startTime >= now       : %d\n", future)
	}

	fmt.Printf("\nSCHEDULER LEASES (now=%s)\n", now.Format("15:04:05"))
	leaseCur, err := db.Collection("provider_scheduler_leases").Find(ctx, bson.M{})
	if err == nil {
		var leases []bson.M
		if leaseCur.All(ctx, &leases) == nil {
			for _, l := range leases {
				until, _ := l["leaseUntil"].(primitive.DateTime)
				expired := until.Time().UTC().Before(now)
				fmt.Printf("  %-20v until=%s expired=%t owner=%v\n",
					l["_id"], until.Time().UTC().Format("15:04:05"), expired, l["leaseOwner"])
			}
		}
	}

	fmt.Println("\nFIXTURE TARGETS")
	fxCur, err := db.Collection("provider_fixtures").Find(ctx, bson.M{},
		options.Find().SetSort(bson.D{{Key: "startTime", Value: 1}}))
	if err == nil {
		var fx []bson.M
		if fxCur.All(ctx, &fx) == nil {
			for _, f := range fx {
				st, _ := f["startTime"].(primitive.DateTime)
				fmt.Printf("  fixture=%-8v league=%-5v format=%-6v supported=%-5v eligible=%-5v start=%s\n",
					f["_id"], f["leagueId"], f["format"], f["supported"], f["eligible"],
					st.Time().UTC().Format("2006-01-02 15:04"))
			}
		}
	}

	fmt.Println("\nPUBLIC MATCHES")
	mCur, err := db.Collection("matches").Find(ctx, bson.M{},
		options.Find().SetSort(bson.D{{Key: "startTime", Value: 1}}))
	if err == nil {
		var ms []bson.M
		if mCur.All(ctx, &ms) == nil {
			for _, m := range ms {
				st, _ := m["startTime"].(primitive.DateTime)
				fmt.Printf("  %-22v v %-22v status=%-10v hidden=%-5v format=%-5v league=%-5v start=%s\n",
					m["teamAName"], m["teamBName"], m["status"], m["hidden"], m["format"],
					m["providerLeagueId"], st.Time().UTC().Format("2006-01-02 15:04"))
			}
		}
	}
}
