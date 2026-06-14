package orders

import (
	"encoding/json"
	"errors"
	"net/http"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/shared/httpjson"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) GetOrders(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		httpjson.Write(w, http.StatusUnauthorized, map[string]any{
			"success": false,
			"message": "Unauthorized",
		})
		return
	}

	status := r.URL.Query().Get("status")
	matchID := r.URL.Query().Get("matchId")

	orders := h.service.GetUserOrders(r.Context(), userID, status, matchID)

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Orders fetched successfully",
		"data":   orders,
	})
}

// ListAdminOrders lets an admin view orders across all users. Optional query
// params: userId, status, matchId, marketId, side. userId is a 24-char hex
// ObjectID; an invalid value returns 400.
func (h *Handler) ListAdminOrders(w http.ResponseWriter, r *http.Request) {
	userIDParam := r.URL.Query().Get("userId")
	var userID primitive.ObjectID
	if userIDParam != "" {
		parsed, err := primitive.ObjectIDFromHex(userIDParam)
		if err != nil {
			httpjson.Write(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": "Invalid userId",
			})
			return
		}
		userID = parsed
	}

	status := r.URL.Query().Get("status")
	matchID := r.URL.Query().Get("matchId")
	marketID := r.URL.Query().Get("marketId")
	side := r.URL.Query().Get("side")

	if side != "" && side != "buy" && side != "sell" {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "side must be 'buy' or 'sell'",
		})
		return
	}

	orders := h.service.ListOrders(r.Context(), userID, status, matchID, marketID, side)

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Orders fetched successfully",
		"data":   orders,
	})
}

// GetAdminOrder returns a single order by ID, regardless of which user owns it.
// Used by the admin console for drilling into a specific order.
func (h *Handler) GetAdminOrder(w http.ResponseWriter, r *http.Request) {
	orderIDHex := r.PathValue("id")
	orderID, err := primitive.ObjectIDFromHex(orderIDHex)
	if err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid order ID",
		})
		return
	}

	order, err := h.service.GetOrderByID(r.Context(), orderID)
	if err != nil || order == nil {
		httpjson.Write(w, http.StatusNotFound, map[string]any{
			"success": false,
			"message": "Order not found",
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Order fetched successfully",
		"data":    order,
	})
}

func (h *Handler) CreateOrder(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		httpjson.Write(w, http.StatusUnauthorized, map[string]any{
			"success": false,
			"message": "Unauthorized",
		})
		return
	}

	var req CreateOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid request body",
		})
		return
	}

	if req.MatchID == "" || req.MarketID == "" {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Match ID and Market ID are required",
		})
		return
	}

	if req.Side != "buy" && req.Side != "sell" {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Side must be 'buy' or 'sell'",
		})
		return
	}

	if req.Quantity <= 0 {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Quantity must be positive",
		})
		return
	}

	if req.Strike <= 0 {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Strike must be positive",
		})
		return
	}

	order, err := h.service.CreateOrder(r.Context(), userID, req)
	if err != nil {
		switch {
		case errors.Is(err, ErrMarketNotFound):
			httpjson.Write(w, http.StatusNotFound, map[string]any{
				"success": false,
				"message": "Market not found",
			})
		case errors.Is(err, ErrMatchNotFound):
			httpjson.Write(w, http.StatusNotFound, map[string]any{
				"success": false,
				"message": "Match not found",
			})
		case errors.Is(err, ErrMarketNotTradable):
			httpjson.Write(w, http.StatusConflict, map[string]any{
				"success": false,
				"message": "Market is not open for trading",
			})
		case errors.Is(err, ErrMatchNotTradable):
			httpjson.Write(w, http.StatusConflict, map[string]any{
				"success": false,
				"message": "Match is not live for trading",
			})
		case errors.Is(err, ErrInsufficientBalance):
			httpjson.Write(w, http.StatusConflict, map[string]any{
				"success": false,
				"message": "Insufficient available wallet balance",
			})
		case errors.Is(err, ErrInsufficientPosition):
			httpjson.Write(w, http.StatusConflict, map[string]any{
				"success": false,
				"message": "Insufficient position to sell",
			})
		case errors.Is(err, ErrStrikeNotFound):
			httpjson.Write(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": "Strike not found in option chain",
			})
		case errors.Is(err, ErrInvalidSide):
			httpjson.Write(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": "Side must be 'buy' or 'sell'",
			})
		case errors.Is(err, ErrInvalidQuantity):
			httpjson.Write(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": "Quantity must be positive",
			})
		case errors.Is(err, ErrInvalidPrice):
			httpjson.Write(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": "Price must be positive",
			})
		case errors.Is(err, ErrInvalidStrike):
			httpjson.Write(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": "Strike must be positive",
			})
		default:
			httpjson.Write(w, http.StatusInternalServerError, map[string]any{
				"success": false,
				"message": "Failed to create order",
			})
		}
		return
	}

	httpjson.Write(w, http.StatusCreated, map[string]any{
		"success": true,
		"message": "Order created successfully",
		"data":    order,
	})
}

func (h *Handler) CancelOrder(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		httpjson.Write(w, http.StatusUnauthorized, map[string]any{
			"success": false,
			"message": "Unauthorized",
		})
		return
	}

	orderIDHex := r.PathValue("id")
	orderID, err := primitive.ObjectIDFromHex(orderIDHex)
	if err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid order ID",
		})
		return
	}

	order, err := h.service.CancelOrder(r.Context(), orderID, userID)
	if err != nil || order == nil {
		httpjson.Write(w, http.StatusNotFound, map[string]any{
			"success": false,
			"message": "Order not found or cannot be cancelled",
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Order cancelled successfully",
		"data":    order,
	})
}
