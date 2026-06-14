package auth

import (
	"context"
	"testing"
	"time"
)

func TestConfiguredAdminEmailPromotesExistingUserAtLoginAndProfile(t *testing.T) {
	repo := NewInMemoryUserRepository()
	userService, err := NewService(repo, "test-secret", time.Hour, []string{})
	if err != nil {
		t.Fatalf("user service: %v", err)
	}

	registered, err := userService.Register(context.Background(), registerRequest{
		Name:     "Rachit Narula",
		Email:    "rachitnarulawork@gmail.com",
		Password: "Password123",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if registered.Role != "user" {
		t.Fatalf("stored role = %q, want user before admin config", registered.Role)
	}

	adminService, err := NewService(repo, "test-secret", time.Hour, []string{"rachitnarulawork@gmail.com"})
	if err != nil {
		t.Fatalf("admin service: %v", err)
	}

	user, token, err := adminService.Login(context.Background(), loginRequest{
		Email:    "rachitnarulawork@gmail.com",
		Password: "Password123",
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if user.Role != "admin" {
		t.Fatalf("login role = %q, want admin", user.Role)
	}

	_, role, err := adminService.ParseToken(token)
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if role != "admin" {
		t.Fatalf("token role = %q, want admin", role)
	}

	profile, err := adminService.Me(context.Background(), registered.ID)
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	if profile.Role != "admin" {
		t.Fatalf("profile role = %q, want admin", profile.Role)
	}
}
