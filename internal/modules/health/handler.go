package health

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

type Handler struct{}

func NewHandler() *Handler {
	return &Handler{}
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
