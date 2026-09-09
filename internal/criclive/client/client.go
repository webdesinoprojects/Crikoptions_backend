package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 64 << 20

// Endpoint paths. Kept in one place so quota accounting and logging can refer
// to a stable label instead of a formatted URL.
const (
	EndpointLive       = "/cricket/live"
	EndpointSchedule   = "/cricket/schedule"
	EndpointScorecard  = "/cricket/scorecard"
	EndpointCommentary = "/cricket/commentary"
	EndpointOvers      = "/cricket/overs"
	EndpointSquads     = "/cricket/squads"
)

type Client struct {
	baseURL    *url.URL
	token      string
	httpClient *http.Client
}

// RequestError reports a transport or decoding failure without retaining the
// original request, which carries the API token.
type RequestError struct {
	Endpoint string
	Message  string
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("criclive request %s failed: %s", e.Endpoint, e.Message)
}

type HTTPError struct {
	StatusCode int
	Endpoint   string
	Code       string
	Message    string
	RateLimit  RateLimit
}

func (e *HTTPError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("criclive request %s returned HTTP %d (%s): %s", e.Endpoint, e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("criclive request %s returned HTTP %d: %s", e.Endpoint, e.StatusCode, e.Message)
}

// RateLimitError is returned for HTTP 429 and carries enough metadata for a
// scheduler to defer work without parsing an error string.
type RateLimitError struct {
	Endpoint   string
	Message    string
	RetryAfter time.Duration
	RateLimit  RateLimit
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("criclive request %s was rate limited; retry after %s", e.Endpoint, e.RetryAfter)
	}
	return fmt.Sprintf("criclive request %s was rate limited", e.Endpoint)
}

func New(cfg Config, httpClient *http.Client) (*Client, error) {
	token := strings.TrimSpace(cfg.APIToken)
	if token == "" {
		return nil, errors.New("CricLive API token is required")
	}
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("CricLive base URL must be an absolute http or https URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("CricLive base URL must not contain credentials, query parameters, or a fragment")
	}
	if parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname()) {
		return nil, errors.New("CricLive base URL must use https outside loopback development")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")

	if httpClient == nil {
		timeout := cfg.HTTPTimeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		httpClient = &http.Client{Timeout: timeout}
	}
	// The bearer token is attached per request; redirects are refused so a
	// provider redirect can never replay the Authorization header to another
	// host. Copying avoids mutating a caller-owned client.
	protectedHTTPClient := *httpClient
	protectedHTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("CricLive redirects are disabled")
	}
	return &Client{baseURL: parsed, token: token, httpClient: &protectedHTTPClient}, nil
}

// LiveScores returns every match CricLive currently considers in-window, which
// includes Preview, In Progress, Stumps, and recently Complete states.
func (c *Client) LiveScores(ctx context.Context) (LiveResponse, RateLimit, error) {
	var response LiveResponse
	raw, rateLimit, err := c.get(ctx, EndpointLive, &response)
	response.Raw = raw
	return response, rateLimit, err
}

func (c *Client) Schedule(ctx context.Context) (ScheduleResponse, RateLimit, error) {
	var response ScheduleResponse
	_, rateLimit, err := c.get(ctx, EndpointSchedule, &response)
	return response, rateLimit, err
}

func (c *Client) Commentary(ctx context.Context, matchID int64) (CommentaryResponse, RateLimit, error) {
	if matchID <= 0 {
		return CommentaryResponse{}, RateLimit{}, errors.New("CricLive match ID must be positive")
	}
	var response CommentaryResponse
	raw, rateLimit, err := c.get(ctx, EndpointCommentary+"/"+strconv.FormatInt(matchID, 10), &response)
	response.Raw = raw
	return response, rateLimit, err
}

func (c *Client) Scorecard(ctx context.Context, matchID int64) (ScorecardResponse, RateLimit, error) {
	if matchID <= 0 {
		return ScorecardResponse{}, RateLimit{}, errors.New("CricLive match ID must be positive")
	}
	var response ScorecardResponse
	raw, rateLimit, err := c.get(ctx, EndpointScorecard+"/"+strconv.FormatInt(matchID, 10), &response)
	response.Raw = raw
	return response, rateLimit, err
}

func (c *Client) Overs(ctx context.Context, matchID int64) (OversResponse, RateLimit, error) {
	if matchID <= 0 {
		return OversResponse{}, RateLimit{}, errors.New("CricLive match ID must be positive")
	}
	var response OversResponse
	raw, rateLimit, err := c.get(ctx, EndpointOvers+"/"+strconv.FormatInt(matchID, 10), &response)
	response.Raw = raw
	return response, rateLimit, err
}

func (c *Client) Squads(ctx context.Context, matchID int64) (SquadsResponse, RateLimit, error) {
	if matchID <= 0 {
		return SquadsResponse{}, RateLimit{}, errors.New("CricLive match ID must be positive")
	}
	var response SquadsResponse
	_, rateLimit, err := c.get(ctx, EndpointSquads+"/"+strconv.FormatInt(matchID, 10), &response)
	return response, rateLimit, err
}

func (c *Client) get(ctx context.Context, endpoint string, destination any) (json.RawMessage, RateLimit, error) {
	u := *c.baseURL
	u.Path = strings.TrimRight(c.baseURL.Path, "/") + "/" + strings.TrimLeft(endpoint, "/")

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, RateLimit{}, &RequestError{Endpoint: endpoint, Message: redact(err.Error(), c.token)}
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("User-Agent", "Crikoptions-CricLive/1.0")

	response, err := c.httpClient.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, RateLimit{}, ctxErr
		}
		return nil, RateLimit{}, &RequestError{Endpoint: endpoint, Message: redact(err.Error(), c.token)}
	}
	defer response.Body.Close()

	rateLimit := parseRateLimit(response.Header, time.Now())
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, rateLimit, &RequestError{Endpoint: endpoint, Message: "could not read response body"}
	}
	if len(body) > maxResponseBytes {
		return nil, rateLimit, &RequestError{Endpoint: endpoint, Message: "response exceeded 64 MiB limit"}
	}

	if response.StatusCode == http.StatusTooManyRequests {
		message, _ := providerError(body)
		return nil, rateLimit, &RateLimitError{
			Endpoint: endpoint, Message: redact(message, c.token),
			RetryAfter: rateLimit.RetryAfter, RateLimit: rateLimit,
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, code := providerError(body)
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		return nil, rateLimit, &HTTPError{
			StatusCode: response.StatusCode, Endpoint: endpoint, Code: redact(code, c.token),
			Message: redact(message, c.token), RateLimit: rateLimit,
		}
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return nil, rateLimit, &RequestError{Endpoint: endpoint, Message: "invalid JSON response"}
	}
	return json.RawMessage(body), rateLimit, nil
}

// providerError reads CricLive's {"success":false,"error":"..."} envelope as
// well as the {"message":...} shape returned by the gateway.
func providerError(body []byte) (message string, code string) {
	var value struct {
		Success bool            `json:"success"`
		Message string          `json:"message"`
		Code    string          `json:"code"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &value) != nil {
		return "", ""
	}
	message, code = value.Message, value.Code
	if len(value.Error) == 0 {
		return message, code
	}
	var text string
	if json.Unmarshal(value.Error, &text) == nil {
		if message == "" {
			message = text
		}
		return message, code
	}
	var nested struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(value.Error, &nested) == nil {
		if message == "" {
			message = nested.Message
		}
		if code == "" {
			code = nested.Code
		}
	}
	return message, code
}

func parseRateLimit(header http.Header, now time.Time) RateLimit {
	var result RateLimit
	result.Limit = firstIntHeader(header, "X-RateLimit-Limit", "RateLimit-Limit")
	result.Remaining = firstIntHeader(header, "X-RateLimit-Remaining", "RateLimit-Remaining")
	result.RetryAfter = parseDelayHeader(header.Get("Retry-After"), now)

	reset := firstHeader(header, "X-RateLimit-Reset", "RateLimit-Reset")
	if seconds, err := strconv.ParseInt(strings.TrimSpace(reset), 10, 64); err == nil {
		if seconds >= 1_000_000_000 {
			value := time.Unix(seconds, 0).UTC()
			result.ResetAt = &value
		} else if seconds > 0 {
			result.ResetAfter = time.Duration(seconds) * time.Second
		}
	} else if value, err := http.ParseTime(reset); err == nil {
		value = value.UTC()
		result.ResetAt = &value
	}
	return result
}

func firstIntHeader(header http.Header, names ...string) *int {
	value := firstHeader(header, names...)
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return nil
	}
	return &n
}

func firstHeader(header http.Header, names ...string) string {
	for _, name := range names {
		if value := header.Get(name); value != "" {
			return value
		}
	}
	return ""
}

func parseDelayHeader(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if deadline, err := http.ParseTime(value); err == nil {
		delay := deadline.Sub(now)
		if delay > 0 {
			return delay
		}
	}
	return 0
}

func redact(value, token string) string {
	if token == "" || value == "" {
		return value
	}
	value = strings.ReplaceAll(value, token, "[REDACTED]")
	value = strings.ReplaceAll(value, url.QueryEscape(token), "[REDACTED]")
	return value
}
