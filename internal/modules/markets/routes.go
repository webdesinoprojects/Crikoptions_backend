package markets

import (
	"net/http"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
)

func RegisterRoutes(mux *http.ServeMux, handler *Handler, authHandler *auth.Handler) {
	mux.HandleFunc("GET /api/v1/matches/{id}/markets", func(w http.ResponseWriter, r *http.Request) {
		handler.GetMarketsByMatchID(w, r)
	})

	mux.HandleFunc("GET /api/v1/markets/{id}", func(w http.ResponseWriter, r *http.Request) {
		handler.GetMarketDetail(w, r)
	})

	mux.HandleFunc("POST /api/v1/markets/{id}/calculate-price", func(w http.ResponseWriter, r *http.Request) {
		handler.CalculatePrice(w, r)
	})

	// Admin-only: create + control markets (option/auction chains).
	if authHandler != nil {
		mux.HandleFunc("POST /api/v1/admin/markets", authHandler.RequireAuth(authHandler.RequireAdmin(handler.CreateMarket)))
		mux.HandleFunc("PATCH /api/v1/admin/markets/{id}/suspend", authHandler.RequireAuth(authHandler.RequireAdmin(handler.SuspendMarket)))
		mux.HandleFunc("PATCH /api/v1/admin/markets/{id}/resume", authHandler.RequireAuth(authHandler.RequireAdmin(handler.ResumeMarket)))
	}
}