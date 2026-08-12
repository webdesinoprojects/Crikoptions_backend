package challenges

import (
	"net/http"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
)

func RegisterRoutes(mux *http.ServeMux, h *Handler, authHandler *auth.Handler) {
	mux.HandleFunc("GET /api/v1/challenges", authHandler.RequireAuth(h.GetChallenges))
	mux.HandleFunc("POST /api/v1/challenges/{challengeId}/claim", authHandler.RequireAuth(h.ClaimChallenge))
}
