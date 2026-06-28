package portfolio

import (
	"net/http"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
)

func RegisterRoutes(mux *http.ServeMux, handler *Handler, authHandler *auth.Handler) {
	mux.HandleFunc("GET /api/v1/portfolio/summary", authHandler.RequireAuth(handler.GetSummary))
	mux.HandleFunc("GET /api/v1/portfolio/daily-pnl", authHandler.RequireAuth(handler.GetDailyPnL))
	mux.HandleFunc("GET /api/v1/portfolio/risk-summary", authHandler.RequireAuth(handler.GetRiskSummary))
	mux.HandleFunc("GET /api/v1/portfolio/markets/{marketId}/pnl", authHandler.RequireAuth(handler.GetMarketPnL))
}
