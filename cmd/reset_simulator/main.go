package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/config"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/database"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/simulator"
)

const defaultMatchSpecs = "0000000000000000000000aa=csk_vs_mi,0000000000000000000000bb=rcb_vs_kkr,0000000000000000000000dd=ind_vs_aus_odi"

type resetSpec struct {
	matchID string
	script  string
}

func main() {
	matchSpecs := flag.String("matches", defaultMatchSpecs, "comma-separated matchID=simulator_script pairs")
	dataDir := flag.String("data-dir", "", "simulator data directory; defaults to SIMULATOR_DATA_DIR or ./data/simulator")
	dryRun := flag.Bool("dry-run", false, "print what would be reset without writing MongoDB")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config load: %v", err)
	}

	simCfg := simulator.LoadConfig()
	if strings.TrimSpace(*dataDir) != "" {
		simCfg.DataDir = strings.TrimSpace(*dataDir)
	}

	specs, err := parseResetSpecs(*matchSpecs)
	if err != nil {
		log.Fatalf("parse matches: %v", err)
	}

	ctx := context.Background()
	mongo, err := database.ConnectMongo(ctx, cfg.MongoURI, cfg.MongoDB)
	if err != nil {
		log.Fatalf("mongo connect: %v", err)
	}
	defer func() { _ = mongo.Close(context.Background()) }()

	matchesRepo := matches.NewMongoRepository(mongo.DB)
	eventsRepo := matches.NewMongoEventRepository(mongo.DB)
	if err := matchesRepo.EnsureIndexes(ctx); err != nil {
		log.Fatalf("ensure matches indexes: %v", err)
	}
	if err := matchesRepo.EnsureDefaultMatches(ctx); err != nil {
		log.Fatalf("ensure default matches: %v", err)
	}
	if err := eventsRepo.EnsureIndexes(ctx); err != nil {
		log.Fatalf("ensure match_events indexes: %v", err)
	}
	matchesService := matches.NewService(matchesRepo, eventsRepo, nil)

	log.Printf("reset simulator target database=%q dryRun=%v", cfg.MongoDB, *dryRun)
	for _, spec := range specs {
		if err := resetOne(ctx, matchesService, simCfg.DataDir, spec, *dryRun); err != nil {
			log.Fatalf("reset %s (%s): %v", spec.matchID, spec.script, err)
		}
	}
}

func resetOne(ctx context.Context, svc *matches.Service, dataDir string, spec resetSpec, dryRun bool) error {
	ds, err := simulator.LoadDataset(dataDir, spec.script)
	if err != nil {
		return err
	}
	if ds.MatchID != "" && ds.MatchID != spec.matchID {
		return fmt.Errorf("dataset is for match %s, not %s", ds.MatchID, spec.matchID)
	}

	current, err := svc.GetMatchByID(ctx, spec.matchID)
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("match not found")
	}

	innings1Events, err := svc.BallEventCount(ctx, spec.matchID, 1)
	if err != nil {
		return err
	}
	innings2Events, err := svc.BallEventCount(ctx, spec.matchID, 2)
	if err != nil {
		return err
	}

	log.Printf(
		"match=%s script=%s current=%d/%d innings=%d ballsLeft=%d status=%s events=[i1:%d i2:%d]",
		spec.matchID,
		spec.script,
		current.CurrentScore,
		current.WicketsLost,
		current.Innings,
		current.BallsLeft,
		current.Status,
		innings1Events,
		innings2Events,
	)

	if dryRun {
		return nil
	}

	if err := svc.ClearMatchEvents(ctx, spec.matchID); err != nil {
		return fmt.Errorf("clear match_events: %w", err)
	}

	ballsLeft := matches.TotalBallsForFormat(current.Format)
	targetZero := 0
	if _, err := svc.UpdateMatchScore(ctx, spec.matchID, matches.UpdateScoreRequest{
		Innings:      1,
		CurrentScore: 0,
		WicketsLost:  0,
		BallsLeft:    ballsLeft,
		TargetScore:  &targetZero,
		Status:       matches.StatusLive,
	}); err != nil {
		return fmt.Errorf("reset score: %w", err)
	}

	if _, err := svc.UpdateLiveContext(ctx, spec.matchID, matches.UpdateLiveContextRequest{
		Striker:     matches.BatterStats{Name: ds.Innings1.StartStriker},
		NonStriker:  matches.BatterStats{Name: ds.Innings1.StartNonStriker},
		Bowler:      matches.BowlerStats{Name: ds.Innings1.StartBowler},
		Partnership: matches.PartnershipStats{},
	}); err != nil {
		return fmt.Errorf("reset live context: %w", err)
	}

	updated, err := svc.GetMatchByID(ctx, spec.matchID)
	if err != nil {
		return err
	}
	log.Printf(
		"match=%s reset complete current=%d/%d innings=%d ballsLeft=%d status=%s at=%s",
		spec.matchID,
		updated.CurrentScore,
		updated.WicketsLost,
		updated.Innings,
		updated.BallsLeft,
		updated.Status,
		time.Now().UTC().Format(time.RFC3339),
	)
	return nil
}

func parseResetSpecs(raw string) ([]resetSpec, error) {
	var specs []resetSpec
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		matchID, script, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("expected matchID=script, got %q", part)
		}
		matchID = strings.TrimSpace(matchID)
		script = strings.TrimSpace(script)
		if matchID == "" || script == "" {
			return nil, fmt.Errorf("empty matchID or script in %q", part)
		}
		specs = append(specs, resetSpec{matchID: matchID, script: script})
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("no matches provided")
	}
	return specs, nil
}
