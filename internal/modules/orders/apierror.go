package orders

import "strconv"

// APIError carries an explicit HTTP status and human-readable message so the
// handler can return the consistent { success:false, message } error contract
// with dynamic content (e.g. strike, lot counts) for the exit/sell flow.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Message }

func newAPIError(status int, message string) *APIError {
	return &APIError{Status: status, Message: message}
}

func newCodedAPIError(status int, code, message string) *APIError {
	return &APIError{Status: status, Code: code, Message: message}
}

// formatStrike renders a strike without trailing zeros (130 -> "130").
func formatStrike(strike float64) string {
	return strconv.FormatFloat(strike, 'f', -1, 64)
}
