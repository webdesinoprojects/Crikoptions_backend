package markets

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/shared/httpjson"
)

type Handler struct {
	service      *Service
	matchHistory MatchHistoryProvider
}

type MatchHistoryProvider interface {
	GetMatchByID(ctx context.Context, id string) (*matches.Match, error)
	GetInningsEvents(ctx context.Context, id string, innings, limit int) ([]matches.BallEvent, error)
}

func NewHandler(service *Service, history ...MatchHistoryProvider) *Handler {
	var provider MatchHistoryProvider
	if len(history) > 0 {
		provider = history[0]
	}
	return &Handler{service: service, matchHistory: provider}
}

func (h *Handler) GetMarketsByMatchID(w http.ResponseWriter, r *http.Request) {
	matchID := r.PathValue("id")
	if matchID == "" {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid match ID",
		})
		return
	}

	markets := h.service.GetMarketsByMatchID(r.Context(), matchID)
	if markets == nil {
		markets = []Market{}
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Markets fetched successfully",
		"data":    markets,
	})
}

func (h *Handler) GetMarketDetail(w http.ResponseWriter, r *http.Request) {
	marketID := r.PathValue("id")
	if marketID == "" {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid market ID",
		})
		return
	}

	market, err := h.service.GetMarketByID(r.Context(), marketID)
	if err != nil || market == nil {
		httpjson.Write(w, http.StatusNotFound, map[string]any{
			"success": false,
			"message": "Market not found",
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Market detail fetched successfully",
		"data":    market,
	})
}

func (h *Handler) GetOptionChainHistory(w http.ResponseWriter, r *http.Request) {
	if h.matchHistory == nil {
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]any{
			"success": false,
			"message": "Match history is unavailable",
		})
		return
	}

	marketID := r.PathValue("id")
	if marketID == "" {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid market ID",
		})
		return
	}

	market, err := h.service.GetMarketByID(r.Context(), marketID)
	if err != nil || market == nil {
		httpjson.Write(w, http.StatusNotFound, map[string]any{
			"success": false,
			"message": "Market not found",
		})
		return
	}

	match, err := h.matchHistory.GetMatchByID(r.Context(), market.MatchID)
	if err != nil || match == nil {
		httpjson.Write(w, http.StatusNotFound, map[string]any{
			"success": false,
			"message": "Match not found",
		})
		return
	}

	limit := parseHistoryLimit(r.URL.Query().Get("limit"))
	events, err := h.matchHistory.GetInningsEvents(r.Context(), match.ID.Hex(), match.Innings, limit)
	if err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"message": "Failed to fetch option chain history",
		})
		return
	}

	history, err := h.service.BuildOptionChainHistory(*market, *match, events)
	if err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"message": "Failed to build option chain history",
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Option chain history fetched successfully",
		"data":    history,
	})
}

// CreateMarket (admin) attaches a new tradable market to a match.
func (h *Handler) CreateMarket(w http.ResponseWriter, r *http.Request) {
	var req CreateMarketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid request body",
		})
		return
	}

	market, err := h.service.CreateMarket(r.Context(), req)
	if err != nil {
		switch err {
		case errInvalidMarket:
			httpjson.Write(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": "matchId and title are required and prices must be non-negative",
			})
		case errInvalidStatus:
			httpjson.Write(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": "status must be active, suspended, or closed",
			})
		default:
			httpjson.Write(w, http.StatusInternalServerError, map[string]any{
				"success": false,
				"message": "Failed to create market",
			})
		}
		return
	}

	httpjson.Write(w, http.StatusCreated, map[string]any{
		"success": true,
		"message": "Market created successfully",
		"data":    market,
	})
}

// SuspendMarket (admin) halts trading on a market.
func (h *Handler) SuspendMarket(w http.ResponseWriter, r *http.Request) {
	h.setStatus(w, r, MarketStatusSuspended, "Market suspended")
}

// ResumeMarket (admin) re-enables trading on a market.
func (h *Handler) ResumeMarket(w http.ResponseWriter, r *http.Request) {
	h.setStatus(w, r, MarketStatusActive, "Market resumed")
}

func (h *Handler) setStatus(w http.ResponseWriter, r *http.Request, status, okMessage string) {
	marketID := r.PathValue("id")
	if marketID == "" {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid market ID",
		})
		return
	}

	market, err := h.service.SetMarketStatus(r.Context(), marketID, status)
	if err != nil {
		if err == errMarketNotFound {
			httpjson.Write(w, http.StatusNotFound, map[string]any{
				"success": false,
				"message": "Market not found",
			})
			return
		}
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"message": "Failed to update market status",
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": okMessage,
		"data":    market,
	})
}

func (h *Handler) CalculatePrice(w http.ResponseWriter, r *http.Request) {
	marketID := r.PathValue("id")
	if marketID == "" {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid market ID",
		})
		return
	}

	var req struct {
		Innings      int `json:"innings"`
		CurrentScore int `json:"currentScore"`
		WicketsLost  int `json:"wicketsLost"`
		BallsLeft    int `json:"ballsLeft"`
		BallsBowled  int `json:"ballsBowled"`
		TargetScore  int `json:"targetScore"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid request body",
		})
		return
	}

	if req.Innings != 1 && req.Innings != 2 {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "innings must be 1 or 2",
		})
		return
	}
	if req.WicketsLost < 0 || req.WicketsLost > 10 {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "wicketsLost must be between 0 and 10",
		})
		return
	}
	if req.Innings == 1 && (req.BallsLeft < 0 || req.BallsLeft > 120) {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "ballsLeft must be between 0 and 120 for 1st innings",
		})
		return
	}
	if req.Innings == 2 {
		if req.TargetScore <= 0 {
			httpjson.Write(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": "targetScore must be positive for 2nd innings",
			})
			return
		}
		if req.BallsBowled < 0 || req.BallsBowled > 120 {
			httpjson.Write(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": "ballsBowled must be between 0 and 120 for 2nd innings",
			})
			return
		}
	}

	result, err := h.service.CalculatePrice(PriceCalculationInput{
		MatchID:      marketID,
		Innings:      req.Innings,
		CurrentScore: req.CurrentScore,
		WicketsLost:  req.WicketsLost,
		BallsLeft:    req.BallsLeft,
		BallsBowled:  req.BallsBowled,
		TargetScore:  req.TargetScore,
	})
	if err != nil {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"message": "Failed to calculate price",
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Price calculated successfully",
		"data":    result,
	})
}

func parseHistoryLimit(raw string) int {
	const (
		defaultLimit = 240
		maxLimit     = 400
	)
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultLimit
	}
	limit, err := strconv.Atoi(trimmed)
	if err != nil || limit <= 0 {
		return defaultLimit
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}
