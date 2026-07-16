package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/shared/httpjson"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/store"
)

type Repository interface {
	ListLeagues(context.Context) ([]store.League, error)
	SetLeagueEnabled(context.Context, int64, bool) (bool, error)
	AdminStatus(context.Context, time.Time) (store.Status, error)
	GlobalTradingKilled(context.Context) (bool, error)
	SetGlobalTradingKill(context.Context, bool, time.Time) error
	ListIncidents(context.Context, int64) ([]store.Incident, error)
	RequestFixtureResync(context.Context, int64, time.Time) (bool, error)
	FixtureDiagnostics(context.Context, int64, bool) (store.FixtureDiagnostics, bool, error)
}

type Handler struct {
	repository Repository
	now        func() time.Time
}

func NewHandler(repository Repository) *Handler {
	return &Handler{repository: repository, now: time.Now}
}

func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	status, err := h.repository.AdminStatus(r.Context(), h.now().UTC())
	if err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "Could not load Sportmonks status"})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"success": true, "data": status})
}

func (h *Handler) Leagues(w http.ResponseWriter, r *http.Request) {
	leagues, err := h.repository.ListLeagues(r.Context())
	if err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "Could not load Sportmonks leagues"})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"success": true, "data": leagues})
}

func (h *Handler) SetLeagueEnabled(w http.ResponseWriter, r *http.Request) {
	leagueID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || leagueID <= 0 {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{"success": false, "message": "Invalid league ID"})
		return
	}
	var request struct {
		Enabled *bool `json:"enabled"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Enabled == nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{"success": false, "message": "enabled must be a boolean"})
		return
	}
	updated, err := h.repository.SetLeagueEnabled(r.Context(), leagueID, *request.Enabled)
	if err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "Could not update Sportmonks league"})
		return
	}
	if !updated {
		httpjson.Write(w, http.StatusNotFound, map[string]any{"success": false, "message": "Entitled league not found"})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"id": leagueID, "enabled": *request.Enabled}})
}

func (h *Handler) KillSwitch(w http.ResponseWriter, r *http.Request) {
	killed, err := h.repository.GlobalTradingKilled(r.Context())
	if err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "Could not load trading kill switch"})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"success": true, "data": map[string]bool{"killed": killed}})
}

func (h *Handler) SetKillSwitch(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Killed *bool `json:"killed"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Killed == nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{"success": false, "message": "killed must be a boolean"})
		return
	}
	now := h.now().UTC()
	if err := h.repository.SetGlobalTradingKill(r.Context(), *request.Killed, now); err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "Could not update trading kill switch"})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"success": true, "data": map[string]bool{"killed": *request.Killed}})
}

func (h *Handler) Incidents(w http.ResponseWriter, r *http.Request) {
	incidents, err := h.repository.ListIncidents(r.Context(), 100)
	if err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "Could not load provider incidents"})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"success": true, "data": incidents})
}

func (h *Handler) ResyncFixture(w http.ResponseWriter, r *http.Request) {
	fixtureID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || fixtureID <= 0 {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{"success": false, "message": "Invalid fixture ID"})
		return
	}
	updated, err := h.repository.RequestFixtureResync(r.Context(), fixtureID, h.now().UTC())
	if err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "Could not request fixture resync"})
		return
	}
	if !updated {
		httpjson.Write(w, http.StatusNotFound, map[string]any{"success": false, "message": "Eligible fixture not found"})
		return
	}
	httpjson.Write(w, http.StatusAccepted, map[string]any{"success": true, "data": map[string]int64{"fixtureId": fixtureID}})
}

func (h *Handler) FixtureDiagnostics(w http.ResponseWriter, r *http.Request) {
	fixtureID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || fixtureID <= 0 {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{"success": false, "message": "Invalid fixture ID"})
		return
	}
	includeRaw := r.URL.Query().Get("includeRaw") == "true"
	diagnostics, found, err := h.repository.FixtureDiagnostics(r.Context(), fixtureID, includeRaw)
	if err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "Could not load fixture diagnostics"})
		return
	}
	if !found {
		httpjson.Write(w, http.StatusNotFound, map[string]any{"success": false, "message": "Fixture not found"})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"success": true, "data": diagnostics})
}
