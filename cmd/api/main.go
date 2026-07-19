package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/config"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/database"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/middleware"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/chat"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/health"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/orders"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/portfolio"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/positions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/simulator"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/watchlist"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/realtime"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/routes"
	sportmonksadmin "github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/admin"
	sportmonksclient "github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/client"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/settlement"
	sportmonksstore "github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/store"
	sportmonksworker "github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/worker"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/watchdog"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config load: %v", err)
	}
	providerConfig, err := sportmonksclient.LoadConfigFromEnv()
	if err != nil {
		log.Fatalf("Sportmonks config: %v", err)
	}

	mongo, err := database.ConnectMongo(context.Background(), cfg.MongoURI, cfg.MongoDB)
	if err != nil {
		log.Fatalf("MongoDB connection failed; no in-memory fallback is configured: %v", err)
	}
	defer func() { _ = mongo.Close(context.Background()) }()
	log.Printf("MongoDB connected; no in-memory fallback enabled; using database %q", cfg.MongoDB)
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	if deleted, err := database.PurgeEphemeralStorage(cleanupCtx, mongo.DB); err != nil {
		log.Printf("mongo ephemeral purge: %v", err)
	} else if deleted > 0 {
		log.Printf("mongo ephemeral purge freed %d documents (users/wallets/orders/matches/trades untouched)", deleted)
	}
	if err := database.EnsureEphemeralTTLs(cleanupCtx, mongo.DB); err != nil {
		log.Printf("mongo ephemeral TTL ensure: %v", err)
	}
	cleanupCancel()
	checkCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = mongo.RequireTransactions(checkCtx)
	cancel()
	if err != nil {
		log.Fatalf("transactional persistence prerequisite: %v", err)
	}
	providerCtx := context.Background()
	var stopProvider context.CancelFunc
	if providerConfig.Mode == sportmonksclient.ModeLive {
		providerCtx, stopProvider = context.WithCancel(context.Background())
	}

	authRepo := auth.NewMongoUserRepository(mongo.DB)
	adminEmails := parseAdminEmails(os.Getenv("ADMIN_EMAILS"))
	authService, err := auth.NewService(authRepo, cfg.JWTSecret, time.Duration(cfg.TokenHours)*time.Hour, adminEmails)
	if err != nil {
		log.Fatalf("auth service: %v", err)
	}
	mustEnsureIndexes(context.Background(), "users", authService.EnsureIndexes)
	authHandler := auth.NewHandler(authService)

	// Matches.
	matchesRepo := matches.NewMongoRepository(mongo.DB)
	mustEnsureIndexes(context.Background(), "matches", matchesRepo.EnsureIndexes)
	if providerConfig.Mode != sportmonksclient.ModeLive {
		seedMongoDefaults(context.Background(), "matches", matchesRepo.SeedDefaults)
		if err := matchesRepo.EnsureDefaultMatches(context.Background()); err != nil {
			log.Fatalf("MongoDB ensure default matches: %v", err)
		}
	}
	matchEventsRepo := matches.NewMongoEventRepository(mongo.DB)
	mustEnsureIndexes(context.Background(), "match_events", matchEventsRepo.EnsureIndexes)
	realtimeHub := realtime.NewHub()
	var stopOutboxWatcher context.CancelFunc
	var outboxWatcher *realtime.OutboxWatcher
	if providerConfig.Mode == sportmonksclient.ModeLive {
		outboxCtx, cancel := context.WithCancel(providerCtx)
		stopOutboxWatcher = cancel
		outboxWatcher = realtime.NewOutboxWatcher(mongo.DB, realtimeHub)
		go func() {
			if err := outboxWatcher.Run(outboxCtx); err != nil && outboxCtx.Err() == nil {
				log.Printf("realtime outbox watcher stopped: %v", err)
			}
		}()
		readyCtx, readyCancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := outboxWatcher.WaitReady(readyCtx)
		readyCancel()
		if err != nil {
			log.Fatalf("realtime outbox prerequisite: %v", err)
		}
	}
	matchesService := matches.NewService(matchesRepo, matchEventsRepo, realtimeHub)
	if providerConfig.Mode == sportmonksclient.ModeLive {
		if n, err := matchesRepo.HideNonSportmonksMatches(context.Background()); err != nil {
			log.Printf("hide demo matches: %v", err)
		} else if n > 0 {
			log.Printf("hid %d demo/simulator matches for Sportmonks live mode", n)
		}
	}
	if err := matchesService.ReconcileOnStartup(context.Background()); err != nil {
		log.Printf("matches reconcile: %v", err)
	}
	matchesHandler := matches.NewHandler(matchesService)

	// Markets.
	marketsRepo := markets.NewMongoRepository(mongo.DB)
	mustEnsureIndexes(context.Background(), "markets", marketsRepo.EnsureIndexes)
	if providerConfig.Mode != sportmonksclient.ModeLive {
		seedMongoDefaults(context.Background(), "markets", marketsRepo.SeedDefaults)
		if err := marketsRepo.EnsureDefaultMarkets(context.Background()); err != nil {
			log.Printf("markets EnsureDefaultMarkets: %v", err)
		}
	}
	marketsService := markets.NewService(marketsRepo)
	marketsHandler := markets.NewHandler(marketsService, matchesService)
	feedStore := sportmonksstore.New(mongo.DB, marketsService)
	marketsService.SetProviderManualGateController(feedStore)
	mustEnsureIndexes(context.Background(), "Sportmonks provider", feedStore.EnsureIndexes)
	sportmonksAdminHandler := sportmonksadmin.NewHandler(feedStore)
	if providerConfig.Mode == sportmonksclient.ModeLive {
		if n, err := feedStore.CompleteStuckTerminalMatches(context.Background(), time.Now().UTC()); err != nil {
			log.Printf("sportmonks complete stuck terminal matches: %v", err)
		} else if n > 0 {
			log.Printf("sportmonks completed %d stuck terminal matches on startup", n)
		}
		if n, err := feedStore.RepairUpcomingUnsupportedMatches(context.Background(), time.Now().UTC()); err != nil {
			log.Printf("sportmonks repair upcoming matches: %v", err)
		} else if n > 0 {
			log.Printf("sportmonks repaired %d unsupported upcoming matches on startup", n)
		}
		if n, err := feedStore.HealFalselyStaleLiveMatches(context.Background(), time.Now().UTC(), 2*time.Minute); err != nil {
			log.Printf("sportmonks heal stale live matches: %v", err)
		} else if n > 0 {
			log.Printf("sportmonks healed %d falsely stale live matches on startup", n)
		}
		go watchdog.Run(providerCtx, feedStore, 5*time.Second)
		provider, providerErr := sportmonksclient.New(providerConfig, &http.Client{Timeout: providerConfig.HTTPTimeout})
		if providerErr != nil {
			log.Fatalf("Sportmonks client: %v", providerErr)
		}
		feedWorker, workerErr := sportmonksworker.New(providerConfig, provider, feedStore, processInstanceID(), log.Default())
		if workerErr != nil {
			log.Fatalf("Sportmonks feed worker: %v", workerErr)
		}
		go func() {
			log.Printf("Sportmonks feed worker started mode=%s fastPolling=%t", providerConfig.Mode, providerConfig.FastPollingEnabled)
			if runErr := feedWorker.Run(providerCtx); runErr != nil && providerCtx.Err() == nil {
				log.Printf("Sportmonks feed worker stopped: %v", runErr)
			}
		}()
		go func() {
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-providerCtx.Done():
					return
				case <-ticker.C:
					purgeCtx, cancel := context.WithTimeout(providerCtx, 2*time.Minute)
					if n, err := database.PurgeEphemeralStorage(purgeCtx, mongo.DB); err != nil {
						log.Printf("mongo periodic ephemeral purge: %v", err)
					} else if n > 0 {
						log.Printf("mongo periodic ephemeral purge removed %d docs", n)
					}
					cancel()
				}
			}
		}()
	}

	// Orders.
	ordersRepo := orders.NewMongoRepository(mongo.DB)
	mustEnsureIndexes(context.Background(), "orders", ordersRepo.EnsureIndexes)
	if providerConfig.Mode != sportmonksclient.ModeLive {
		seedMongoDefaults(context.Background(), "orders", ordersRepo.SeedDefaults)
	}

	// Executions.
	executionsRepo := executions.NewMongoRepository(mongo.DB)
	mustEnsureIndexes(context.Background(), "executions", executionsRepo.EnsureIndexes)
	executionsService := executions.NewService(executionsRepo)
	executionsHandler := executions.NewHandler(executionsService)

	// Wallets.
	walletRepo := wallet.NewMongoRepository(mongo.DB)
	mustEnsureIndexes(context.Background(), "wallet", walletRepo.EnsureIndexes)
	walletService := wallet.NewService(walletRepo)
	walletHandler := wallet.NewHandler(walletService)

	// Wire the welcome bonus creditor into the auth handler now that
	// both services are constructed (avoids import cycle auth→wallet).
	authHandler.SetWelcomeCreditor(walletService)

	// Positions (derived from executions + markets). Created before orders so
	// the order service can resolve/broadcast position state on exits.
	positionsRepo := positions.NewMongoProjectionRepository(mongo.DB)
	mustEnsureIndexes(context.Background(), "position_projections", positionsRepo.EnsureIndexes)
	positionsService := positions.NewServiceWithProjection(executionsService, marketsService, positionsRepo, matchesService, marketsService)
	positionsHandler := positions.NewHandler(positionsService)

	ordersService := orders.NewService(ordersRepo, marketsService, matchesService, walletService, executionsService, positionsService, realtimeHub)
	ordersHandler := orders.NewHandler(ordersService)
	matchesService.SetSettlement(ordersService)
	if providerConfig.Mode == sportmonksclient.ModeLive {
		processor, processorErr := settlement.NewProcessor(feedStore, ordersService, processInstanceID(), providerConfig.LeaseTTL)
		if processorErr != nil {
			log.Fatalf("Sportmonks settlement processor: %v", processorErr)
		}
		go func() {
			if runErr := processor.Run(providerCtx); runErr != nil && providerCtx.Err() == nil {
				log.Printf("Sportmonks settlement processor stopped: %v", runErr)
			}
		}()
	}

	// Portfolio aggregates wallet, positions, markets, and matches server-side so
	// dashboard/portfolio calculations are not duplicated in frontend clients.
	portfolioService := portfolio.NewService(positionsService, walletService, marketsService, matchesService, authRepo)
	portfolioHandler := portfolio.NewHandler(portfolioService)

	// Watchlist.
	watchlistRepo := watchlist.NewMongoRepository(mongo.DB)
	mustEnsureIndexes(context.Background(), "watchlist", watchlistRepo.EnsureIndexes)
	if providerConfig.Mode != sportmonksclient.ModeLive {
		seedMongoDefaults(context.Background(), "watchlist", watchlistRepo.SeedDefaults)
	}
	watchlistService := watchlist.NewService(watchlistRepo, marketsService)
	watchlistHandler := watchlist.NewHandler(watchlistService)

	readinessCheck := mongo.Ping
	if outboxWatcher != nil {
		readinessCheck = func(ctx context.Context) error {
			if err := mongo.Ping(ctx); err != nil {
				return err
			}
			return outboxWatcher.Ready(ctx)
		}
	}
	healthHandler := health.NewHandler(readinessCheck)
	realtimeHandler := realtime.NewHandler(realtimeHub, func(token string) (string, error) {
		id, _, err := authService.ParseToken(token)
		if err != nil {
			return "", err
		}
		return id.Hex(), nil
	})
	realtimeHandler.SetAllowedOrigins(cfg.AllowedOrigins)
	realtimeHandler.SetChatEnabled(cfg.ChatEnabled)

	var chatHandler *chat.Handler
	if cfg.ChatEnabled {
		chatRepo := chat.NewMongoRepository(mongo.DB)
		chatService := chat.NewService(chatRepo, authService, matchesService, realtimeHub)
		mustEnsureIndexes(context.Background(), "chat", chatService.EnsureIndexes)
		chatHandler = chat.NewHandler(chatService)
		log.Printf("chat enabled")
	} else {
		log.Printf("chat disabled")
	}

	// Simulator — replay ball events from CSV datasets automatically.
	simCfg := simulator.LoadConfig()
	simLocks := simulator.NewMongoLockStore(mongo.DB)
	mustEnsureIndexes(context.Background(), "simulator_locks", simLocks.EnsureIndexes)
	simService := simulator.NewService(simCfg, matchesService)
	simService.SetSquareOff(ordersService)
	simService.SetLockStore(simLocks)
	simHandler := simulator.NewHandler(simService)
	defer simService.Shutdown()
	if providerConfig.Mode != sportmonksclient.ModeLive {
		simService.AutoStartOnBoot(context.Background())
	}

	router := routes.NewRouter(healthHandler, matchesHandler, authHandler, marketsHandler, watchlistHandler, ordersHandler, positionsHandler, portfolioHandler, walletHandler, executionsHandler, realtimeHandler, simHandler, chatHandler, sportmonksAdminHandler)
	handler := middleware.Chain(router, middleware.Recover, middleware.Logger, middleware.CORS(cfg.AllowedOrigins))

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("listening on http://localhost:%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	if stopOutboxWatcher != nil {
		stopOutboxWatcher()
	}
	if stopProvider != nil {
		stopProvider()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = srv.Shutdown(ctx)
}

func processInstanceID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

func parseAdminEmails(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func mustEnsureIndexes(ctx context.Context, collection string, ensure func(context.Context) error) {
	if err := ensure(ctx); err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "space quota") || strings.Contains(msg, "storage quota") {
			log.Printf("MongoDB index setup %s skipped (Atlas space quota): %v", collection, err)
			return
		}
		log.Fatalf("MongoDB index setup %s: %v", collection, err)
	}
}

func seedMongoDefaults(ctx context.Context, collection string, seed func(context.Context) (int, error)) {
	inserted, err := seed(ctx)
	if err != nil {
		log.Fatalf("MongoDB seed %s: %v", collection, err)
	}
	if inserted > 0 {
		log.Printf("MongoDB seeded %s: %d documents", collection, inserted)
	}
}
