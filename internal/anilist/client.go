// Package anilist implements bounded, public AniList GraphQL tracking.
package anilist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"
)

var (
	ErrRateLimited    = errors.New("AniList request budget exhausted; retry after the cooldown")
	ErrUnavailable    = errors.New("AniList is unavailable")
	ErrNotFound       = errors.New("AniList account or item was not found")
	ErrNotPublic      = errors.New("AniList data is not publicly accessible")
	ErrGraphQL        = errors.New("AniList returned GraphQL errors; partial data was discarded")
	ErrMalformed      = errors.New("AniList returned incomplete or malformed data")
	ErrResponseBudget = errors.New("AniList response exceeded the size budget")
)

type APIError struct {
	Cause      error
	Status     int
	RetryAfter time.Duration
}

func (e *APIError) Error() string { return e.Cause.Error() }
func (e *APIError) Unwrap() error { return e.Cause }

type ClientOptions struct {
	Endpoint          string
	HTTPClient        *http.Client
	RequestsPerMinute int
	RequestTimeout    time.Duration
	MaxResponseBytes  int64
	Now               func() time.Time
	Wait              func(context.Context, time.Duration) error
}
type Client struct {
	options  ClientOptions
	gate     chan struct{}
	spacing  time.Duration
	next     time.Time
	blocked  time.Time
	failures int
}

// Close releases idle connections after all tracking/search work has joined.
func (c *Client) Close() { c.options.HTTPClient.CloseIdleConnections() }

func NewClient(options ClientOptions) (*Client, error) {
	if options.Endpoint == "" {
		options.Endpoint = "https://graphql.anilist.co"
	}
	if options.RequestsPerMinute == 0 {
		options.RequestsPerMinute = 29
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = 15 * time.Second
	}
	if options.MaxResponseBytes == 0 {
		options.MaxResponseBytes = 1 << 20
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Wait == nil {
		options.Wait = waitFor
	}
	endpoint, err := url.Parse(options.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.Fragment != "" || options.RequestsPerMinute < 1 || options.RequestsPerMinute > 29 || options.RequestTimeout <= 0 || options.MaxResponseBytes < 1 || options.MaxResponseBytes > 16<<20 {
		return nil, errors.New("invalid AniList client endpoint or limits")
	}
	var httpClient http.Client
	if options.HTTPClient != nil {
		httpClient = *options.HTTPClient
	} else {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = 2
		transport.MaxIdleConnsPerHost = 1
		transport.MaxConnsPerHost = 1
		transport.IdleConnTimeout = time.Minute
		transport.ResponseHeaderTimeout = options.RequestTimeout
		httpClient.Transport = transport
	}
	// A public GraphQL request should not follow an unexpected endpoint redirect.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	options.HTTPClient = &httpClient
	return &Client{options: options, gate: make(chan struct{}, 1), spacing: time.Minute / time.Duration(options.RequestsPerMinute)}, nil
}

// query serializes a client's requests and retains one bounded response at a
// time. All operations sharing this client share pacing, cooldown and backoff.
// It never retries a failed query or exposes an upstream error body.
func (c *Client) query(ctx context.Context, operation, query string, variables map[string]any, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(struct {
		OperationName string         `json:"operationName"`
		Query         string         `json:"query"`
		Variables     map[string]any `json:"variables"`
	}{operation, query, variables})
	if err != nil {
		return ErrMalformed
	}
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.gate }()
	due := c.next
	if c.blocked.After(due) {
		due = c.blocked
	}
	if delay := due.Sub(c.options.Now()); delay > 0 {
		if err := c.options.Wait(ctx, delay); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	requestContext, cancel := context.WithTimeout(ctx, c.options.RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, c.options.Endpoint, bytes.NewReader(encoded))
	if err != nil {
		return ErrMalformed
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Kotantei")
	// An attempted request consumes pacing even if no response headers arrive.
	c.next = c.options.Now().Add(c.spacing)
	response, err := c.options.HTTPClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.serverBackoff()
		if requestContext.Err() != nil {
			return requestContext.Err()
		}
		return ErrUnavailable
	}
	defer response.Body.Close()
	c.consumeHeaders(response.Header, response.StatusCode)
	if response.StatusCode == 429 {
		return c.rateError(response.StatusCode)
	}
	if response.StatusCode >= 500 {
		c.serverBackoff()
		return &APIError{Cause: ErrUnavailable, Status: response.StatusCode}
	}
	if response.StatusCode == 401 || response.StatusCode == 403 {
		return &APIError{Cause: ErrNotPublic, Status: response.StatusCode}
	}
	if response.StatusCode == 404 {
		return &APIError{Cause: ErrNotFound, Status: 404}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &APIError{Cause: ErrGraphQL, Status: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, c.options.MaxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.serverBackoff()
		if requestContext.Err() != nil {
			return requestContext.Err()
		}
		return ErrUnavailable
	}
	if int64(len(body)) > c.options.MaxResponseBytes {
		return ErrResponseBudget
	}
	if !utf8.Valid(body) {
		return ErrMalformed
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Status int `json:"status"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return ErrMalformed
	}
	if len(envelope.Errors) > 0 {
		cause := ErrGraphQL
		status := response.StatusCode
		for _, problem := range envelope.Errors {
			switch problem.Status {
			case 429:
				c.consumeHeaders(response.Header, 429)
				return c.rateError(429)
			case 401, 403:
				cause = ErrNotPublic
				status = problem.Status
			case 404:
				if cause == ErrGraphQL {
					cause = ErrNotFound
					status = 404
				}
			case 500, 502, 503, 504:
				c.serverBackoff()
				cause = ErrUnavailable
				status = problem.Status
			}
		}
		return &APIError{Cause: cause, Status: status}
	}
	if len(envelope.Data) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) || json.Unmarshal(envelope.Data, result) != nil {
		return ErrMalformed
	}
	c.failures = 0
	return nil
}

func (c *Client) consumeHeaders(header http.Header, status int) {
	now := c.options.Now()
	if limit, err := strconv.Atoi(header.Get("X-RateLimit-Limit")); err == nil && limit > 0 {
		if limit > c.options.RequestsPerMinute {
			limit = c.options.RequestsPerMinute
		}
		c.spacing = time.Minute / time.Duration(limit)
	}
	c.next = now.Add(c.spacing)
	remaining, remainingErr := strconv.Atoi(header.Get("X-RateLimit-Remaining"))
	pause := retryAfter(header.Get("Retry-After"), now)
	if status == 429 || (remainingErr == nil && remaining == 0) {
		if reset, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil && reset > 0 {
			if delay := time.Unix(reset, 0).Sub(now); delay > pause {
				pause = delay
			}
		}
		if pause <= 0 {
			pause = time.Minute
		}
	}
	if until := now.Add(pause); pause > 0 && until.After(c.blocked) {
		c.blocked = until
	}
}

func (c *Client) rateError(status int) error {
	now := c.options.Now()
	if !c.blocked.After(now) {
		c.blocked = now.Add(time.Minute)
	}
	return &APIError{Cause: ErrRateLimited, Status: status, RetryAfter: c.blocked.Sub(now)}
}

func (c *Client) serverBackoff() {
	if c.failures < 6 {
		c.failures++
	}
	delay := 5 * time.Second
	for n := 1; n < c.failures; n++ {
		delay *= 2
	}
	if delay > 90*time.Second {
		delay = 90 * time.Second
	}
	if due := c.options.Now().Add(delay); due.After(c.blocked) {
		c.blocked = due
	}
}

func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 && seconds <= int64((1<<63-1)/time.Second) {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

func waitFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
