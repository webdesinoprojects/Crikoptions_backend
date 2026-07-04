package orders

import (
	"net/http"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
)

func RegisterRoutes(mux *http.ServeMux, handler *Handler, authHandler *auth.Handler) {
	// Read endpoints: RequireAuth sets ctx[CtxUserID] = userID.
	mux.HandleFunc("GET /api/v1/orders", authHandler.RequireAuth(handler.GetOrders))
	mux.HandleFunc("POST /api/v1/orders/preview", authHandler.RequireAuth(handler.PreviewOrder))
	mux.HandleFunc("POST /api/v1/orders", authHandler.RequireAuth(handler.CreateOrder))
	mux.HandleFunc("PATCH /api/v1/orders/{id}/cancel", authHandler.RequireAuth(handler.CancelOrder))

	// Convenience exit endpoint: close a derived position by id without flipping.
	mux.HandleFunc("POST /api/v1/positions/close-all", authHandler.RequireAuth(handler.CloseAllPositions))
	mux.HandleFunc("POST /api/v1/positions/{id}/close", authHandler.RequireAuth(handler.ClosePosition))

	// Admin-only endpoints - require both auth and admin role.
	mux.HandleFunc("GET /api/v1/admin/orders", authHandler.RequireAuth(authHandler.RequireAdmin(handler.ListAdminOrders)))
	mux.HandleFunc("GET /api/v1/admin/orders/{id}", authHandler.RequireAuth(authHandler.RequireAdmin(handler.GetAdminOrder)))
}
