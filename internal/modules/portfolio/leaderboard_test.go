package portfolio

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/markets"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/positions"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/wallet"
)

type leaderboardUsers struct {
	users []auth.User
}

func (l *leaderboardUsers) ListAll(context.Context) ([]auth.User, error) {
	return l.users, nil
}

type perUserPositions struct {
	openByUser map[primitive.ObjectID][]positions.Position
}

func (p perUserPositions) GetUserOpenPositions(_ context.Context, userID primitive.ObjectID) ([]positions.Position, error) {
	return p.openByUser[userID], nil
}

func (p perUserPositions) GetUserClosedPositions(context.Context, primitive.ObjectID) ([]positions.Position, error) {
	return nil, nil
}

type perUserWallet struct {
	accounts map[primitive.ObjectID]*wallet.Account
}

func (p perUserWallet) GetWallet(_ context.Context, userID primitive.ObjectID) (*wallet.Account, error) {
	return p.accounts[userID], nil
}

func TestGetLeaderboardRanksByROI(t *testing.T) {
	userA := primitive.NewObjectID()
	userB := primitive.NewObjectID()
	marketID := "market-1"

	svc := NewService(
		perUserPositions{openByUser: map[primitive.ObjectID][]positions.Position{
			userA: {{
				ID: "open-a", UserID: userA, MatchID: "match-1", MarketID: marketID,
				Status: "open", Lots: 10, BuyPrice: 10, LTP: 20, PnL: 100,
			}},
		}},
		perUserWallet{accounts: map[primitive.ObjectID]*wallet.Account{
			userA: {UserID: userA, CashBalance: 1000, AvailableBalance: 1000},
			userB: {UserID: userB, CashBalance: 1000, AvailableBalance: 1000},
		}},
		stubMarkets{items: map[string]*markets.Market{marketID: {Title: "Market"}}},
		stubMatches{items: map[string]*matches.Match{"match-1": {TeamAName: "A", TeamBName: "B"}}},
		&leaderboardUsers{users: []auth.User{
			{ID: userA, Name: "Alice"},
			{ID: userB, Name: "Bob"},
		}},
	)

	rows, err := svc.GetLeaderboard(context.Background())
	if err != nil {
		t.Fatalf("GetLeaderboard: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Rank != 1 || rows[0].Name != "Alice" {
		t.Fatalf("first row = %+v, want Alice at rank 1", rows[0])
	}
	if rows[0].ROI <= rows[1].ROI {
		t.Fatalf("roi order = %.2f then %.2f, want Alice higher", rows[0].ROI, rows[1].ROI)
	}
	if rows[0].Country != "India" || rows[1].Country != "India" {
		t.Fatalf("country = %q / %q, want India for all", rows[0].Country, rows[1].Country)
	}
}
