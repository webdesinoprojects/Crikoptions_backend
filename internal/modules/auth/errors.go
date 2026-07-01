package auth

import (
	"errors"
	"net/http"
)

var (
	errEmailExists       = errors.New("email already registered")
	errPhoneExists       = errors.New("phone already registered")
	errInvalidCreds      = errors.New("invalid credentials")
	errUserNotFound      = errors.New("user not found")
	errUnauthorized      = errors.New("unauthorized")
	errInvalidToken      = errors.New("invalid token")
	errTokenExpired      = errors.New("token expired")
	errInvalidPayload    = errors.New("invalid payload")
	errNothingToUpdate   = errors.New("nothing to update")
	errInvalidName       = errors.New("name must be between 2 and 80 characters")
	errInvalidEmail      = errors.New("please enter a valid email address")
	errInvalidPhone      = errors.New("phone must be 10-15 digits (numbers only, no spaces or dashes)")
	errPasswordTooShort  = errors.New("password must be at least 8 characters")
	errPasswordTooLong   = errors.New("password must not exceed 128 characters")
)

func mapAuthError(err error) (int, string) {
	switch {
	case errors.Is(err, errInvalidName):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, errInvalidEmail):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, errInvalidPhone):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, errPasswordTooShort):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, errPasswordTooLong):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, errInvalidPayload):
		return http.StatusBadRequest, "Invalid input"
	case errors.Is(err, errEmailExists), errors.Is(err, errPhoneExists):
		return http.StatusConflict, err.Error()
	case errors.Is(err, errInvalidCreds):
		return http.StatusUnauthorized, "Invalid email or password"
	case errors.Is(err, errUserNotFound):
		return http.StatusNotFound, "User not found"
	case errors.Is(err, errNothingToUpdate):
		return http.StatusBadRequest, "Nothing to update"
	case errors.Is(err, errTokenExpired), errors.Is(err, errInvalidToken), errors.Is(err, errUnauthorized):
		return http.StatusUnauthorized, "Unauthorized"
	default:
		return http.StatusInternalServerError, "Something went wrong"
	}
}
