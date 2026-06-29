package portfolio

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

func (h *Handler) GetSummary(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		writeUnauthorized(w)
		return
	}

	summary, err := h.service.GetSummary(r.Context(), userID)
	if err != nil {
		writeServerError(w, "Failed to fetch portfolio summary")
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

	daily, err := h.service.GetDailyPnL(r.Context(), userID)
	if err != nil {
		writeServerError(w, "Failed to fetch daily PnL")
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

	risk, err := h.service.GetRiskSummary(r.Context(), userID)
	if err != nil {
		writeServerError(w, "Failed to fetch risk summary")
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Risk summary fetched successfully",
		"data":    risk,
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

	pnl, err := h.service.GetMarketPnL(r.Context(), userID, marketID)
	if err != nil {
		writeServerError(w, "Failed to fetch market PnL")
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

func writeServerError(w http.ResponseWriter, message string) {
	httpjson.Write(w, http.StatusInternalServerError, map[string]any{
		"success": false,
		"message": message,
	})
}
