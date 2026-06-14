package executions

import (
	"net/http"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
)

func RegisterRoutes(mux *http.ServeMux, handler *Handler, authHandler *auth.Handler) {
	mux.HandleFunc("GET /api/v1/executions", authHandler.RequireAuth(handler.GetExecutions))
	mux.HandleFunc("GET /api/v1/admin/executions", authHandler.RequireAuth(authHandler.RequireAdmin(handler.ListAdminExecutions)))
}
