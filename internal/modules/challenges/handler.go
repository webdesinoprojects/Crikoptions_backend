package challenges

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (h *Handler) GetChallenges(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	challenges, err := h.service.Evaluate(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": challenges})
}

// ClaimChallenge pays out a reward. The challenge ID comes from the path and
// nothing else about the reward is taken from the request, so the payout cannot
// be steered by the client.
func (h *Handler) ClaimChallenge(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	claimed, err := h.service.Claim(r.Context(), userID, r.PathValue("challengeId"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": claimed})
	case errors.Is(err, ErrUnknownChallenge):
		writeError(w, http.StatusNotFound, "unknown challenge")
	case errors.Is(err, ErrAlreadyClaimed):
		writeError(w, http.StatusConflict, "challenge reward already claimed")
	case errors.Is(err, ErrNotComplete):
		writeError(w, http.StatusForbidden, "challenge is not complete yet")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
