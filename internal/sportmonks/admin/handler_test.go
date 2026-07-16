package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/store"
)

type fakeRepository struct {
	leagues []store.League
	status  store.Status
	id      int64
	enabled bool
}

func (f *fakeRepository) ListLeagues(context.Context) ([]store.League, error) { return f.leagues, nil }
func (f *fakeRepository) AdminStatus(context.Context, time.Time) (store.Status, error) {
	return f.status, nil
}
func (f *fakeRepository) SetLeagueEnabled(_ context.Context, id int64, enabled bool) (bool, error) {
	f.id, f.enabled = id, enabled
	return true, nil
}
func (f *fakeRepository) GlobalTradingKilled(context.Context) (bool, error) { return false, nil }
func (f *fakeRepository) SetGlobalTradingKill(context.Context, bool, time.Time) error {
	return nil
}
func (f *fakeRepository) ListIncidents(context.Context, int64) ([]store.Incident, error) {
	return nil, nil
}
func (f *fakeRepository) RequestFixtureResync(context.Context, int64, time.Time) (bool, error) {
	return true, nil
}
func (f *fakeRepository) FixtureDiagnostics(context.Context, int64, bool) (store.FixtureDiagnostics, bool, error) {
	return store.FixtureDiagnostics{}, true, nil
}

func TestSetLeagueEnabled(t *testing.T) {
	repository := &fakeRepository{}
	handler := NewHandler(repository)
	request := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"enabled":true}`))
	request.SetPathValue("id", "7")
	response := httptest.NewRecorder()
	handler.SetLeagueEnabled(response, request)
	if response.Code != http.StatusOK || repository.id != 7 || !repository.enabled {
		t.Fatalf("status=%d update=%d/%t body=%s", response.Code, repository.id, repository.enabled, response.Body.String())
	}
}

func TestSetLeagueEnabledRejectsMissingBoolean(t *testing.T) {
	handler := NewHandler(&fakeRepository{})
	request := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{}`))
	request.SetPathValue("id", "7")
	response := httptest.NewRecorder()
	handler.SetLeagueEnabled(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
