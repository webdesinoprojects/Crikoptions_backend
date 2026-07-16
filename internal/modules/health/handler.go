package health

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

type ReadinessCheck func(context.Context) error

type Handler struct {
	ready ReadinessCheck
}

func NewHandler(checks ...ReadinessCheck) *Handler {
	var check ReadinessCheck
	if len(checks) > 0 {
		check = checks[0]
	}
	return &Handler{ready: check}
}

func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	status := http.StatusOK
	payload := map[string]string{"status": "ready"}
	if h.ready != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := h.ready(ctx); err != nil {
			status = http.StatusServiceUnavailable
			payload["status"] = "not_ready"
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	payload := map[string]string{"status": "ok"}
	addRenderMetadata(payload, "repo", "RENDER_GIT_REPO_SLUG")
	addRenderMetadata(payload, "branch", "RENDER_GIT_BRANCH")
	addRenderMetadata(payload, "commit", "RENDER_GIT_COMMIT")

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(payload)
}

func addRenderMetadata(payload map[string]string, field, envKey string) {
	if value := strings.TrimSpace(os.Getenv(envKey)); value != "" {
		payload[field] = value
	}
}
