package simulator

import (
	"encoding/json"
	"net/http"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/shared/httpjson"
)

// Handler exposes simulator admin endpoints.
type Handler struct {
	svc *Service
}

// NewHandler creates a new simulator HTTP handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Start handles POST /api/v1/admin/matches/{id}/simulator/start
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	matchID := r.PathValue("id")
	var req StartRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	status, err := h.svc.Start(r.Context(), matchID, req)
	if err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Simulator started",
		"data":    status,
	})
}

// Pause handles POST /api/v1/admin/matches/{id}/simulator/pause
func (h *Handler) Pause(w http.ResponseWriter, r *http.Request) {
	status, err := h.svc.Pause(r.PathValue("id"))
	if err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Simulator paused",
		"data":    status,
	})
}

// Resume handles POST /api/v1/admin/matches/{id}/simulator/resume
func (h *Handler) Resume(w http.ResponseWriter, r *http.Request) {
	status, err := h.svc.Resume(r.PathValue("id"))
	if err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Simulator resumed",
		"data":    status,
	})
}

// Reset handles POST /api/v1/admin/matches/{id}/simulator/reset
func (h *Handler) Reset(w http.ResponseWriter, r *http.Request) {
	status, err := h.svc.Reset(r.Context(), r.PathValue("id"))
	if err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Simulator reset",
		"data":    status,
	})
}

// Status handles GET /api/v1/admin/matches/{id}/simulator/status
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	status := h.svc.Status(r.PathValue("id"))
	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Simulator status",
		"data":    status,
	})
}
