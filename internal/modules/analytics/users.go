package analytics

import (
	"context"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
)

// UserRow is one line of the admin users table.
type UserRow struct {
	ID                 string    `json:"id" bson:"_id"`
	Name               string    `json:"name" bson:"name"`
	Email              string    `json:"email" bson:"email"`
	Phone              string    `json:"phone,omitempty" bson:"phone,omitempty"`
	Tier               string    `json:"tier" bson:"tier"`
	Role               string    `json:"role" bson:"role"`
	CreatedAt time.Time `json:"createdAt" bson:"createdAt"`
	// Pointer so an account with no recorded activity serialises as null
	// rather than Go's zero time — `omitempty` does not omit a zero struct,
	// and "0001-01-01" reaching the UI would render as a real date.
	LastSeenAt         *time.Time `json:"lastSeenAt" bson:"lastSeenAt,omitempty"`
	SessionCount       int64      `json:"sessionCount" bson:"sessionCount"`
	TotalActiveSeconds int64      `json:"totalActiveSeconds" bson:"totalActiveSeconds"`
	// AvgSessionSeconds is this user's own average visit length.
	AvgSessionSeconds float64 `json:"avgSessionSeconds" bson:"avgSessionSeconds"`
	OrderCount        int64   `json:"orderCount" bson:"orderCount"`
	WalletBalance     float64 `json:"walletBalance" bson:"walletBalance"`
	// Status is "active" (seen in the last 24h), "dormant" (seen before
	// that) or "never" (registered but never made an authenticated request
	// since activity tracking shipped).
	Status string `json:"status" bson:"status"`
	// IsNew marks an account created within the trailing 7 days.
	IsNew bool `json:"isNew" bson:"isNew"`
}

type UsersPage struct {
	Users []UserRow `json:"users"`
	Total int64     `json:"total"`
	Page  int       `json:"page"`
	Limit int       `json:"limit"`
}

// userProjection derives the computed table columns. Shared by the list and
// the single-user detail so both report identical numbers.
func userProjection(now time.Time) bson.M {
	dayAgo := now.Add(-24 * time.Hour)
	weekAgo := now.Add(-7 * 24 * time.Hour)

	sessions := bson.M{"$ifNull": bson.A{"$sessionCount", 0}}
	active := bson.M{"$ifNull": bson.A{"$totalActiveSeconds", 0}}

	return bson.M{
		"name":               1,
		"email":              1,
		"phone":              1,
		"tier":               1,
		"role":               1,
		"createdAt":          1,
		"lastSeenAt":         1,
		"sessionCount":       sessions,
		"totalActiveSeconds": active,
		"avgSessionSeconds": bson.M{"$cond": bson.A{
			bson.M{"$gt": bson.A{sessions, 0}},
			bson.M{"$divide": bson.A{active, sessions}},
			0,
		}},
		"isNew": bson.M{"$gte": bson.A{"$createdAt", weekAgo}},
		"status": bson.M{"$switch": bson.M{
			"branches": bson.A{
				bson.M{"case": bson.M{"$eq": bson.A{bson.M{"$ifNull": bson.A{"$lastSeenAt", nil}}, nil}}, "then": "never"},
				bson.M{"case": bson.M{"$gte": bson.A{"$lastSeenAt", dayAgo}}, "then": "active"},
			},
			"default": "dormant",
		}},
	}
}

// ListUsers returns one page of the admin users table. search matches name or
// email (case-insensitive substring); status filters to active/dormant/never.
func (s *Service) ListUsers(ctx context.Context, search, status string, page, limit int) (UsersPage, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	now := time.Now().UTC()

	match := bson.M{}
	if s := strings.TrimSpace(search); s != "" {
		// Escaped so a user searching for "a.b" does not get regex behaviour.
		safe := regexpEscape(s)
		match["$or"] = bson.A{
			bson.M{"name": bson.M{"$regex": safe, "$options": "i"}},
			bson.M{"email": bson.M{"$regex": safe, "$options": "i"}},
		}
	}

	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: match}},
		bson.D{{Key: "$project", Value: userProjection(now)}},
	}
	if st := strings.TrimSpace(strings.ToLower(status)); st != "" && st != "all" {
		pipeline = append(pipeline, bson.D{{Key: "$match", Value: bson.M{"status": st}}})
	}

	// $facet runs the page and its count in one round trip.
	pipeline = append(pipeline,
		bson.D{{Key: "$sort", Value: bson.D{{Key: "createdAt", Value: -1}}}},
		bson.D{{Key: "$facet", Value: bson.M{
			"rows": bson.A{
				bson.M{"$skip": (page - 1) * limit},
				bson.M{"$limit": limit},
				// Joins are done after paging so they only touch one page.
				bson.M{"$lookup": bson.M{
					"from":         "orders",
					"localField":   "_id",
					"foreignField": "userId",
					"as":           "orders",
					"pipeline":     bson.A{bson.M{"$count": "n"}},
				}},
				bson.M{"$lookup": bson.M{
					"from":         "wallet_accounts",
					"localField":   "_id",
					"foreignField": "userId",
					"as":           "wallet",
					"pipeline":     bson.A{bson.M{"$project": bson.M{"availableBalance": 1}}},
				}},
				bson.M{"$set": bson.M{
					"orderCount":    bson.M{"$ifNull": bson.A{bson.M{"$first": "$orders.n"}, 0}},
					"walletBalance": bson.M{"$ifNull": bson.A{bson.M{"$first": "$wallet.availableBalance"}, 0}},
				}},
				bson.M{"$unset": bson.A{"orders", "wallet"}},
			},
			"total": bson.A{bson.M{"$count": "n"}},
		}}},
	)

	cur, err := s.users.Aggregate(ctx, pipeline)
	if err != nil {
		return UsersPage{}, err
	}
	defer cur.Close(ctx)

	var facet struct {
		Rows  []UserRow `bson:"rows"`
		Total []struct {
			N int64 `bson:"n"`
		} `bson:"total"`
	}
	if cur.Next(ctx) {
		if err := cur.Decode(&facet); err != nil {
			return UsersPage{}, err
		}
	}
	if err := cur.Err(); err != nil {
		return UsersPage{}, err
	}

	var total int64
	if len(facet.Total) > 0 {
		total = facet.Total[0].N
	}
	if facet.Rows == nil {
		facet.Rows = []UserRow{}
	}
	return UsersPage{Users: facet.Rows, Total: total, Page: page, Limit: limit}, nil
}

// UserDetail is the single-user view behind a row click.
type UserDetail struct {
	UserRow `bson:",inline"`
	// CurrentSessionSeconds is how long the in-progress visit has run, or 0
	// when the user is not currently active.
	CurrentSessionSeconds float64    `json:"currentSessionSeconds"`
	LoginAt               *time.Time `json:"loginAt" bson:"loginAt,omitempty"`
	FilledOrders          int64      `json:"filledOrders"`
	WalletReserved        float64    `json:"walletReserved"`
}

func (s *Service) UserDetail(ctx context.Context, id primitive.ObjectID) (UserDetail, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	now := time.Now().UTC()
	projection := userProjection(now)
	projection["loginAt"] = 1

	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.M{"_id": id}}},
		bson.D{{Key: "$project", Value: projection}},
		bson.D{{Key: "$lookup", Value: bson.M{
			"from":         "orders",
			"localField":   "_id",
			"foreignField": "userId",
			"as":           "orderStats",
			"pipeline": bson.A{bson.M{"$group": bson.M{
				"_id":    nil,
				"total":  bson.M{"$sum": 1},
				"filled": bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$eq": bson.A{"$status", "FILLED"}}, 1, 0}}},
			}}},
		}}},
		bson.D{{Key: "$lookup", Value: bson.M{
			"from":         "wallet_accounts",
			"localField":   "_id",
			"foreignField": "userId",
			"as":           "wallet",
			"pipeline":     bson.A{bson.M{"$project": bson.M{"availableBalance": 1, "reservedBalance": 1}}},
		}}},
		bson.D{{Key: "$set", Value: bson.M{
			"orderCount":     bson.M{"$ifNull": bson.A{bson.M{"$first": "$orderStats.total"}, 0}},
			"filledOrders":   bson.M{"$ifNull": bson.A{bson.M{"$first": "$orderStats.filled"}, 0}},
			"walletBalance":  bson.M{"$ifNull": bson.A{bson.M{"$first": "$wallet.availableBalance"}, 0}},
			"walletReserved": bson.M{"$ifNull": bson.A{bson.M{"$first": "$wallet.reservedBalance"}, 0}},
		}}},
		bson.D{{Key: "$unset", Value: bson.A{"orderStats", "wallet"}}},
	}

	cur, err := s.users.Aggregate(ctx, pipeline)
	if err != nil {
		return UserDetail{}, false, err
	}
	defer cur.Close(ctx)

	if !cur.Next(ctx) {
		return UserDetail{}, false, cur.Err()
	}

	var detail UserDetail
	if err := cur.Decode(&detail); err != nil {
		return UserDetail{}, false, err
	}
	// An in-progress visit is one whose last activity is still inside the
	// idle window; otherwise the session has ended and there is nothing to
	// report as "current".
	if detail.Status == "active" && detail.LoginAt != nil && detail.LastSeenAt != nil &&
		now.Sub(*detail.LastSeenAt) < auth.SessionIdleGap {
		detail.CurrentSessionSeconds = detail.LastSeenAt.Sub(*detail.LoginAt).Seconds()
	}
	return detail, true, cur.Err()
}

// regexpEscape quotes the characters that would otherwise be interpreted as
// regex syntax in a $regex search term.
func regexpEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
