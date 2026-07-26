package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Service struct {
	repo        UserRepository
	jwt         *jwt
	adminEmails map[string]struct{}
	google      *googleVerifier
}

// EnableGoogleAuth turns on Google sign-in by configuring the OAuth client ID
// that ID tokens must be issued for. A blank clientID leaves Google auth
// disabled (the endpoint then reports it is not configured).
func (s *Service) EnableGoogleAuth(clientID string) {
	if strings.TrimSpace(clientID) == "" {
		return
	}
	s.google = newGoogleVerifier(clientID)
}

func NewService(repo UserRepository, jwtSecret string, tokenTTL time.Duration, adminEmails []string) (*Service, error) {
	if repo == nil {
		return nil, errors.New("repo is required")
	}
	j, err := newJWT(jwtSecret, tokenTTL)
	if err != nil {
		return nil, errors.New("JWT_SECRET is required")
	}
	adminSet := make(map[string]struct{}, len(adminEmails))
	for _, e := range adminEmails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" {
			adminSet[e] = struct{}{}
		}
	}
	return &Service{repo: repo, jwt: j, adminEmails: adminSet}, nil
}

// isAdminEmail reports whether email (case-insensitive) is configured as an
// admin. The default role for new users is "user"; configured admin emails
// are promoted to "admin" at registration/login time.
func (s *Service) isAdminEmail(email string) bool {
	_, ok := s.adminEmails[strings.ToLower(strings.TrimSpace(email))]
	return ok
}

func (s *Service) EnsureIndexes(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.repo.EnsureIndexes(ctx)
}

func (s *Service) Register(ctx context.Context, req registerRequest) (User, error) {
	req, err := validateRegister(req)
	if err != nil {
		return User{}, err
	}

	hash, err := hashPassword(req.Password)
	if err != nil {
		return User{}, err
	}

	role := "user"
	if s.isAdminEmail(req.Email) {
		role = "admin"
	}

	now := time.Now().UTC()
	rec := userRecord{
		User: User{
			ID:    primitive.NewObjectID(),
			Name:  req.Name,
			Email: req.Email,
			Phone: req.Phone,
			Tier:  "STANDARD",
			Role:  role,
			Settings: UserSettings{
				RiskLimits: RiskLimits{
					MaxExposure:     10000.0,
					DefaultLeverage: 1,
					AutoKillSwitch:  false,
				},
				Preferences: Preferences{
					Theme:                "TERMINAL_DARK",
					DataDensity:          "COMPACT",
					NotificationsEnabled: true,
				},
			},
			CreatedAt: now,
			UpdatedAt: now,
		},
		PasswordHash: hash,
	}

	return s.repo.Create(ctx, rec)
}

func (s *Service) Login(ctx context.Context, req loginRequest) (User, string, error) {
	req, err := validateLogin(req)
	if err != nil {
		return User{}, "", err
	}

	rec, ok, err := s.repo.FindByEmail(ctx, req.Email)
	if err != nil {
		return User{}, "", err
	}
	if !ok || !verifyPassword(req.Password, rec.PasswordHash) {
		return User{}, "", errInvalidCreds
	}

	user := s.withEffectiveRole(rec.User)
	token, err := s.jwt.Issue(user.ID.Hex(), user.Role)
	if err != nil {
		return User{}, "", errUnauthorized
	}

	return user, token, nil
}

// LoginWithGoogle verifies a Google ID token ("credential"), finds or creates
// the matching user by email, and issues a session JWT. The returned bool is
// true when a brand-new account was provisioned (so callers can grant the
// welcome bonus).
func (s *Service) LoginWithGoogle(ctx context.Context, credential string) (User, string, bool, error) {
	if s.google == nil {
		return User{}, "", false, errGoogleNotConfigured
	}

	claims, err := s.google.Verify(ctx, credential)
	if err != nil {
		return User{}, "", false, err
	}

	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if email == "" || !emailRegexp.MatchString(email) {
		return User{}, "", false, errGoogleInvalidToken
	}

	created := false
	rec, ok, err := s.repo.FindByEmail(ctx, email)
	if err != nil {
		return User{}, "", false, err
	}

	var user User
	if ok {
		user = rec.User
	} else {
		user, err = s.createGoogleUser(ctx, email, claims.Name)
		if err != nil {
			return User{}, "", false, err
		}
		created = true
	}

	user = s.withEffectiveRole(user)
	token, err := s.jwt.Issue(user.ID.Hex(), user.Role)
	if err != nil {
		return User{}, "", false, errUnauthorized
	}
	return user, token, created, nil
}

// createGoogleUser provisions a passwordless account for a Google sign-in.
func (s *Service) createGoogleUser(ctx context.Context, email, name string) (User, error) {
	name = strings.TrimSpace(name)
	if len(name) < 2 {
		name = strings.Split(email, "@")[0]
	}
	if len(name) > 80 {
		name = name[:80]
	}

	role := "user"
	if s.isAdminEmail(email) {
		role = "admin"
	}

	now := time.Now().UTC()
	rec := userRecord{
		User: User{
			ID:    primitive.NewObjectID(),
			Name:  name,
			Email: email,
			Tier:  "STANDARD",
			Role:  role,
			Settings: UserSettings{
				RiskLimits: RiskLimits{
					MaxExposure:     10000.0,
					DefaultLeverage: 1,
					AutoKillSwitch:  false,
				},
				Preferences: Preferences{
					Theme:                "TERMINAL_DARK",
					DataDensity:          "COMPACT",
					NotificationsEnabled: true,
				},
			},
			CreatedAt: now,
			UpdatedAt: now,
		},
		// No password hash — this account signs in via Google only.
	}
	return s.repo.Create(ctx, rec)
}

// ParseToken verifies a JWT and returns the authenticated user ID and role.
func (s *Service) ParseToken(token string) (primitive.ObjectID, string, error) {
	claims, err := s.jwt.Parse(token)
	if err != nil {
		return primitive.ObjectID{}, "", err
	}
	id, err := primitive.ObjectIDFromHex(claims.Sub)
	if err != nil {
		return primitive.ObjectID{}, "", errInvalidToken
	}
	return id, claims.Role, nil
}

func (s *Service) Me(ctx context.Context, userID primitive.ObjectID) (User, error) {
	u, ok, err := s.repo.FindByID(ctx, userID)
	if err != nil {
		return User{}, err
	}
	if !ok {
		return User{}, errUserNotFound
	}
	return s.withEffectiveRole(u), nil
}

func (s *Service) UpdateMe(ctx context.Context, userID primitive.ObjectID, req updateMeRequest) (User, error) {
	var name *string
	if req.Name != nil {
		v := *req.Name
		v = strings.TrimSpace(v)
		if len(v) < 2 || len(v) > 80 {
			return User{}, errInvalidPayload
		}
		name = &v
	}

	var phone *string
	if req.Phone != nil {
		v := strings.TrimSpace(*req.Phone)
		if v != "" && !isValidPhone(v) {
			return User{}, errInvalidPayload
		}
		phone = &v
	}

	if name == nil && phone == nil && req.Settings == nil {
		return User{}, errNothingToUpdate
	}

	user, err := s.repo.UpdateMe(ctx, userID, name, phone, req.Settings)
	if err != nil {
		return User{}, err
	}
	return s.withEffectiveRole(user), nil
}

func (s *Service) withEffectiveRole(user User) User {
	if s.isAdminEmail(user.Email) {
		user.Role = "admin"
	}
	return user
}
