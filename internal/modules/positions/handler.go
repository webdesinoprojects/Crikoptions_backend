package positions

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

// GetOpenPositions returns the authenticated user's open positions.
// PRD API #13: GET /api/v1/positions/open
func (h *Handler) GetOpenPositions(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		httpjson.Write(w, http.StatusUnauthorized, map[string]any{
			"success": false,
			"message": "Unauthorized",
		})
		return
	}

	positions, err := h.service.ListUserPositions(r.Context(), userID, PositionFilter{
		Status:   "open",
		MatchID:  r.URL.Query().Get("matchId"),
		MarketID: r.URL.Query().Get("marketId"),
	})
	if err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"message": "Failed to fetch open positions",
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Open positions fetched successfully",
		"data":    positions,
	})
}

// GetClosedPositions returns the authenticated user's closed positions.
// PRD API #14: GET /api/v1/positions/closed
func (h *Handler) GetClosedPositions(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		httpjson.Write(w, http.StatusUnauthorized, map[string]any{
			"success": false,
			"message": "Unauthorized",
		})
		return
	}

	positions, err := h.service.ListUserPositions(r.Context(), userID, PositionFilter{
		Status:   "closed",
		MatchID:  r.URL.Query().Get("matchId"),
		MarketID: r.URL.Query().Get("marketId"),
	})
	if err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"message": "Failed to fetch closed positions",
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Closed positions fetched successfully",
		"data":    positions,
	})
}

// GetPositionDetail returns a single position by its ID.
// PRD API #15: GET /api/v1/positions/{positionId}
func (h *Handler) GetPositionDetail(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		httpjson.Write(w, http.StatusUnauthorized, map[string]any{
			"success": false,
			"message": "Unauthorized",
		})
		return
	}

	positionID := r.PathValue("id")
	if positionID == "" {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid position ID",
		})
		return
	}

	position, err := h.service.GetUserPosition(r.Context(), userID, positionID)
	if err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid position ID",
		})
		return
	}
	if position == nil {
		httpjson.Write(w, http.StatusNotFound, map[string]any{
			"success": false,
			"message": "Position not found",
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Position fetched successfully",
		"data":    position,
	})
}

// ListAdminPositions returns positions across all users, with optional
// filters: userId, status, matchId, marketId.
// PRD API #28: GET /api/v1/admin/positions
func (h *Handler) ListAdminPositions(w http.ResponseWriter, r *http.Request) {
	userIDParam := r.URL.Query().Get("userId")
	status := r.URL.Query().Get("status")
	matchID := r.URL.Query().Get("matchId")
	marketID := r.URL.Query().Get("marketId")

	if status != "" && status != "open" && status != "closed" {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "status must be 'open' or 'closed'",
		})
		return
	}

	filter := PositionFilter{
		UserID:   userIDParam,
		Status:   status,
		MatchID:  matchID,
		MarketID: marketID,
	}

	positions, err := h.service.ListAdminPositions(r.Context(), filter)
	if err != nil {
		switch err {
		case errInvalidUserID:
			httpjson.Write(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": "Invalid userId",
			})
		default:
			httpjson.Write(w, http.StatusInternalServerError, map[string]any{
				"success": false,
				"message": "Failed to fetch positions",
			})
		}
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Positions fetched successfully",
		"data":    positions,
	})
}

// GetAdminPositionDetail returns any single position by its derived ID.
// PRD API #28 (per-id): GET /api/v1/admin/positions/{positionId}
func (h *Handler) GetAdminPositionDetail(w http.ResponseWriter, r *http.Request) {
	positionID := r.PathValue("id")
	if positionID == "" {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid position ID",
		})
		return
	}

	position, err := h.service.GetAdminPosition(r.Context(), positionID)
	if err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid position ID",
		})
		return
	}
	if position == nil {
		httpjson.Write(w, http.StatusNotFound, map[string]any{
			"success": false,
			"message": "Position not found",
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Position fetched successfully",
		"data":    position,
	})
}
