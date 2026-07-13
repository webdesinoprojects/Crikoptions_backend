package portfolio

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

func (h *Handler) GetSummary(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		writeUnauthorized(w)
		return
	}

	ctx, cancel := portfolioTimeout(r.Context())
	defer cancel()

	summary, err := h.service.GetSummary(ctx, userID)
	if err != nil {
		writeServiceError(w, "Failed to fetch portfolio summary", err)
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Portfolio summary fetched successfully",
		"data":    summary,
	})
}

func (h *Handler) GetDailyPnL(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		writeUnauthorized(w)
		return
	}

	ctx, cancel := portfolioTimeout(r.Context())
	defer cancel()

	daily, err := h.service.GetDailyPnL(ctx, userID)
	if err != nil {
		writeServiceError(w, "Failed to fetch daily PnL", err)
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Daily PnL fetched successfully",
		"data":    daily,
	})
}

func (h *Handler) GetRiskSummary(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		writeUnauthorized(w)
		return
	}

	ctx, cancel := portfolioTimeout(r.Context())
	defer cancel()

	risk, err := h.service.GetRiskSummary(ctx, userID)
	if err != nil {
		writeServiceError(w, "Failed to fetch risk summary", err)
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Risk summary fetched successfully",
		"data":    risk,
	})
}

// GetLeaderboard returns all users ranked by portfolio ROI (public).
func (h *Handler) GetLeaderboard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	rows, err := h.service.GetLeaderboard(ctx)
	if err != nil {
		writeServiceError(w, "Failed to fetch leaderboard", err)
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Leaderboard fetched successfully",
		"data":    rows,
	})
}

func (h *Handler) GetMarketPnL(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		writeUnauthorized(w)
		return
	}

	marketID := r.PathValue("marketId")
	if marketID == "" {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid market ID",
		})
		return
	}

	ctx, cancel := portfolioTimeout(r.Context())
	defer cancel()

	pnl, err := h.service.GetMarketPnL(ctx, userID, marketID)
	if err != nil {
		writeServiceError(w, "Failed to fetch market PnL", err)
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Market PnL fetched successfully",
		"data":    pnl,
	})
}

func writeUnauthorized(w http.ResponseWriter) {
	httpjson.Write(w, http.StatusUnauthorized, map[string]any{
		"success": false,
		"message": "Unauthorized",
	})
}

func writeServiceError(w http.ResponseWriter, message string, err error) {
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

func portfolioTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 2*time.Second)
}
