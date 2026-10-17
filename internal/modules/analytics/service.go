// Package analytics computes admin-facing usage metrics (total users,
// signup growth, daily active users, average session length) by reading
// the users collection. It owns no data of its own — auth.Service writes
// the loginAt/lastSeenAt fields this package reads.
package analytics

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

type Service struct {
	users *mongo.Collection
}

func NewService(db *mongo.Database) *Service {
	return &Service{users: db.Collection("users")}
}

// Overview is the admin analytics snapshot returned by GET
// /api/v1/admin/analytics/overview.
type Overview struct {
	TotalUsers int64 `json:"totalUsers"`
	// NewUsersToday counts users whose createdAt falls on the current UTC
	// calendar day.
	NewUsersToday int64 `json:"newUsersToday"`
	// NewUsersThisWeek counts users created in the trailing 7 days
	// (rolling window, not calendar week).
	NewUsersThisWeek int64 `json:"newUsersThisWeek"`
	// DailyActiveUsers counts users whose lastSeenAt falls in the
	// trailing 24 hours. lastSeenAt is refreshed (at most once a minute
	// per user) on every authenticated request.
	DailyActiveUsers int64 `json:"dailyActiveUsers"`
	// AvgSessionMinutes is the average of (lastSeenAt - loginAt) across
	// users active in the trailing 24 hours, where a session is reset
	// after 30 minutes of inactivity (auth.sessionIdleGap).
	//
	// Still an approximation: the clock stops at the user's last request,
	// so time spent reading the final page is not counted, and a single
	// request produces a zero-length session. It measures time between
	// first and last activity, which is the usual web-analytics
	// definition — not true time-on-page.
	AvgSessionMinutes float64 `json:"avgSessionMinutes"`
	// SignupTrend is one entry per day for the trailing 30 days, including
	// days with no signups, so the chart has no gaps.
	SignupTrend []DayCount `json:"signupTrend"`
	GeneratedAt time.Time   `json:"generatedAt"`
}

// DayCount is a single point on the signup trend chart.
type DayCount struct {
	Date  string `json:"date"` // YYYY-MM-DD, UTC
	Count int64  `json:"count"`
}

func (s *Service) Overview(ctx context.Context) (Overview, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	now := time.Now().UTC()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	sevenDaysAgo := now.Add(-7 * 24 * time.Hour)
	twentyFourHoursAgo := now.Add(-24 * time.Hour)

	total, err := s.users.CountDocuments(ctx, bson.M{})
	if err != nil {
		return Overview{}, err
	}

	newToday, err := s.users.CountDocuments(ctx, bson.M{"createdAt": bson.M{"$gte": startOfToday}})
	if err != nil {
		return Overview{}, err
	}

	newThisWeek, err := s.users.CountDocuments(ctx, bson.M{"createdAt": bson.M{"$gte": sevenDaysAgo}})
	if err != nil {
		return Overview{}, err
	}

	dau, err := s.users.CountDocuments(ctx, bson.M{"lastSeenAt": bson.M{"$gte": twentyFourHoursAgo}})
	if err != nil {
		return Overview{}, err
	}

	avgMinutes, err := s.avgSessionMinutes(ctx, twentyFourHoursAgo)
	if err != nil {
		return Overview{}, err
	}

	trend, err := s.signupTrend(ctx, startOfToday, 30)
	if err != nil {
		return Overview{}, err
	}

	return Overview{
		TotalUsers:        total,
		NewUsersToday:     newToday,
		NewUsersThisWeek:  newThisWeek,
		DailyActiveUsers:  dau,
		AvgSessionMinutes: avgMinutes,
		SignupTrend:       trend,
		GeneratedAt:       now,
	}, nil
}

// signupTrend returns daily signup counts for the `days` days ending on
// startOfToday, zero-filling days with no registrations so the chart keeps an
// even time axis.
func (s *Service) signupTrend(ctx context.Context, startOfToday time.Time, days int) ([]DayCount, error) {
	from := startOfToday.AddDate(0, 0, -(days - 1))

	cur, err := s.users.Aggregate(ctx, mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.M{"createdAt": bson.M{"$gte": from}}}},
		bson.D{{Key: "$group", Value: bson.M{
			"_id": bson.M{"$dateToString": bson.M{
				"format":   "%Y-%m-%d",
				"date":     "$createdAt",
				"timezone": "UTC",
			}},
			"n": bson.M{"$sum": 1},
		}}},
	})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	counts := make(map[string]int64)
	for cur.Next(ctx) {
		var row struct {
			Date string `bson:"_id"`
			N    int64  `bson:"n"`
		}
		if err := cur.Decode(&row); err != nil {
			return nil, err
		}
		counts[row.Date] = row.N
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}

	trend := make([]DayCount, 0, days)
	for i := 0; i < days; i++ {
		day := from.AddDate(0, 0, i).Format("2006-01-02")
		trend = append(trend, DayCount{Date: day, Count: counts[day]})
	}
	return trend, nil
}

// avgSessionMinutes averages (lastSeenAt - loginAt) in minutes over users
// active since `since`, excluding users with no recorded loginAt (accounts
// that have never logged in since this feature shipped).
func (s *Service) avgSessionMinutes(ctx context.Context, since time.Time) (float64, error) {
	pipeline := bson.A{
		bson.M{"$match": bson.M{
			"lastSeenAt": bson.M{"$gte": since},
			"loginAt":    bson.M{"$exists": true},
		}},
		bson.M{"$project": bson.M{
			"durationMinutes": bson.M{
				"$max": bson.A{
					0,
					bson.M{"$divide": bson.A{
						bson.M{"$subtract": bson.A{"$lastSeenAt", "$loginAt"}},
						60000, // ms -> minutes
					}},
				},
			},
		}},
		bson.M{"$group": bson.M{
			"_id":        nil,
			"avgMinutes": bson.M{"$avg": "$durationMinutes"},
		}},
	}

	cur, err := s.users.Aggregate(ctx, pipeline)
	if err != nil {
		return 0, err
	}
	defer cur.Close(ctx)

	var result struct {
		AvgMinutes float64 `bson:"avgMinutes"`
	}
	if cur.Next(ctx) {
		if err := cur.Decode(&result); err != nil {
			return 0, err
		}
	}
	return result.AvgMinutes, cur.Err()
}
