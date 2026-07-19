package matches

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/shared/httpjson"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) GetHomeMatches(w http.ResponseWriter, r *http.Request) {
	matches := h.service.GetHomeMatches(r.Context())

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Matches fetched successfully",
		"data":    matches,
	})
}

func (h *Handler) GetUpcomingMatches(w http.ResponseWriter, r *http.Request) {
	matches := h.service.GetUpcomingMatches(r.Context())

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Upcoming matches fetched successfully",
		"data":    matches,
	})
}

func (h *Handler) GetMatchDetail(w http.ResponseWriter, r *http.Request) {
	match, err := h.service.GetMatchByID(r.Context(), r.PathValue("id"))
	if err != nil || match == nil {
		httpjson.Write(w, http.StatusNotFound, map[string]any{
			"success": false,
			"message": "Match not found",
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Match detail fetched successfully",
		"data":    match,
	})
}

func (h *Handler) CreateMatch(w http.ResponseWriter, r *http.Request) {
	var req CreateMatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid request body",
		})
		return
	}

	if req.TeamAName == "" || req.TeamBName == "" {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Team names are required",
		})
		return
	}

	match, err := h.service.CreateMatch(r.Context(), req)
	if err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"message": "Failed to create match",
		})
		return
	}

	httpjson.Write(w, http.StatusCreated, map[string]any{
		"success": true,
		"message": "Match created successfully",
		"data":    match,
	})
}

func (h *Handler) UpdateMatchScore(w http.ResponseWriter, r *http.Request) {
	var req UpdateScoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid request body",
		})
		return
	}

	if req.CurrentScore < 0 || req.WicketsLost < 0 || req.BallsLeft < 0 || (req.TargetScore != nil && *req.TargetScore < 0) {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid score values",
		})
		return
	}

	if req.BallsLeft > BallsODI {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Balls left cannot exceed format maximum",
		})
		return
	}

	match, err := h.service.UpdateMatchScore(r.Context(), r.PathValue("id"), req)
	if err != nil {
		switch {
		case errors.Is(err, errMatchNotFound):
			httpjson.Write(w, http.StatusNotFound, map[string]any{
				"success": false,
				"message": "Match not found",
			})
		case errors.Is(err, errProviderOwnedMatch):
			httpjson.Write(w, http.StatusConflict, map[string]any{
				"success": false,
				"code":    "PROVIDER_OWNED_MATCH",
				"message": "Provider-owned matches cannot be edited manually",
			})
		default:
			httpjson.Write(w, http.StatusInternalServerError, map[string]any{
				"success": false,
				"message": "Failed to update match score",
			})
		}
		return
	}
	if match == nil {
		httpjson.Write(w, http.StatusNotFound, map[string]any{
			"success": false,
			"message": "Match not found",
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Match score updated successfully",
		"data":    match,
	})
}

func (h *Handler) UpdateLiveContext(w http.ResponseWriter, r *http.Request) {
	var req UpdateLiveContextRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid request body",
		})
		return
	}

	match, err := h.service.UpdateLiveContext(r.Context(), r.PathValue("id"), req)
	if err != nil {
		switch {
		case errors.Is(err, errMatchNotFound):
			httpjson.Write(w, http.StatusNotFound, map[string]any{"success": false, "message": "Match not found"})
		case errors.Is(err, errInvalidLiveContext):
			httpjson.Write(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": "Striker, non-striker, and bowler names are required and figures cannot be negative",
			})
		case errors.Is(err, errProviderOwnedMatch):
			httpjson.Write(w, http.StatusConflict, map[string]any{
				"success": false,
				"code":    "PROVIDER_OWNED_MATCH",
				"message": "Provider-owned matches cannot be edited manually",
			})
		default:
			httpjson.Write(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "Failed to update live match context"})
		}
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Live match context updated",
		"data":    match,
	})
}

// GetMatchEvents returns per-ball events for the match's current over
// so a late-joining client can render "This over" in sync with oversText.
// GET /api/v1/matches/{id}/events?limit=6
func (h *Handler) GetMatchEvents(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Has("innings") || r.URL.Query().Has("afterSequence") || r.URL.Query().Get("history") == "true" {
		h.getInningsEvents(w, r)
		return
	}
	limit := 6
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	events, err := h.service.GetRecentEvents(r.Context(), r.PathValue("id"), limit)
	if err != nil {
		if errors.Is(err, errMatchNotFound) {
			httpjson.Write(w, http.StatusNotFound, map[string]any{
				"success": false,
				"message": "Match not found",
			})
			return
		}
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"message": "Failed to fetch match events",
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Match events fetched successfully",
		"data":    events,
	})
}

func (h *Handler) getInningsEvents(w http.ResponseWriter, r *http.Request) {
	innings, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("innings")))
	afterSequence, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("afterSequence")), 10, 64)
	if err != nil && strings.TrimSpace(r.URL.Query().Get("afterSequence")) != "" {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{"success": false, "message": "Invalid sequence cursor"})
		return
	}
	limit := 100
	if value, parseErr := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit"))); parseErr == nil && value > 0 {
		limit = min(value, 500)
	}
	events, err := h.service.GetInningsEventsAfter(r.Context(), r.PathValue("id"), innings, afterSequence, limit)
	if err != nil {
		status := http.StatusInternalServerError
		message := "Failed to fetch innings events"
		if errors.Is(err, errMatchNotFound) {
			status, message = http.StatusNotFound, "Match not found"
		}
		httpjson.Write(w, status, map[string]any{"success": false, "message": message})
		return
	}
	var nextSequence any
	if len(events) == limit {
		nextSequence = events[len(events)-1].Sequence
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true, "message": "Innings events fetched successfully",
		"data": events, "nextSequence": nextSequence,
	})
}

func (h *Handler) RecordBall(w http.ResponseWriter, r *http.Request) {
	var req BallEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid request body",
		})
		return
	}

	match, err := h.service.RecordBall(r.Context(), r.PathValue("id"), req)
	if err != nil {
		switch {
		case errors.Is(err, errMatchNotFound):
			httpjson.Write(w, http.StatusNotFound, map[string]any{
				"success": false,
				"message": "Match not found",
			})
		case errors.Is(err, errMatchNotLiveBall):
			httpjson.Write(w, http.StatusConflict, map[string]any{
				"success": false,
				"message": "Match must be live to record balls",
			})
		case errors.Is(err, errProviderOwnedMatch):
			httpjson.Write(w, http.StatusConflict, map[string]any{
				"success": false,
				"code":    "PROVIDER_OWNED_MATCH",
				"message": "Provider-owned matches cannot be edited manually",
			})
		case errors.Is(err, errInvalidBallEvent):
			httpjson.Write(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": "Invalid ball event",
			})
		case errors.Is(err, errNextBatterRequired):
			httpjson.Write(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": "Enter the incoming batter before recording a wicket",
			})
		default:
			httpjson.Write(w, http.StatusInternalServerError, map[string]any{
				"success": false,
				"message": "Failed to record ball",
			})
		}
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Ball recorded successfully",
		"data":    match,
	})
}

func (h *Handler) StartMatch(w http.ResponseWriter, r *http.Request) {
	match, err := h.service.StartMatch(r.Context(), r.PathValue("id"))
	if err != nil {
		writeMatchActionError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Match started",
		"data":    match,
	})
}

func (h *Handler) CompleteMatch(w http.ResponseWriter, r *http.Request) {
	match, err := h.service.CompleteMatch(r.Context(), r.PathValue("id"))
	if err != nil {
		writeMatchActionError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Match completed",
		"data":    match,
	})
}

func writeMatchActionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errMatchNotFound):
		httpjson.Write(w, http.StatusNotFound, map[string]any{
			"success": false,
			"message": "Match not found",
		})
	case errors.Is(err, errMatchAlreadyLive):
		httpjson.Write(w, http.StatusConflict, map[string]any{
			"success": false,
			"message": "Match is already live",
		})
	case errors.Is(err, errMatchNotLive):
		httpjson.Write(w, http.StatusConflict, map[string]any{
			"success": false,
			"message": "Match is not live",
		})
	case errors.Is(err, errInvalidTransition):
		httpjson.Write(w, http.StatusConflict, map[string]any{
			"success": false,
			"message": "Invalid match status transition",
		})
	case errors.Is(err, errProviderOwnedMatch):
		httpjson.Write(w, http.StatusConflict, map[string]any{
			"success": false,
			"code":    "PROVIDER_OWNED_MATCH",
			"message": "Provider-owned matches cannot be edited manually",
		})
	default:
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"message": "Match action failed",
		})
	}
}
