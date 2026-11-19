package anilist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type requestJSON struct {
	OperationName string                     `json:"operationName"`
	Query         string                     `json:"query"`
	Variables     map[string]json.RawMessage `json:"variables"`
}
type testClock struct {
	mutex sync.Mutex
	now   time.Time
	waits []time.Duration
}

func (c *testClock) Now() time.Time { c.mutex.Lock(); defer c.mutex.Unlock(); return c.now }
func (c *testClock) Wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	return nil
}

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *testClock) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	clock := &testClock{now: time.Unix(100, 0).UTC()}
	client, err := NewClient(ClientOptions{Endpoint: server.URL, HTTPClient: server.Client(), Now: clock.Now, Wait: clock.Wait})
	if err != nil {
		t.Fatal(err)
	}
	return client, clock
}
func answerQuery(ctx context.Context, client *Client) error {
	var result struct {
		Answer int `json:"answer"`
	}
	return client.query(ctx, "Example", "query Example { answer }", map[string]any{}, &result)
}

func TestGraphQLClientSendsVariablesWithoutInterpolatingUserInput(t *testing.T) {
	name := `Reader" { unexpected }`
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "" {
			t.Error("request was not public JSON GraphQL")
		}
		var request requestJSON
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		var actual string
		if err := json.Unmarshal(request.Variables["name"], &actual); err != nil {
			t.Error(err)
		}
		if actual != name || request.OperationName != "Example" || strings.Contains(request.Query, name) {
			t.Error("username was interpolated instead of passed as a variable")
		}
		fmt.Fprint(w, `{"data":{"answer":42}}`)
	})
	var result struct {
		Answer int `json:"answer"`
	}
	if err := client.query(context.Background(), "Example", "query Example($name:String!){ answer }", map[string]any{"name": name}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Answer != 42 {
		t.Fatal("GraphQL data was not decoded")
	}
}

func TestProfileOmitsTheUnusedLookupVariable(t *testing.T) {
	for _, tc := range []struct {
		label, selector, name string
		id                    int64
		value                 any
	}{
		{"by ID", "id", "", 101, int64(101)},
		{"by name", "name", `Reader" { unexpected }`, 0, `Reader" { unexpected }`},
	} {
		t.Run(tc.label, func(t *testing.T) {
			client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				var request requestJSON
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				want, _ := json.Marshal(tc.value)
				// AniList treats an explicitly null, unused User selector as a
				// filter; an otherwise valid lookup then returns Not Found.
				if request.OperationName != "KotanteiProfile" || len(request.Variables) != 1 || string(request.Variables[tc.selector]) != string(want) {
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"data":null,"errors":[{"status":404}]}`)
					return
				}
				if tc.name != "" && strings.Contains(request.Query, tc.name) {
					t.Error("profile username was interpolated into the query")
				}
				fmt.Fprint(w, `{"data":{"User":{"id":101,"name":"Reader"}}}`)
			})
			user, err := client.profile(context.Background(), tc.id, tc.name)
			if err != nil || user.ID != 101 {
				t.Fatal("valid profile lookup sent an unused null selector", user.ID, err)
			}
		})
	}
}

func TestGraphQLClientRejectsPartialErrorsWithoutExposingResponseText(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		cause  error
	}{
		{200, `{"data":{"answer":42},"errors":[{"message":"DO-NOT-PRINT-SECRET"}]}`, ErrGraphQL},
		{403, `{"errors":[{"message":"DO-NOT-PRINT-SECRET"}]}`, ErrNotPublic},
		{404, `{"data":null}`, ErrNotFound},
		{200, `{"data":null,"errors":[{"message":"DO-NOT-PRINT-SECRET","status":404}]}`, ErrNotFound},
		{200, `{"data":null,"errors":[{"message":"DO-NOT-PRINT-SECRET","status":429}]}`, ErrRateLimited},
		{503, `DO-NOT-PRINT-SECRET`, ErrUnavailable},
	} {
		t.Run(fmt.Sprint(tc.status, tc.cause), func(t *testing.T) {
			client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) })
			var result struct{ Answer int }
			err := client.query(context.Background(), "Example", "query Example { answer }", nil, &result)
			if !errors.Is(err, tc.cause) || strings.Contains(err.Error(), "DO-NOT-PRINT") || result.Answer != 0 {
				t.Fatalf("partial/unsafe response accepted: answer=%d err=%v", result.Answer, err)
			}
		})
	}
}

func TestGraphQLClientBoundsResponseBodiesAndRejectsMalformedJSON(t *testing.T) {
	for _, body := range []string{`{"data":null}`, `{}`, `[]`, `{"data":{"answer":1}} garbage`, string([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'})} {
		client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		if err := answerQuery(context.Background(), client); !errors.Is(err, ErrMalformed) {
			t.Fatalf("invalid response accepted: %v", err)
		}
	}
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", 100)) })
	client.options.MaxResponseBytes = 16
	if err := answerQuery(context.Background(), client); !errors.Is(err, ErrResponseBudget) {
		t.Fatalf("response budget ignored: %v", err)
	}
}

func TestRateLimiterHonorsResetAndRetryAfterWithoutRetryingTheFailedRequest(t *testing.T) {
	var calls atomic.Int64
	client, clock := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "30")
			w.Header().Set("X-RateLimit-Reset", "140")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"errors":[{"status":429}]}`)
			return
		}
		fmt.Fprint(w, `{"data":{"answer":1}}`)
	})
	if err := answerQuery(context.Background(), client); !errors.Is(err, ErrRateLimited) {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("rate-limited request was retried automatically")
	}
	if err := answerQuery(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(clock.waits, []time.Duration{40 * time.Second}) {
		t.Fatalf("cooldown was shortened: %v", clock.waits)
	}
}

func TestRateLimiterConsumesSuccessfulResponseHeaders(t *testing.T) {
	for _, tc := range []struct {
		limit, remaining, reset string
		delay                   time.Duration
	}{
		{"90", "10", "", time.Minute / 29}, {"2", "1", "", 30 * time.Second}, {"30", "0", "160", time.Minute},
	} {
		var calls atomic.Int64
		client, clock := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("X-RateLimit-Limit", tc.limit)
				w.Header().Set("X-RateLimit-Remaining", tc.remaining)
				w.Header().Set("X-RateLimit-Reset", tc.reset)
			}
			fmt.Fprint(w, `{"data":{"answer":1}}`)
		})
		for range 2 {
			if err := answerQuery(context.Background(), client); err != nil {
				t.Fatal(err)
			}
		}
		if len(clock.waits) != 1 || clock.waits[0] != tc.delay {
			t.Fatalf("response headers ignored: waits=%v want=%s", clock.waits, tc.delay)
		}
	}
}

func TestGraphQLRateLimitErrorsHonorResetHeaders(t *testing.T) {
	var calls atomic.Int64
	client, clock := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Reset", "180")
			fmt.Fprint(w, `{"data":null,"errors":[{"status":429}]}`)
			return
		}
		fmt.Fprint(w, `{"data":{"answer":1}}`)
	})
	if err := answerQuery(context.Background(), client); !errors.Is(err, ErrRateLimited) {
		t.Fatal(err)
	}
	if err := answerQuery(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(clock.waits, []time.Duration{80 * time.Second}) {
		t.Fatalf("GraphQL cooldown ignored reset header: %v", clock.waits)
	}
}

func TestGraphQLClientBacksOffServerFailures(t *testing.T) {
	var calls atomic.Int64
	client, clock := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{"data":{"answer":1}}`)
	})
	for range 2 {
		if err := answerQuery(context.Background(), client); !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
	}
	if err := answerQuery(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(clock.waits, []time.Duration{5 * time.Second, 10 * time.Second}) {
		t.Fatalf("server failure backoff ignored: %v", clock.waits)
	}
}

func TestTransportFailuresStillConsumeTheConfiguredRequestBudget(t *testing.T) {
	var calls atomic.Int64
	first, clock := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close()
			return
		}
		fmt.Fprint(w, `{"data":{"answer":1}}`)
	})
	options := first.options
	options.RequestsPerMinute = 1
	client, err := NewClient(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := answerQuery(context.Background(), client); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("failed transport request was retried automatically")
	}
	if err := answerQuery(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(clock.waits, []time.Duration{time.Minute}) {
		t.Fatalf("transport failure bypassed the configured request budget: %v", clock.waits)
	}
}

func TestInterruptedResponseBodiesUseServerFailureBackoff(t *testing.T) {
	var calls atomic.Int64
	client, clock := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", "100")
			fmt.Fprint(w, `{"data":`)
			return
		}
		fmt.Fprint(w, `{"data":{"answer":1}}`)
	})
	if err := answerQuery(context.Background(), client); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if err := answerQuery(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(clock.waits, []time.Duration{5 * time.Second}) {
		t.Fatalf("interrupted response bypassed server failure backoff: %v", clock.waits)
	}
}

func TestCanceledRateWaitDoesNotIssueAnotherRequest(t *testing.T) {
	var calls atomic.Int64
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{"data":{"answer":1}}`) })
	if err := answerQuery(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.options.Wait = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
	if err := answerQuery(ctx, client); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("canceled limiter sent a request")
	}
}

func TestGraphQLClientUsesABoundedRequestDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	client.options.RequestTimeout = 20 * time.Millisecond
	if err := answerQuery(context.Background(), client); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("HTTP deadline was lost: %v", err)
	}
}

func TestConcurrentGraphQLCallsShareOnePacedRequestBudget(t *testing.T) {
	var active, maxActive atomic.Int64
	client, clock := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maxActive.Load()
			if old >= current || maxActive.CompareAndSwap(old, current) {
				break
			}
		}
		fmt.Fprint(w, `{"data":{"answer":1}}`)
	})
	const workers = 8
	results := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Go(func() { results <- answerQuery(context.Background(), client) })
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maxActive.Load() != 1 || len(clock.waits) != workers-1 {
		t.Fatalf("request budget was not shared: active=%d waits=%v", maxActive.Load(), clock.waits)
	}
	for _, delay := range clock.waits {
		if delay != time.Minute/29 {
			t.Fatal("concurrent calls bypassed pacing")
		}
	}
}

func TestGraphQLClientDefaultsAreBoundedAndInvalidOptionsAreRejected(t *testing.T) {
	client, err := NewClient(ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if client.options.RequestsPerMinute != 29 || client.options.RequestTimeout != 15*time.Second || client.options.MaxResponseBytes != 1<<20 {
		t.Fatal("unbounded defaults")
	}
	for _, options := range []ClientOptions{{Endpoint: "file:///invalid"}, {RequestsPerMinute: 30}, {RequestsPerMinute: -1}, {RequestTimeout: -time.Second}, {MaxResponseBytes: -1}} {
		if _, err := NewClient(options); err == nil {
			t.Fatal("invalid client options accepted")
		}
	}
}
