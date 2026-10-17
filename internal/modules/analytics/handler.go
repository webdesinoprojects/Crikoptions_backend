package analytics

import (
	"net/http"
	"strconv"

	"go.mongodb.org/mongo-driver/bson/primitive"

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

// ListUsers handles GET /api/v1/admin/analytics/users (admin-only).
// Query params: search, status (active|dormant|never|all), page, limit.
func (h *Handler) ListUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	limit, _ := strconv.Atoi(q.Get("limit"))

	result, err := h.service.ListUsers(r.Context(), q.Get("search"), q.Get("status"), page, limit)
	if err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"message": "Failed to fetch users",
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Users fetched successfully",
		"data":    result,
	})
}

// GetUser handles GET /api/v1/admin/analytics/users/{userId} (admin-only).
func (h *Handler) GetUser(w http.ResponseWriter, r *http.Request) {
	id, err := primitive.ObjectIDFromHex(r.PathValue("userId"))
	if err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid user id",
		})
		return
	}

	detail, found, err := h.service.UserDetail(r.Context(), id)
	if err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"message": "Failed to fetch user",
		})
		return
	}
	if !found {
		httpjson.Write(w, http.StatusNotFound, map[string]any{
			"success": false,
			"message": "User not found",
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "User fetched successfully",
		"data":    detail,
	})
}
