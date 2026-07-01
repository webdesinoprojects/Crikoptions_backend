package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/shared/httpjson"
)

type Handler struct {
	service        *Service
	welcomeCreditor WelcomeCreditor
}

// WelcomeCreditor is implemented by the wallet service and called right after
// a new user is created to credit the signup bonus.
type WelcomeCreditor interface {
	ApplyWelcomeCredit(ctx context.Context, userID primitive.ObjectID) error
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// SetWelcomeCreditor wires in the wallet service after construction (avoids
// import cycles between auth and wallet packages).
func (h *Handler) SetWelcomeCreditor(wc WelcomeCreditor) {
	h.welcomeCreditor = wc
}

func (h *Handler) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r.Header.Get("Authorization"))
		if token == "" {
			httpjson.Write(w, http.StatusUnauthorized, map[string]any{
				"success": false,
				"message": "Missing Authorization header",
			})
			return
		}

		userID, role, err := h.service.ParseToken(token)
		if err != nil {
			httpjson.Write(w, http.StatusUnauthorized, map[string]any{
				"success": false,
				"message": "Invalid token",
			})
			return
		}

		r.Header.Set("X-Crik-User-Hash", shortHash(userID.Hex()))
		ctx := context.WithValue(r.Context(), CtxUserID, userID)
		ctx = context.WithValue(ctx, CtxRole, role)
		next(w, r.WithContext(ctx))
	}
}

// RequireAdmin wraps a handler so that only authenticated users with the
// "admin" role can access it. Must be chained after RequireAuth.
func (h *Handler) RequireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role, _ := r.Context().Value(CtxRole).(string)
		if role != "admin" {
			httpjson.Write(w, http.StatusForbidden, map[string]any{
				"success": false,
				"message": "Admin access required",
			})
			return
		}
		next(w, r)
	}
}

func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(r, &req); err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid request body",
		})
		return
	}

	user, err := h.service.Register(r.Context(), req)
	if err != nil {
		status, msg := mapAuthError(err)
		httpjson.Write(w, status, map[string]any{
			"success": false,
			"message": msg,
		})
		return
	}

	// Credit the ₹1,00,000 welcome bonus in a background goroutine so the
	// registration response is never delayed by the wallet write.
	if h.welcomeCreditor != nil {
		userID := user.ID
		go func(id primitive.ObjectID) {
			if credErr := h.welcomeCreditor.ApplyWelcomeCredit(context.Background(), id); credErr != nil {
				// Log only — do not fail registration if bonus credit fails.
				_ = credErr
			}
		}(userID)
	}

	httpjson.Write(w, http.StatusCreated, map[string]any{
		"success": true,
		"message": "User registered successfully",
		"data":    user,
	})
}


func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid request body",
		})
		return
	}

	user, token, err := h.service.Login(r.Context(), req)
	if err != nil {
		status, msg := mapAuthError(err)
		httpjson.Write(w, status, map[string]any{
			"success": false,
			"message": msg,
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Login successful",
		"data": map[string]any{
			"token": token,
			"user":  user,
		},
	})
}

func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	userID, ok := UserIDFromContext(r)
	if !ok || userID.IsZero() {
		httpjson.Write(w, http.StatusUnauthorized, map[string]any{
			"success": false,
			"message": "Unauthorized",
		})
		return
	}

	user, err := h.service.Me(r.Context(), userID)
	if err != nil {
		status, msg := mapAuthError(err)
		httpjson.Write(w, status, map[string]any{
			"success": false,
			"message": msg,
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Profile fetched successfully",
		"data":    user,
	})
}

func (h *Handler) UpdateMe(w http.ResponseWriter, r *http.Request) {
	userID, ok := UserIDFromContext(r)
	if !ok || userID.IsZero() {
		httpjson.Write(w, http.StatusUnauthorized, map[string]any{
			"success": false,
			"message": "Unauthorized",
		})
		return
	}

	var req updateMeRequest
	if err := decodeJSON(r, &req); err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Invalid request body",
		})
		return
	}

	user, err := h.service.UpdateMe(r.Context(), userID, req)
	if err != nil {
		status, msg := mapAuthError(err)
		httpjson.Write(w, status, map[string]any{
			"success": false,
			"message": msg,
		})
		return
	}

	httpjson.Write(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Profile updated successfully",
		"data":    user,
	})
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}
