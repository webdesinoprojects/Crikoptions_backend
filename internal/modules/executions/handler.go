package executions

import (
	"net/http"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/shared/httpjson"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) GetExecutions(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		httpjson.Write(w, http.StatusUnauthorized, map[string]any{
			"success": false,
			"message": "Unauthorized",
		})
		return
	}

	matchID := r.URL.Query().Get("matchId")
	marketID := r.URL.Query().Get("marketId")
	items := h.service.ListUserExecutions(r.Context(), userID, matchID, marketID, 100)
	if items == nil {
		items = []Execution{}
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Executions fetched successfully",
		"data":    items,
	})
}

func (h *Handler) ListAdminExecutions(w http.ResponseWriter, r *http.Request) {
	matchID := r.URL.Query().Get("matchId")
	marketID := r.URL.Query().Get("marketId")
	items := h.service.ListExecutions(r.Context(), Filter{
		MatchID:  matchID,
		MarketID: marketID,
		Limit:    200,
	})
	if items == nil {
		items = []Execution{}
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Executions fetched successfully",
		"data":    items,
	})
}
