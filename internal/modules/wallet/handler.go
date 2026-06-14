package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/shared/httpjson"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) GetWallet(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		writeUnauthorized(w)
		return
	}

	account, err := h.service.GetWallet(r.Context(), userID)
	if err != nil {
		writeServerError(w, "Failed to fetch wallet")
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Wallet fetched successfully",
		"data":    account,
	})
}

func (h *Handler) GetLedger(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		writeUnauthorized(w)
		return
	}

	entries, err := h.service.GetLedger(r.Context(), userID, parseLimit(r))
	if err != nil {
		writeServerError(w, "Failed to fetch wallet ledger")
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Wallet ledger fetched successfully",
		"data":    entries,
	})
}

func (h *Handler) AdminGetWallet(w http.ResponseWriter, r *http.Request) {
	userID, ok := parsePathUserID(w, r)
	if !ok {
		return
	}

	account, err := h.service.AdminGetWallet(r.Context(), userID)
	if err != nil {
		writeServerError(w, "Failed to fetch user wallet")
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "User wallet fetched successfully",
		"data":    account,
	})
}

func (h *Handler) AdminCredit(w http.ResponseWriter, r *http.Request) {
	h.handleFunding(w, r, h.service.AdminCredit, "Wallet credited successfully")
}

func (h *Handler) AdminDebit(w http.ResponseWriter, r *http.Request) {
	h.handleFunding(w, r, h.service.AdminDebit, "Wallet debited successfully")
}

func (h *Handler) AdminListLedger(w http.ResponseWriter, r *http.Request) {
	userID := primitive.ObjectID{}
	if raw := r.URL.Query().Get("userId"); raw != "" {
		parsed, err := ParseUserID(raw)
		if err != nil {
			writeBadRequest(w, "Invalid userId")
			return
		}
		userID = parsed
	}

	entries, err := h.service.AdminListLedger(r.Context(), userID, parseLimit(r))
	if err != nil {
		writeServerError(w, "Failed to fetch wallet ledger")
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Wallet ledger fetched successfully",
		"data":    entries,
	})
}

func (h *Handler) handleFunding(
	w http.ResponseWriter,
	r *http.Request,
	fn func(context.Context, primitive.ObjectID, primitive.ObjectID, FundingRequest) (*FundingResponse, error),
	message string,
) {
	adminID, ok := auth.UserIDFromContext(r)
	if !ok {
		writeUnauthorized(w)
		return
	}

	userID, ok := parsePathUserID(w, r)
	if !ok {
		return
	}

	var req FundingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBadRequest(w, "Invalid request body")
		return
	}

	result, err := fn(r.Context(), adminID, userID, req)
	if err != nil {
		switch {
		case errors.Is(err, errInvalidAmount):
			writeBadRequest(w, "Amount must be positive")
		case errors.Is(err, errInsufficientFunds):
			httpjson.Write(w, http.StatusConflict, map[string]any{
				"success": false,
				"message": "Insufficient available wallet balance",
			})
		default:
			writeServerError(w, "Failed to adjust wallet")
		}
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": message,
		"data":    result,
	})
}

func parsePathUserID(w http.ResponseWriter, r *http.Request) (primitive.ObjectID, bool) {
	userID, err := ParseUserID(r.PathValue("userId"))
	if err != nil {
		writeBadRequest(w, "Invalid userId")
		return primitive.ObjectID{}, false
	}
	return userID, true
}

func parseLimit(r *http.Request) int64 {
	limit, err := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
	if err != nil {
		return 50
	}
	return limit
}

func writeUnauthorized(w http.ResponseWriter) {
	httpjson.Write(w, http.StatusUnauthorized, map[string]any{
		"success": false,
		"message": "Unauthorized",
	})
}

func writeBadRequest(w http.ResponseWriter, message string) {
	httpjson.Write(w, http.StatusBadRequest, map[string]any{
		"success": false,
		"message": message,
	})
}

func writeServerError(w http.ResponseWriter, message string) {
	httpjson.Write(w, http.StatusInternalServerError, map[string]any{
		"success": false,
		"message": message,
	})
}
