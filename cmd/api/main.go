package main

import (
	"context"
	"errors"
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
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/executions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/health"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/orders"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/portfolio"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/positions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/watchlist"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/realtime"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/routes"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config load: %v", err)
	}

	mongo, err := database.ConnectMongo(context.Background(), cfg.MongoURI, cfg.MongoDB)
	if err != nil {
		log.Fatalf("MongoDB connection failed; no in-memory fallback is configured: %v", err)
	}
	defer func() { _ = mongo.Close(context.Background()) }()
	log.Printf("MongoDB connected; no in-memory fallback enabled; using database %q", cfg.MongoDB)

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
	seedMongoDefaults(context.Background(), "matches", matchesRepo.SeedDefaults)
	matchEventsRepo := matches.NewMongoEventRepository(mongo.DB)
	mustEnsureIndexes(context.Background(), "match_events", matchEventsRepo.EnsureIndexes)
	realtimeHub := realtime.NewHub()
	matchesService := matches.NewService(matchesRepo, matchEventsRepo, realtimeHub)
	if err := matchesService.ReconcileOnStartup(context.Background()); err != nil {
		log.Printf("matches reconcile: %v", err)
	}
	matchesHandler := matches.NewHandler(matchesService)

	// Markets.
	marketsRepo := markets.NewMongoRepository(mongo.DB)
	mustEnsureIndexes(context.Background(), "markets", marketsRepo.EnsureIndexes)
	seedMongoDefaults(context.Background(), "markets", marketsRepo.SeedDefaults)
	marketsService := markets.NewService(marketsRepo)
	marketsHandler := markets.NewHandler(marketsService)

	// Orders.
	ordersRepo := orders.NewMongoRepository(mongo.DB)
	mustEnsureIndexes(context.Background(), "orders", ordersRepo.EnsureIndexes)
	seedMongoDefaults(context.Background(), "orders", ordersRepo.SeedDefaults)

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
	positionsService := positions.NewServiceWithProjection(executionsService, marketsService, positionsRepo)
	positionsHandler := positions.NewHandler(positionsService)

	ordersService := orders.NewService(ordersRepo, marketsService, matchesService, walletService, executionsService, positionsService, realtimeHub)
	ordersHandler := orders.NewHandler(ordersService)

	// Portfolio aggregates wallet, positions, markets, and matches server-side so
	// dashboard/portfolio calculations are not duplicated in frontend clients.
	portfolioService := portfolio.NewService(positionsService, walletService, marketsService, matchesService)
	portfolioHandler := portfolio.NewHandler(portfolioService)

	// Watchlist.
	watchlistRepo := watchlist.NewMongoRepository(mongo.DB)
	mustEnsureIndexes(context.Background(), "watchlist", watchlistRepo.EnsureIndexes)
	seedMongoDefaults(context.Background(), "watchlist", watchlistRepo.SeedDefaults)
	watchlistService := watchlist.NewService(watchlistRepo, marketsService)
	watchlistHandler := watchlist.NewHandler(watchlistService)

	healthHandler := health.NewHandler()
	realtimeHandler := realtime.NewHandler(realtimeHub, func(token string) (string, error) {
		id, _, err := authService.ParseToken(token)
		if err != nil {
			return "", err
		}
		return id.Hex(), nil
	})
	router := routes.NewRouter(healthHandler, matchesHandler, authHandler, marketsHandler, watchlistHandler, ordersHandler, positionsHandler, portfolioHandler, walletHandler, executionsHandler, realtimeHandler)
	handler := middleware.Chain(router, middleware.Recover, middleware.Logger, middleware.CORS)

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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = srv.Shutdown(ctx)
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
