package mal

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type requestPacer struct {
	gate chan struct{}
	next time.Time
}
type httpRoute struct {
	base       *url.URL
	credential string
	blocked    time.Time
	failures   int
}
type requestSettings struct {
	interval, timeout time.Duration
	maxBytes          int64
	now               func() time.Time
	wait              func(context.Context, time.Duration) error
}
type httpResult struct {
	body   []byte
	status int
	header http.Header
}

func newPacer() *requestPacer { return &requestPacer{gate: make(chan struct{}, 1)} }
func parseEndpoint(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid public HTTP endpoint")
	}
	return u, nil
}
func endpointURL(base *url.URL, path string, query url.Values) *url.URL {
	u := *base
	u.Path = strings.TrimRight(base.Path, "/") + "/" + path
	u.RawPath = ""
	u.RawQuery = query.Encode()
	return &u
}
func sameOrigin(a, b *url.URL) bool { return a.Scheme == b.Scheme && a.Host == b.Host }
func publicHTTPClient(original *http.Client, timeout time.Duration) *http.Client {
	var client http.Client
	if original != nil {
		client = *original
	} else {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = 4
		transport.MaxIdleConnsPerHost = 1
		transport.MaxConnsPerHost = 1
		transport.MaxResponseHeaderBytes = 32 << 10
		transport.IdleConnTimeout = time.Minute
		transport.ResponseHeaderTimeout = timeout
		client.Transport = transport
	}
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

// Routes share pacing but not cooldowns: a blocked website must not suppress
// the official API. Credentials belong only to the explicitly configured route.
func getHTTP(ctx context.Context, client *http.Client, pacer *requestPacer, route *httpRoute, settings requestSettings, target *url.URL, optional bool) (httpResult, error) {
	if err := ctx.Err(); err != nil {
		return httpResult{}, err
	}
	if !sameOrigin(target, route.base) || target.User != nil || !strings.HasPrefix(target.Path, strings.TrimRight(route.base.Path, "/")+"/") {
		return httpResult{}, ErrMalformed
	}
	select {
	case pacer.gate <- struct{}{}:
	case <-ctx.Done():
		return httpResult{}, ctx.Err()
	}
	defer func() { <-pacer.gate }()
	now := settings.now()
	if optional && route.blocked.After(now) {
		cause := ErrRateLimited
		if route.failures > 0 {
			cause = ErrUnavailable
		}
		return httpResult{}, &HTTPError{Cause: cause, RetryAfter: route.blocked.Sub(now)}
	}
	due := pacer.next
	if route.blocked.After(due) {
		due = route.blocked
	}
	if delay := due.Sub(now); delay > 0 {
		if err := settings.wait(ctx, delay); err != nil {
			return httpResult{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return httpResult{}, err
	}
	if err := takeRequest(ctx); err != nil {
		return httpResult{}, err
	}
	requestContext, cancel := context.WithTimeout(ctx, settings.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, target.String(), nil)
	if err != nil {
		return httpResult{}, ErrMalformed
	}
	request.Header.Set("User-Agent", "Kotantei")
	request.Header.Set("Accept", "application/json,text/html;q=0.9")
	if route.credential != "" {
		request.Header.Set("X-MAL-CLIENT-ID", route.credential)
	}
	pacer.next = settings.now().Add(settings.interval)
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return httpResult{}, ctx.Err()
		}
		route.backoff(settings.now())
		if requestContext.Err() != nil {
			return httpResult{}, requestContext.Err()
		}
		return httpResult{}, ErrUnavailable
	}
	defer response.Body.Close()
	now = settings.now()
	delay := retryAfter(response.Header.Get("Retry-After"), now)
	if response.StatusCode == 429 && delay <= 0 {
		delay = time.Minute
	}
	if until := now.Add(delay); delay > 0 && until.After(route.blocked) {
		route.blocked = until
	}
	cause := error(nil)
	switch {
	case response.StatusCode == 401:
		cause = ErrUnauthorized
	case response.StatusCode == 403:
		cause = ErrNotPublic
	case response.StatusCode == 404:
		cause = ErrNotFound
	case response.StatusCode == 429:
		cause = ErrRateLimited
	case response.StatusCode >= 500:
		route.backoff(now)
		cause = ErrUnavailable
	case response.StatusCode < 200 || response.StatusCode >= 400:
		cause = ErrMalformed
	}
	if cause != nil {
		return httpResult{}, &HTTPError{Cause: cause, Status: response.StatusCode, RetryAfter: max(0, route.blocked.Sub(now))}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, settings.maxBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return httpResult{}, ctx.Err()
		}
		route.backoff(settings.now())
		if requestContext.Err() != nil {
			return httpResult{}, requestContext.Err()
		}
		return httpResult{}, ErrUnavailable
	}
	if int64(len(body)) > settings.maxBytes {
		return httpResult{}, ErrResponseBudget
	}
	if !utf8.Valid(body) {
		return httpResult{}, ErrMalformed
	}
	route.failures = 0
	return httpResult{body: body, status: response.StatusCode, header: response.Header}, nil
}

func (route *httpRoute) backoff(now time.Time) {
	if route.failures < 6 {
		route.failures++
	}
	delay := 5 * time.Second
	for n := 1; n < route.failures; n++ {
		delay *= 2
	}
	if delay > 90*time.Second {
		delay = 90 * time.Second
	}
	if at := now.Add(delay); at.After(route.blocked) {
		route.blocked = at
	}
}
func retryAfter(raw string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 && seconds <= int64((1<<63-1)/time.Second) {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(raw); err == nil && at.After(now) {
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
