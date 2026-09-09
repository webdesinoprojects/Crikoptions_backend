package admin

import (
	"net/http"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
)

func RegisterRoutes(mux *http.ServeMux, handler *Handler, authHandler *auth.Handler) {
	if handler == nil || authHandler == nil {
		return
	}
	adminOnly := func(next http.HandlerFunc) http.HandlerFunc {
		return authHandler.RequireAuth(authHandler.RequireAdmin(next))
	}
	mux.HandleFunc("GET /api/v1/admin/providers/criclive/status", adminOnly(handler.Status))
	mux.HandleFunc("GET /api/v1/admin/providers/criclive/leagues", adminOnly(handler.Leagues))
	mux.HandleFunc("PATCH /api/v1/admin/providers/criclive/leagues/{id}", adminOnly(handler.SetLeagueEnabled))
	mux.HandleFunc("GET /api/v1/admin/providers/criclive/kill-switch", adminOnly(handler.KillSwitch))
	mux.HandleFunc("PUT /api/v1/admin/providers/criclive/kill-switch", adminOnly(handler.SetKillSwitch))
	mux.HandleFunc("GET /api/v1/admin/providers/criclive/incidents", adminOnly(handler.Incidents))
	mux.HandleFunc("GET /api/v1/admin/providers/criclive/fixtures/{id}/diagnostics", adminOnly(handler.FixtureDiagnostics))
	mux.HandleFunc("POST /api/v1/admin/providers/criclive/fixtures/{id}/resync", adminOnly(handler.ResyncFixture))
}
