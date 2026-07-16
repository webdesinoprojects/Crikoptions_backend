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

type Client struct {
	baseURL    *url.URL
	token      string
	httpClient *http.Client
}

type PageOptions struct {
	Page     int
	Includes []string
	Sort     string
}

type LiveScoresOptions struct {
	Page     int
	Status   string
	Includes []string
}

type FixtureOptions struct {
	Includes []string
}

type FixturesOptions struct {
	From     time.Time
	To       time.Time
	LeagueID int64
	Status   string
	Page     int
	Includes []string
	Sort     string
}

// RequestError reports a transport or decoding failure without retaining the
// original request URL, which contains the API token.
type RequestError struct {
	Endpoint string
	Message  string
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("sportmonks request %s failed: %s", e.Endpoint, e.Message)
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
		return fmt.Sprintf("sportmonks request %s returned HTTP %d (%s): %s", e.Endpoint, e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("sportmonks request %s returned HTTP %d: %s", e.Endpoint, e.StatusCode, e.Message)
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
		return fmt.Sprintf("sportmonks request %s was rate limited; retry after %s", e.Endpoint, e.RetryAfter)
	}
	return fmt.Sprintf("sportmonks request %s was rate limited", e.Endpoint)
}

func New(cfg Config, httpClient *http.Client) (*Client, error) {
	token := strings.TrimSpace(cfg.APIToken)
	if token == "" {
		return nil, errors.New("Sportmonks API token is required")
	}
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("Sportmonks base URL must be an absolute http or https URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Sportmonks base URL must not contain credentials, query parameters, or a fragment")
	}
	if parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname()) {
		return nil, errors.New("Sportmonks base URL must use https outside loopback development")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")

	if httpClient == nil {
		timeout := cfg.HTTPTimeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		httpClient = &http.Client{Timeout: timeout}
	}
	// Redirects are disabled because authentication lives in the query string;
	// following a provider redirect could otherwise disclose the token to a
	// different host. Copying avoids mutating a caller-owned client.
	protectedHTTPClient := *httpClient
	protectedHTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("Sportmonks redirects are disabled")
	}
	return &Client{baseURL: parsed, token: token, httpClient: &protectedHTTPClient}, nil
}

func (c *Client) Leagues(ctx context.Context, options PageOptions) (Envelope[[]League], error) {
	query, err := pageQuery(options)
	if err != nil {
		return Envelope[[]League]{}, err
	}
	var envelope Envelope[[]League]
	rateLimit, err := c.get(ctx, "/leagues", query, &envelope)
	envelope.RateLimit = rateLimit
	return envelope, err
}

func (c *Client) Scores(ctx context.Context) (Envelope[[]Score], error) {
	var envelope Envelope[[]Score]
	rateLimit, err := c.get(ctx, "/scores", nil, &envelope)
	envelope.RateLimit = rateLimit
	return envelope, err
}

func (c *Client) LiveScores(ctx context.Context, options LiveScoresOptions) (Envelope[[]Fixture], error) {
	if options.Page < 0 {
		return Envelope[[]Fixture]{}, errors.New("Sportmonks page must not be negative")
	}
	query := make(url.Values)
	setPage(query, options.Page)
	setIncludes(query, options.Includes)
	if status := strings.TrimSpace(options.Status); status != "" {
		query.Set("filter[status]", status)
	}
	var envelope Envelope[[]Fixture]
	rateLimit, err := c.get(ctx, "/livescores", query, &envelope)
	envelope.RateLimit = rateLimit
	return envelope, err
}

func (c *Client) FixtureByID(ctx context.Context, fixtureID int64, options FixtureOptions) (Envelope[Fixture], error) {
	if fixtureID <= 0 {
		return Envelope[Fixture]{}, errors.New("Sportmonks fixture ID must be positive")
	}
	query := make(url.Values)
	setIncludes(query, options.Includes)
	var envelope Envelope[Fixture]
	rateLimit, err := c.get(ctx, "/fixtures/"+strconv.FormatInt(fixtureID, 10), query, &envelope)
	envelope.RateLimit = rateLimit
	return envelope, err
}

func (c *Client) Fixtures(ctx context.Context, options FixturesOptions) (Envelope[[]Fixture], error) {
	if options.From.IsZero() || options.To.IsZero() {
		return Envelope[[]Fixture]{}, errors.New("Sportmonks fixture date window requires both From and To")
	}
	from := options.From.UTC()
	to := options.To.UTC()
	if from.After(to) {
		return Envelope[[]Fixture]{}, errors.New("Sportmonks fixture From date must not be after To date")
	}
	if options.Page < 0 || options.LeagueID < 0 {
		return Envelope[[]Fixture]{}, errors.New("Sportmonks fixture page and league ID must not be negative")
	}
	query := make(url.Values)
	query.Set("filter[starts_between]", from.Format("2006-01-02")+","+to.Format("2006-01-02"))
	if options.LeagueID > 0 {
		query.Set("filter[league_id]", strconv.FormatInt(options.LeagueID, 10))
	}
	if status := strings.TrimSpace(options.Status); status != "" {
		query.Set("filter[status]", status)
	}
	setPage(query, options.Page)
	setIncludes(query, options.Includes)
	if sort := strings.TrimSpace(options.Sort); sort != "" {
		query.Set("sort", sort)
	}

	var envelope Envelope[[]Fixture]
	rateLimit, err := c.get(ctx, "/fixtures", query, &envelope)
	envelope.RateLimit = rateLimit
	return envelope, err
}

func pageQuery(options PageOptions) (url.Values, error) {
	if options.Page < 0 {
		return nil, errors.New("Sportmonks page must not be negative")
	}
	query := make(url.Values)
	setPage(query, options.Page)
	setIncludes(query, options.Includes)
	if sort := strings.TrimSpace(options.Sort); sort != "" {
		query.Set("sort", sort)
	}
	return query, nil
}

func setPage(query url.Values, page int) {
	if page > 0 {
		query.Set("page", strconv.Itoa(page))
	}
}

func setIncludes(query url.Values, includes []string) {
	seen := make(map[string]struct{}, len(includes))
	cleaned := make([]string, 0, len(includes))
	for _, include := range includes {
		include = strings.TrimSpace(include)
		if include == "" {
			continue
		}
		if _, exists := seen[include]; exists {
			continue
		}
		seen[include] = struct{}{}
		cleaned = append(cleaned, include)
	}
	if len(cleaned) > 0 {
		query.Set("include", strings.Join(cleaned, ","))
	}
}

func (c *Client) get(ctx context.Context, endpoint string, query url.Values, destination any) (RateLimit, error) {
	u := *c.baseURL
	u.Path = strings.TrimRight(c.baseURL.Path, "/") + "/" + strings.TrimLeft(endpoint, "/")
	values := make(url.Values, len(query)+1)
	for key, entries := range query {
		values[key] = append([]string(nil), entries...)
	}
	values.Set("api_token", c.token)
	u.RawQuery = values.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return RateLimit{}, &RequestError{Endpoint: endpoint, Message: redact(err.Error(), c.token)}
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Crikoptions-Sportmonks/1.0")

	response, err := c.httpClient.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return RateLimit{}, ctxErr
		}
		return RateLimit{}, &RequestError{Endpoint: endpoint, Message: redact(err.Error(), c.token)}
	}
	defer response.Body.Close()

	rateLimit := parseRateLimit(response.Header, time.Now())
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return rateLimit, &RequestError{Endpoint: endpoint, Message: "could not read response body"}
	}
	if len(body) > maxResponseBytes {
		return rateLimit, &RequestError{Endpoint: endpoint, Message: "response exceeded 64 MiB limit"}
	}

	if response.StatusCode == http.StatusTooManyRequests {
		message, _ := providerError(body)
		return rateLimit, &RateLimitError{
			Endpoint: endpoint, Message: redact(message, c.token),
			RetryAfter: rateLimit.RetryAfter, RateLimit: rateLimit,
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, code := providerError(body)
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		return rateLimit, &HTTPError{
			StatusCode: response.StatusCode, Endpoint: endpoint, Code: redact(code, c.token),
			Message: redact(message, c.token), RateLimit: rateLimit,
		}
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return rateLimit, &RequestError{Endpoint: endpoint, Message: "invalid JSON response"}
	}
	return rateLimit, nil
}

func providerError(body []byte) (message string, code string) {
	var value struct {
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
	if json.Unmarshal(value.Error, &text) == nil && message == "" {
		message = text
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
