package chat

import (
	"net/http"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
)

func RegisterRoutes(mux *http.ServeMux, handler *Handler, authHandler *auth.Handler) {
	if mux == nil || handler == nil || authHandler == nil {
		return
	}
	requireAuth := authHandler.RequireAuth
	requireAdmin := func(next http.HandlerFunc) http.HandlerFunc {
		return authHandler.RequireAuth(authHandler.RequireAdmin(next))
	}
	mux.HandleFunc("GET /api/v1/chat/rooms", requireAuth(handler.ListRooms))
	mux.HandleFunc("GET /api/v1/chat/rooms/{roomId}/messages", requireAuth(handler.ListMessages))
	mux.HandleFunc("POST /api/v1/chat/rooms/{roomId}/messages", requireAuth(handler.CreateMessage))
	mux.HandleFunc("POST /api/v1/chat/rooms/{roomId}/read", requireAuth(handler.MarkRead))
	mux.HandleFunc("DELETE /api/v1/chat/messages/{messageId}", requireAuth(handler.DeleteMessage))
	mux.HandleFunc("POST /api/v1/chat/messages/{messageId}/reports", requireAuth(handler.ReportMessage))
	mux.HandleFunc("GET /api/v1/admin/chat/reports", requireAdmin(handler.ListReports))
	mux.HandleFunc("PATCH /api/v1/admin/chat/reports/{reportId}", requireAdmin(handler.ResolveReport))
}
