package auth

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// seedUser inserts a bare user record and returns its id.
func seedUser(t *testing.T, repo *InMemoryUserRepository, email string) primitive.ObjectID {
	t.Helper()
	user, err := repo.Create(context.Background(), userRecord{
		User: User{ID: primitive.NewObjectID(), Email: email},
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return user.ID
}

// A continuing session keeps its original start time, so the measured length
// grows with activity rather than restarting on every request.
func TestTouchLastSeenKeepsSessionStartWhileActive(t *testing.T) {
	repo := NewInMemoryUserRepository()
	id := seedUser(t, repo, "active@example.com")

	start := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	if err := repo.TouchLogin(context.Background(), id, start); err != nil {
		t.Fatalf("TouchLogin: %v", err)
	}

	later := start.Add(10 * time.Minute)
	if err := repo.TouchLastSeen(context.Background(), id, later, later.Add(-SessionIdleGap)); err != nil {
		t.Fatalf("TouchLastSeen: %v", err)
	}

	user, _, err := repo.FindByID(context.Background(), id)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if !user.LoginAt.Equal(start) {
		t.Fatalf("LoginAt = %v, want unchanged %v", user.LoginAt, start)
	}
	if got := user.LastSeenAt.Sub(user.LoginAt); got != 10*time.Minute {
		t.Fatalf("session length = %v, want 10m", got)
	}
}

// Returning after a long idle gap must start a new session. This is the case
// that would otherwise report a multi-hour "session" just because the JWT was
// still valid.
func TestTouchLastSeenStartsNewSessionAfterIdleGap(t *testing.T) {
	repo := NewInMemoryUserRepository()
	id := seedUser(t, repo, "returning@example.com")

	start := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	if err := repo.TouchLogin(context.Background(), id, start); err != nil {
		t.Fatalf("TouchLogin: %v", err)
	}

	returned := start.Add(8 * time.Hour)
	if err := repo.TouchLastSeen(context.Background(), id, returned, returned.Add(-SessionIdleGap)); err != nil {
		t.Fatalf("TouchLastSeen: %v", err)
	}

	user, _, err := repo.FindByID(context.Background(), id)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if !user.LoginAt.Equal(returned) {
		t.Fatalf("LoginAt = %v, want reset to %v", user.LoginAt, returned)
	}
	if got := user.LastSeenAt.Sub(user.LoginAt); got != 0 {
		t.Fatalf("session length = %v, want 0 for a fresh session", got)
	}
}

// Accounts created before loginAt/lastSeenAt existed must pick up a session
// start on their first authenticated request instead of staying excluded.
func TestTouchLastSeenBackfillsLegacyAccount(t *testing.T) {
	repo := NewInMemoryUserRepository()
	id := seedUser(t, repo, "legacy@example.com")

	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	if err := repo.TouchLastSeen(context.Background(), id, now, now.Add(-SessionIdleGap)); err != nil {
		t.Fatalf("TouchLastSeen: %v", err)
	}

	user, _, err := repo.FindByID(context.Background(), id)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if !user.LoginAt.Equal(now) {
		t.Fatalf("LoginAt = %v, want %v", user.LoginAt, now)
	}
}

// The throttle must suppress repeat writes inside the window and allow them
// again once it passes, so request volume does not become write volume.
func TestShouldTouchThrottlesWithinWindow(t *testing.T) {
	svc, err := NewService(NewInMemoryUserRepository(), "test-secret-value", time.Hour, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	id := primitive.NewObjectID()
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)

	if !svc.shouldTouch(id, now) {
		t.Fatal("first touch should be allowed")
	}
	if svc.shouldTouch(id, now.Add(lastSeenThrottle-time.Second)) {
		t.Fatal("touch inside the throttle window should be suppressed")
	}
	if !svc.shouldTouch(id, now.Add(lastSeenThrottle)) {
		t.Fatal("touch after the throttle window should be allowed")
	}
}

// The throttle map must not grow without bound as users come and go.
func TestShouldTouchSweepsStaleEntries(t *testing.T) {
	svc, err := NewService(NewInMemoryUserRepository(), "test-secret-value", time.Hour, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	for i := 0; i < lastTouchSweepSize; i++ {
		svc.shouldTouch(primitive.NewObjectID(), now)
	}
	if got := len(svc.lastTouch); got != lastTouchSweepSize {
		t.Fatalf("map size before sweep = %d, want %d", got, lastTouchSweepSize)
	}

	// One more touch past the throttle window triggers the sweep, which
	// clears every entry that can no longer suppress a write.
	svc.shouldTouch(primitive.NewObjectID(), now.Add(2*lastSeenThrottle))
	if got := len(svc.lastTouch); got != 1 {
		t.Fatalf("map size after sweep = %d, want 1", got)
	}
}
