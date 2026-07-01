package auth

import (
	"regexp"
	"strings"
)

var emailRegexp = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

// normalizePhone strips common formatting characters (+, spaces, dashes, parentheses)
// so users can type +91 98765 43210 and it becomes 919876543210 for validation.
func normalizePhone(phone string) string {
	var b strings.Builder
	for _, ch := range phone {
		if ch >= '0' && ch <= '9' {
			b.WriteRune(ch)
		}
	}
	return b.String()
}

func validateRegister(req registerRequest) (registerRequest, error) {
	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.Phone = normalizePhone(strings.TrimSpace(req.Phone))

	if len(req.Name) < 2 || len(req.Name) > 80 {
		return registerRequest{}, errInvalidName
	}
	if req.Email == "" || !emailRegexp.MatchString(req.Email) {
		return registerRequest{}, errInvalidEmail
	}
	if req.Phone != "" && !isValidPhone(req.Phone) {
		return registerRequest{}, errInvalidPhone
	}
	if len(req.Password) < 8 {
		return registerRequest{}, errPasswordTooShort
	}
	if len(req.Password) > 128 {
		return registerRequest{}, errPasswordTooLong
	}

	return req, nil
}

func validateLogin(req loginRequest) (loginRequest, error) {
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Email == "" || req.Password == "" {
		return loginRequest{}, errInvalidCreds
	}
	return req, nil
}

func isValidPhone(phone string) bool {
	if len(phone) < 10 || len(phone) > 15 {
		return false
	}
	for _, ch := range phone {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

