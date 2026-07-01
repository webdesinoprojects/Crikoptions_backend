package simulator

import (
	"net/http"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
)

// RegisterRoutes mounts all simulator admin routes onto mux.
// All routes require both authentication and admin role.
func RegisterRoutes(mux *http.ServeMux, handler *Handler, authHandler *auth.Handler) {
	admin := func(h http.HandlerFunc) http.HandlerFunc {
		return authHandler.RequireAuth(authHandler.RequireAdmin(h))
	}

	mux.HandleFunc("POST /api/v1/admin/matches/{id}/simulator/start",
		admin(handler.Start))
	mux.HandleFunc("POST /api/v1/admin/matches/{id}/simulator/pause",
		admin(handler.Pause))
	mux.HandleFunc("POST /api/v1/admin/matches/{id}/simulator/resume",
		admin(handler.Resume))
	mux.HandleFunc("POST /api/v1/admin/matches/{id}/simulator/reset",
		admin(handler.Reset))
	mux.HandleFunc("GET /api/v1/admin/matches/{id}/simulator/status",
		admin(handler.Status))
}
