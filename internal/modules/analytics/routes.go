package analytics

import (
	"net/http"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
)

func RegisterRoutes(mux *http.ServeMux, handler *Handler, authHandler *auth.Handler) {
	if handler == nil || authHandler == nil {
		return
	}

	mux.HandleFunc("GET /api/v1/admin/analytics/overview", authHandler.RequireAuth(authHandler.RequireAdmin(handler.Overview)))
}
