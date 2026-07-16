package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthIncludesRenderDeploymentMetadata(t *testing.T) {
	t.Setenv("RENDER_GIT_REPO_SLUG", "webdesinoprojects/Crikoptions_backend")
	t.Setenv("RENDER_GIT_BRANCH", "main")
	t.Setenv("RENDER_GIT_COMMIT", "8464e70")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	NewHandler().Health(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var response map[string]string
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := map[string]string{
		"status": "ok",
		"repo":   "webdesinoprojects/Crikoptions_backend",
		"branch": "main",
		"commit": "8464e70",
	}
	for field, expected := range want {
		if response[field] != expected {
			t.Errorf("%s = %q, want %q", field, response[field], expected)
		}
	}
}

func TestReadyFailsClosedWhenDependencyFails(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/ready", nil)
	NewHandler(func(context.Context) error { return errors.New("unavailable") }).Ready(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}
