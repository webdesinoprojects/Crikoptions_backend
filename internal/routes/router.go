package routes

import (
	"net/http"

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
	sportmonksadmin "github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/admin"
)

func NewRouter(
	healthHandler *health.Handler,
	matchesHandler *matches.Handler,
	authHandler *auth.Handler,
	marketsHandler *markets.Handler,
	watchlistHandler *watchlist.Handler,
	ordersHandler *orders.Handler,
	positionsHandler *positions.Handler,
	portfolioHandler *portfolio.Handler,
	walletHandler *wallet.Handler,
	executionsHandler *executions.Handler,
	realtimeHandler *realtime.Handler,
	simulatorHandler *simulator.Handler,
	chatHandler *chat.Handler,
	sportmonksHandlers ...*sportmonksadmin.Handler,
) http.Handler {
	mux := http.NewServeMux()

	if healthHandler != nil {
		health.RegisterRoutes(mux, healthHandler)
	}

	if matchesHandler != nil {
		matches.RegisterRoutes(mux, matchesHandler, authHandler)
	}

	if authHandler != nil {
		auth.RegisterRoutes(mux, authHandler)
	}

	if marketsHandler != nil {
		markets.RegisterRoutes(mux, marketsHandler, authHandler)
	}

	if watchlistHandler != nil {
		watchlist.RegisterRoutes(mux, watchlistHandler, authHandler)
	}

	if ordersHandler != nil {
		orders.RegisterRoutes(mux, ordersHandler, authHandler)
	}

	if positionsHandler != nil {
		positions.RegisterRoutes(mux, positionsHandler, authHandler)
	}

	if portfolioHandler != nil {
		portfolio.RegisterRoutes(mux, portfolioHandler, authHandler)
	}

	if walletHandler != nil {
		wallet.RegisterRoutes(mux, walletHandler, authHandler)
	}

	if executionsHandler != nil {
		executions.RegisterRoutes(mux, executionsHandler, authHandler)
	}

	if realtimeHandler != nil {
		realtime.RegisterRoutes(mux, realtimeHandler)
	}

	if simulatorHandler != nil && authHandler != nil {
		simulator.RegisterRoutes(mux, simulatorHandler, authHandler)
	}

	if chatHandler != nil && authHandler != nil {
		chat.RegisterRoutes(mux, chatHandler, authHandler)
	}
	if len(sportmonksHandlers) > 0 {
		sportmonksadmin.RegisterRoutes(mux, sportmonksHandlers[0], authHandler)
	}

	return mux
}
