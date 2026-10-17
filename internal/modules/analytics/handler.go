package analytics

import (
	"net/http"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/shared/httpjson"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Overview handles GET /api/v1/admin/analytics/overview (admin-only, see
// routes.go). It is a read-only snapshot — no payload, no side effects.
func (h *Handler) Overview(w http.ResponseWriter, r *http.Request) {
	overview, err := h.service.Overview(r.Context())
	if err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"message": "Failed to compute analytics overview",
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Analytics overview fetched successfully",
		"data":    overview,
	})
}
