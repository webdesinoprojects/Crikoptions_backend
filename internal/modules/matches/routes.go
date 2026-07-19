package matches

import (
	"net/http"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
)

func RegisterRoutes(mux *http.ServeMux, handler *Handler, authHandler *auth.Handler) {
	// Public endpoints - no auth required.
	mux.HandleFunc("GET /api/v1/matches/home", handler.GetHomeMatches)
	mux.HandleFunc("GET /api/v1/matches/upcoming", handler.GetUpcomingMatches)
	mux.HandleFunc("GET /api/v1/matches/{id}/events", handler.GetMatchEvents)
	mux.HandleFunc("GET /api/v1/matches/{id}/live-state", handler.GetMatchDetail)
	mux.HandleFunc("GET /api/v1/matches/{id}", handler.GetMatchDetail)

	// Admin-only endpoints - require both auth and admin role.
	mux.HandleFunc("POST /api/v1/admin/matches", authHandler.RequireAuth(authHandler.RequireAdmin(handler.CreateMatch)))
	mux.HandleFunc("PATCH /api/v1/admin/matches/{id}/score", authHandler.RequireAuth(authHandler.RequireAdmin(handler.UpdateMatchScore)))
	mux.HandleFunc("PATCH /api/v1/admin/matches/{id}/players", authHandler.RequireAuth(authHandler.RequireAdmin(handler.UpdateLiveContext)))
	mux.HandleFunc("POST /api/v1/admin/matches/{id}/start", authHandler.RequireAuth(authHandler.RequireAdmin(handler.StartMatch)))
	mux.HandleFunc("POST /api/v1/admin/matches/{id}/complete", authHandler.RequireAuth(authHandler.RequireAdmin(handler.CompleteMatch)))
	mux.HandleFunc("POST /api/v1/admin/matches/{id}/ball", authHandler.RequireAuth(authHandler.RequireAdmin(handler.RecordBall)))
}
