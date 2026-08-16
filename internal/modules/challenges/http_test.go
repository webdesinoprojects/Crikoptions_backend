package challenges_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/challenges"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/positions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/routes"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"context"
	"strings"
)

type stubPositions struct{ book []positions.Position }

func (s stubPositions) ListUserPositions(context.Context, primitive.ObjectID, positions.PositionFilter) ([]positions.Position, error) {
	return s.book, nil
}

type stubWallet struct {
	paid   float64
	ledger []wallet.LedgerEntry
}

func (s *stubWallet) CreditChallengeReward(_ context.Context, _ primitive.ObjectID, id, _ string, amount float64) (*wallet.AdjustmentResult, error) {
	s.paid += amount
	s.ledger = append(s.ledger, wallet.LedgerEntry{Type: wallet.LedgerChallengeReward, ReferenceID: id})
	return &wallet.AdjustmentResult{}, nil
}

func (s *stubWallet) CreditDailyChallengeReward(_ context.Context, _ primitive.ObjectID, id, dateUTC, _ string, amount float64) (*wallet.AdjustmentResult, error) {
	s.paid += amount
	s.ledger = append(s.ledger, wallet.LedgerEntry{Type: wallet.LedgerChallengeReward, ReferenceID: id + ":" + dateUTC})
	return &wallet.AdjustmentResult{}, nil
}

func (s *stubWallet) GetLedger(context.Context, primitive.ObjectID, int64) ([]wallet.LedgerEntry, error) {
	return s.ledger, nil
}

// newServer wires the real router so the test exercises method, path and auth
// exactly as a browser would.
func newServer(t *testing.T, book []positions.Position) (http.Handler, *stubWallet, string) {
	t.Helper()
	authSvc, err := auth.NewService(auth.NewInMemoryUserRepository(), "test-secret", time.Hour, nil)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	w := &stubWallet{}
	handler := challenges.NewHandler(challenges.NewService(stubPositions{book: book}, w))
	router := routes.NewRouter(nil, nil, auth.NewHandler(authSvc), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, handler)
	return router, w, registerAndLogin(t, router)
}

func registerAndLogin(t *testing.T, handler http.Handler) string {
	t.Helper()
	body := `{"name":"Trader","email":"trader@example.com","password":"Password123"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"email":"trader@example.com","password":"Password123"}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
	var parsed struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil || parsed.Data.Token == "" {
		t.Fatalf("no token in login response: %s", rec.Body.String())
	}
	return parsed.Data.Token
}

func do(t *testing.T, handler http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestClaimEndpointRefusesAnUnearnedChallenge(t *testing.T) {
	router, w, token := newServer(t, nil) // user has never traded

	rec := do(t, router, http.MethodPost, "/api/v1/challenges/lc-1/claim", token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if w.paid != 0 {
		t.Fatalf("wallet paid %v for an unearned challenge", w.paid)
	}
}

func TestChallengeEndpointsRequireAuth(t *testing.T) {
	router, w, _ := newServer(t, nil)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/challenges"},
		{http.MethodPost, "/api/v1/challenges/lc-1/claim"},
	} {
		if rec := do(t, router, tc.method, tc.path, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status=%d, want 401", tc.method, tc.path, rec.Code)
		}
	}
	if w.paid != 0 {
		t.Fatalf("wallet paid %v on an unauthenticated request", w.paid)
	}
}

func TestClaimEndpointPaysOnceForAnEarnedChallenge(t *testing.T) {
	earned := []positions.Position{{
		MatchID: "m1", MarketID: "k1", Side: "BUY",
		MatchedLots: 1, RealizedPnL: 25, Status: "closed", UpdatedAt: time.Now().UTC(),
	}}
	router, w, token := newServer(t, earned)

	rec := do(t, router, http.MethodPost, "/api/v1/challenges/lc-1/claim", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var body struct {
		Data challenges.Challenge `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Data.Claimed || body.Data.Reward <= 0 {
		t.Fatalf("claimed=%v reward=%v", body.Data.Claimed, body.Data.Reward)
	}
	firstPayout := w.paid

	// Replaying the same request must not pay again.
	if rec := do(t, router, http.MethodPost, "/api/v1/challenges/lc-1/claim", token); rec.Code != http.StatusConflict {
		t.Fatalf("replay status=%d, want 409", rec.Code)
	}
	if w.paid != firstPayout {
		t.Fatalf("replay paid again: %v -> %v", firstPayout, w.paid)
	}
}

func TestListEndpointReportsServerDerivedProgress(t *testing.T) {
	router, _, token := newServer(t, []positions.Position{{
		MatchID: "m1", MarketID: "k1", Side: "SELL", Lots: 3, Status: "open",
	}})

	rec := do(t, router, http.MethodGet, "/api/v1/challenges", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []challenges.Challenge `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) == 0 {
		t.Fatal("no challenges returned")
	}
	seen := map[string]bool{}
	for _, c := range body.Data {
		seen[c.ID] = true
		switch c.ID {
		case "sc-1":
			if c.Status != challenges.StatusComplete {
				t.Fatalf("sc-1 = %s, want COMPLETE after a sell", c.Status)
			}
		case "lc-1":
			if c.Status == challenges.StatusComplete {
				t.Fatal("a SELL completed a long-call challenge")
			}
		case challenges.DailyPowerplayPro, challenges.DailyMiddleOverGenius, challenges.DailyDeathOverAssassin, challenges.DailyLastOverHero:
			if c.AcademyID != "today" {
				t.Fatalf("%s academyId=%s, want today", c.ID, c.AcademyID)
			}
			if c.Status == challenges.StatusLocked {
				t.Fatalf("%s must never be LOCKED", c.ID)
			}
			if c.Progress != 0 {
				t.Fatalf("%s progress=%d, want 0 with no closes", c.ID, c.Progress)
			}
		}
	}
	for _, id := range []string{
		challenges.DailyPowerplayPro, challenges.DailyMiddleOverGenius,
		challenges.DailyDeathOverAssassin, challenges.DailyLastOverHero,
		"lc-1", "sc-1",
	} {
		if !seen[id] {
			t.Fatalf("GET /challenges missing %s", id)
		}
	}
}
