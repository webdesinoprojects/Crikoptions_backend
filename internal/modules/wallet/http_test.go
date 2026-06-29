package wallet_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/routes"
)

func TestWalletHTTPFundingFlow(t *testing.T) {
	authRepo := auth.NewInMemoryUserRepository()
	authSvc, err := auth.NewService(authRepo, "test-secret", time.Hour, []string{"admin@example.com"})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	authHandler := auth.NewHandler(authSvc)

	walletHandler := wallet.NewHandler(wallet.NewService(wallet.NewMemoryRepository()))
	router := routes.NewRouter(nil, nil, authHandler, nil, nil, nil, nil, nil, walletHandler, nil, nil)

	userToken, userID := registerAndLogin(t, router, "trader@example.com", "Trader User")
	adminToken, _ := registerAndLogin(t, router, "admin@example.com", "Admin User")

	status, body := requestJSON(t, router, http.MethodGet, "/api/v1/wallet", nil, userToken)
	if status != http.StatusOK {
		t.Fatalf("GET wallet status = %d body=%s", status, body)
	}
	walletData := readDataMap(t, body)
	if walletData["availableBalance"].(float64) != 0 {
		t.Fatalf("initial available = %v, want 0", walletData["availableBalance"])
	}

	status, _ = requestJSON(t, router, http.MethodPost, "/api/v1/admin/users/"+userID+"/wallet/credit", map[string]any{
		"amount": 1000,
		"reason": "non-admin attempt",
	}, userToken)
	if status != http.StatusForbidden {
		t.Fatalf("non-admin credit status = %d, want 403", status)
	}

	status, body = requestJSON(t, router, http.MethodPost, "/api/v1/admin/users/"+userID+"/wallet/credit", map[string]any{
		"amount": 2500,
		"reason": "initial paper balance",
	}, adminToken)
	if status != http.StatusOK {
		t.Fatalf("admin credit status = %d body=%s", status, body)
	}

	status, body = requestJSON(t, router, http.MethodPost, "/api/v1/admin/users/"+userID+"/wallet/debit", map[string]any{
		"amount": 500,
		"reason": "paper balance correction",
	}, adminToken)
	if status != http.StatusOK {
		t.Fatalf("admin debit status = %d body=%s", status, body)
	}

	status, _ = requestJSON(t, router, http.MethodPost, "/api/v1/admin/users/"+userID+"/wallet/debit", map[string]any{
		"amount": 5000,
		"reason": "oversized debit",
	}, adminToken)
	if status != http.StatusConflict {
		t.Fatalf("oversized debit status = %d, want 409", status)
	}

	status, body = requestJSON(t, router, http.MethodGet, "/api/v1/wallet", nil, userToken)
	if status != http.StatusOK {
		t.Fatalf("GET wallet after credit status = %d body=%s", status, body)
	}
	walletData = readDataMap(t, body)
	if walletData["availableBalance"].(float64) != 2000 {
		t.Fatalf("available = %v, want 2000", walletData["availableBalance"])
	}

	status, body = requestJSON(t, router, http.MethodGet, "/api/v1/wallet/ledger", nil, userToken)
	if status != http.StatusOK {
		t.Fatalf("GET ledger status = %d body=%s", status, body)
	}
	ledger := readDataSlice(t, body)
	if len(ledger) != 2 {
		t.Fatalf("ledger entries = %d, want 2", len(ledger))
	}
	entry := ledger[0].(map[string]any)
	if entry["type"] != wallet.LedgerAdminDebit {
		t.Fatalf("latest ledger type = %v, want %s", entry["type"], wallet.LedgerAdminDebit)
	}
	if entry["balanceAfter"].(float64) != 2000 {
		t.Fatalf("latest ledger balanceAfter = %v, want 2000", entry["balanceAfter"])
	}
}

func registerAndLogin(t *testing.T, handler http.Handler, email, name string) (token string, userID string) {
	t.Helper()

	status, body := requestJSON(t, handler, http.MethodPost, "/api/v1/auth/register", map[string]any{
		"name":     name,
		"email":    email,
		"password": "Password123",
	}, "")
	if status != http.StatusCreated {
		t.Fatalf("register %s status = %d body=%s", email, status, body)
	}

	status, body = requestJSON(t, handler, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"email":    email,
		"password": "Password123",
	}, "")
	if status != http.StatusOK {
		t.Fatalf("login %s status = %d body=%s", email, status, body)
	}

	data := readDataMap(t, body)
	token, _ = data["token"].(string)
	user, _ := data["user"].(map[string]any)
	userID, _ = user["id"].(string)
	if token == "" || userID == "" {
		t.Fatalf("login response missing token/user id: %s", body)
	}
	return token, userID
}

func requestJSON(t *testing.T, handler http.Handler, method, path string, payload any, token string) (int, []byte) {
	t.Helper()

	var body bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&body).Encode(payload); err != nil {
			t.Fatalf("encode request: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, &body)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func readDataMap(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode response: %v body=%s", err, body)
	}
	data, _ := decoded["data"].(map[string]any)
	if data == nil {
		t.Fatalf("response data is not an object: %s", body)
	}
	return data
}

func readDataSlice(t *testing.T, body []byte) []any {
	t.Helper()

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode response: %v body=%s", err, body)
	}
	data, _ := decoded["data"].([]any)
	if data == nil {
		t.Fatalf("response data is not an array: %s", body)
	}
	return data
}
