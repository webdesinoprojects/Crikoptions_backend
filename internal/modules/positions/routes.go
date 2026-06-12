package positions

import (
	"net/http"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
)

func RegisterRoutes(mux *http.ServeMux, handler *Handler, authHandler *auth.Handler) {
	// User-scoped endpoints - auth required.
	mux.HandleFunc("GET /api/v1/positions/open", authHandler.RequireAuth(handler.GetOpenPositions))
	mux.HandleFunc("GET /api/v1/positions/closed", authHandler.RequireAuth(handler.GetClosedPositions))
	mux.HandleFunc("GET /api/v1/positions/{id}", authHandler.RequireAuth(handler.GetPositionDetail))

	// Admin-only endpoints - require both auth and admin role.
	// PRD API #28: GET /api/v1/admin/positions
	mux.HandleFunc("GET /api/v1/admin/positions", authHandler.RequireAuth(authHandler.RequireAdmin(handler.ListAdminPositions)))
	mux.HandleFunc("GET /api/v1/admin/positions/{id}", authHandler.RequireAuth(authHandler.RequireAdmin(handler.GetAdminPositionDetail)))
}
