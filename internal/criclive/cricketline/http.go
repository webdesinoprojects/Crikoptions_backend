package cricketline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/criclive/client"
)

// Endpoint labels. They double as quota-accounting keys, so they name the
// route rather than a formatted URL.
const (
	EndpointLive      = "/api/live"
	EndpointSchedule  = "/api/schedule/range"
	EndpointMatchLive = "/api/match/live"
)

const maxResponseBytes = 16 << 20

// httpClient is the transport for CricketLineApi. The key is sent in the
// X-API-Key header, never the query string, so it cannot leak into access
// logs or error messages that echo a URL.
type httpClient struct {
	baseURL *url.URL
	key     string
	http    *http.Client
}

func newHTTPClient(baseURL, key string, base *http.Client) (*httpClient, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("CricketLine API key is required")
	}
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = client.DefaultCricketLineBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("CricketLine base URL must be an absolute http or https URL")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if base == nil {
		base = &http.Client{Timeout: 15 * time.Second}
	}
	// Redirects are refused so the key header is never replayed to another host.
	protected := *base
	protected.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("CricketLine redirects are disabled")
	}
	return &httpClient{baseURL: parsed, key: key, http: &protected}, nil
}

// get fetches path and decodes a success body into destination. endpoint is
// the stable label used in errors.
func (c *httpClient) get(ctx context.Context, endpoint, path string, query url.Values, destination any) (json.RawMessage, client.RateLimit, error) {
	u := *c.baseURL
	u.Path = c.baseURL.Path + "/" + strings.TrimLeft(path, "/")
	u.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, client.RateLimit{}, &client.RequestError{Endpoint: endpoint, Message: c.redact(err.Error())}
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-API-Key", c.key)
	request.Header.Set("User-Agent", "Crikoptions-CricketLine/1.0")

	response, err := c.http.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, client.RateLimit{}, ctxErr
		}
		return nil, client.RateLimit{}, &client.RequestError{Endpoint: endpoint, Message: c.redact(err.Error())}
	}
	defer response.Body.Close()

	rateLimit := client.ParseRateLimit(response.Header, time.Now())
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, rateLimit, &client.RequestError{Endpoint: endpoint, Message: "could not read response body"}
	}
	if len(body) > maxResponseBytes {
		return nil, rateLimit, &client.RequestError{Endpoint: endpoint, Message: "response exceeded 16 MiB limit"}
	}

	var envelope errorEnvelope
	_ = json.Unmarshal(body, &envelope)
	message, code := envelope.text()

	if response.StatusCode == http.StatusTooManyRequests {
		return nil, rateLimit, &client.RateLimitError{
			Endpoint: endpoint, Message: c.redact(message),
			RetryAfter: rateLimit.RetryAfter, RateLimit: rateLimit,
		}
	}
	// A 200 carrying the error envelope is still an error.
	failed := response.StatusCode < 200 || response.StatusCode >= 300 || strings.EqualFold(envelope.Status, "error")
	if failed {
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		return nil, rateLimit, &client.HTTPError{
			StatusCode: response.StatusCode, Endpoint: endpoint,
			Code: c.redact(code), Message: c.redact(message), RateLimit: rateLimit,
		}
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return nil, rateLimit, &client.RequestError{Endpoint: endpoint, Message: "invalid JSON response"}
	}
	return json.RawMessage(body), rateLimit, nil
}

func (c *httpClient) redact(value string) string {
	if c.key == "" || value == "" {
		return value
	}
	value = strings.ReplaceAll(value, c.key, "[REDACTED]")
	return strings.ReplaceAll(value, url.QueryEscape(c.key), "[REDACTED]")
}

func matchPath(key string) string {
	return fmt.Sprintf("/api/match/%s/live", url.PathEscape(key))
}
