package challenges

import (
	"encoding/json"
	"net/http"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) GetChallenges(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	challenges, err := h.service.Evaluate(r.Context(), userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(challenges)
}
