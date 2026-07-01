package wallet

import (
	"net/http"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
)

func RegisterRoutes(mux *http.ServeMux, handler *Handler, authHandler *auth.Handler) {
	if handler == nil || authHandler == nil {
		return
	}

	mux.HandleFunc("GET /api/v1/wallet", authHandler.RequireAuth(handler.GetWallet))
	mux.HandleFunc("GET /api/v1/wallet/ledger", authHandler.RequireAuth(handler.GetLedger))
	mux.HandleFunc("POST /api/v1/wallet/topup", authHandler.RequireAuth(handler.UserTopUp))

	mux.HandleFunc("GET /api/v1/admin/users/{userId}/wallet", authHandler.RequireAuth(authHandler.RequireAdmin(handler.AdminGetWallet)))
	mux.HandleFunc("POST /api/v1/admin/users/{userId}/wallet/credit", authHandler.RequireAuth(authHandler.RequireAdmin(handler.AdminCredit)))
	mux.HandleFunc("POST /api/v1/admin/users/{userId}/wallet/debit", authHandler.RequireAuth(authHandler.RequireAdmin(handler.AdminDebit)))
	mux.HandleFunc("GET /api/v1/admin/wallet-ledger", authHandler.RequireAuth(authHandler.RequireAdmin(handler.AdminListLedger)))
}

