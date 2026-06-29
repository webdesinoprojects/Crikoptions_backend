package positions

import (
	"context"
	"errors"
	"net/http"
	"time"

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

	ctx, cancel := positionsTimeout(r.Context())
	defer cancel()

	positions, err := h.service.ListUserPositions(ctx, userID, PositionFilter{
		Status:   "open",
		MatchID:  r.URL.Query().Get("matchId"),
		MarketID: r.URL.Query().Get("marketId"),
	})
	if err != nil {
		writePositionServiceError(w, "Failed to fetch open positions", err)
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

	ctx, cancel := positionsTimeout(r.Context())
	defer cancel()

	positions, err := h.service.ListUserPositions(ctx, userID, PositionFilter{
		Status:   "closed",
		MatchID:  r.URL.Query().Get("matchId"),
		MarketID: r.URL.Query().Get("marketId"),
	})
	if err != nil {
		writePositionServiceError(w, "Failed to fetch closed positions", err)
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

	ctx, cancel := positionsTimeout(r.Context())
	defer cancel()

	position, err := h.service.GetUserPosition(ctx, userID, positionID)
	if err != nil {
		writePositionServiceError(w, "Failed to fetch position", err)
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

	ctx, cancel := positionsTimeout(r.Context())
	defer cancel()

	positions, err := h.service.ListAdminPositions(ctx, filter)
	if err != nil {
		switch err {
		case errInvalidUserID:
			httpjson.Write(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": "Invalid userId",
			})
		default:
			writePositionServiceError(w, "Failed to fetch positions", err)
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

	ctx, cancel := positionsTimeout(r.Context())
	defer cancel()

	position, err := h.service.GetAdminPosition(ctx, positionID)
	if err != nil {
		writePositionServiceError(w, "Failed to fetch position", err)
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

func writePositionServiceError(w http.ResponseWriter, message string, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]any{
			"success": false,
			"message": message,
			"error": map[string]any{
				"code": "SERVICE_TIMEOUT",
			},
		})
		return
	}

	httpjson.Write(w, http.StatusInternalServerError, map[string]any{
		"success": false,
		"message": message,
	})
}

func positionsTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 2*time.Second)
}
