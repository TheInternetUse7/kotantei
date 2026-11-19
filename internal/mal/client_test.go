package mal

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

type testClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) Wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	return nil
}

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *testClock) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	clock := &testClock{now: time.Unix(200, 0).UTC()}
	client, err := NewClient(ClientOptions{ClientID: "test-client", APIEndpoint: server.URL + "/v2", ProfileEndpoint: server.URL, HTTPClient: server.Client(), Now: clock.Now, Wait: clock.Wait})
	if err != nil {
		t.Fatal(err)
	}
	return client, clock
}
func writeList(w http.ResponseWriter, entries []Entry, next string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"data": entries, "paging": map[string]any{"next": next}})
}
func readAnime(client *Client) error {
	_, err := client.ListPage(context.Background(), "Reader", "anime", 0, 100, DefaultFeatures)
	return err
}

func TestOfficialListRequestsUseScopedClientIDAndCompleteTrackedFields(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v2/users/Reader/animelist" || r.Header.Get("X-MAL-CLIENT-ID") != "test-client" || r.Header.Get("Authorization") != "" {
			t.Error("incorrect public MAL request")
		}
		q := r.URL.Query()
		if q.Get("sort") != "list_updated_at" || q.Get("limit") != "100" || q.Get("offset") != "0" || q.Get("nsfw") != "true" {
			t.Error("incorrect list pagination/filter")
		}
		for _, field := range []string{"updated_at", "score", "num_episodes_watched", "num_times_rewatched", "tags", "comments", "start_date", "finish_date"} {
			if !strings.Contains(q.Get("fields"), field) {
				t.Errorf("missing stable tracked field %s", field)
			}
		}
		if strings.Contains(q.Get("fields"), "synopsis") {
			t.Error("disabled display-only field fetched")
		}
		writeList(w, []Entry{sampleEntry(42, 101, "anime")}, "")
	})
	page, err := client.ListPage(context.Background(), "Reader", "anime", 0, 100, DefaultFeatures)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Node.ID != 42 || page.NextOffset != nil {
		t.Fatal("list response was not decoded")
	}
}

func TestMangaQueriesIncludeVolumesAndFeatureControlledMetadata(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/users/Reader/mangalist" {
			t.Error("wrong manga endpoint")
		}
		for _, field := range []string{"num_volumes_read", "num_chapters_read", "is_rereading", "num_times_reread", "authors", "genres", "synopsis"} {
			if !strings.Contains(r.URL.Query().Get("fields"), field) {
				t.Error("missing manga field", field)
			}
		}
		writeList(w, []Entry{sampleEntry(43, 101, "manga")}, "")
	})
	if _, err := client.ListPage(context.Background(), "Reader", "manga", 0, 25, DefaultFeatures|Genres|Synopsis|Mangakas); err != nil {
		t.Fatal(err)
	}
}

func TestProfileAndCommentsRequestsNeverReceiveAPIClientCredentials(t *testing.T) {
	client, clock := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-MAL-CLIENT-ID") != "" || r.Header.Get("Authorization") != "" {
			t.Error("API credential leaked into scraping")
		}
		if r.URL.Path == "/profile/Reader" {
			fmt.Fprint(w, profileHTML(101, "Reader", ""))
			return
		}
		if r.URL.Path != "/comments.php" || r.URL.Query().Get("id") != "101" {
			t.Error("wrong stable-ID recovery request")
		}
		fmt.Fprint(w, `<div id="content"><div class="borderClass"><div><a href="/profile/Renamed">Renamed's Profile</a></div></div></div>`)
	})
	profile, err := client.Profile(context.Background(), "Reader", true)
	if err != nil || profile.ID != 101 {
		t.Fatal(profile, err)
	}
	name, err := client.RecoverUsername(context.Background(), 101)
	if err != nil || name != "Renamed" {
		t.Fatal(name, err)
	}
	if !reflect.DeepEqual(clock.waits, []time.Duration{3 * time.Second}) {
		t.Fatal("scraper requests bypassed pacing")
	}
}

func TestOfficialPaginationValidatesNextURLBeforeAnyFollowupRequest(t *testing.T) {
	for _, suffix := range []string{
		"?offset=100&limit=100", "?limit=100&offset=100&sort=list_updated_at",
	} {
		client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeList(w, []Entry{sampleEntry(42, 101, "anime")}, "http://"+r.Host+r.URL.Path+suffix)
		})
		page, err := client.ListPage(context.Background(), "Reader", "anime", 0, 100, DefaultFeatures)
		if err != nil || page.NextOffset == nil || *page.NextOffset != 100 {
			t.Fatal("valid continuation rejected", err)
		}
	}
	for _, next := range []string{"https://untrusted.example/v2/users/Reader/animelist?offset=100", "/v2/users/Other/animelist?offset=100", "?offset=0", "?offset=-1", "?offset=200", "?offset=100&limit=1000", "?offset=100&status=completed"} {
		client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeList(w, []Entry{sampleEntry(42, 101, "anime")}, next)
		})
		if err := readAnime(client); !errors.Is(err, ErrMalformed) {
			t.Fatalf("unsafe continuation accepted: %s %v", next, err)
		}
	}
}

func TestMALClientRejectsPartialMalformedAndOversizedResponses(t *testing.T) {
	for _, body := range []string{`{}`, `{"data":null,"paging":{}}`, `{"data":[],"paging":null}`, `{"data":[],"paging":{}} extra`, `{"data":[{"node":{"id":42},"list_status":{"status":"watching","updated_at":"1970-01-01T00:01:41Z"}}],"paging":{}}`, `{"data":[],"paging":{},"error":"DO-NOT-PRINT-SECRET"}`} {
		client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		err := readAnime(client)
		if !errors.Is(err, ErrMalformed) || strings.Contains(err.Error(), "DO-NOT-PRINT") {
			t.Fatal("partial/error data accepted or leaked", err)
		}
	}
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", 100)) })
	client.options.MaxResponseBytes = 16
	if err := readAnime(client); !errors.Is(err, ErrResponseBudget) {
		t.Fatal("response limit ignored", err)
	}
}

func TestMALHTTPFailuresAreSanitizedAndRespectRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{{401, ErrUnauthorized}, {403, ErrNotPublic}, {404, ErrNotFound}, {429, ErrRateLimited}, {503, ErrUnavailable}} {
		client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			fmt.Fprint(w, "DO-NOT-PRINT-SECRET")
		})
		err := readAnime(client)
		if !errors.Is(err, tc.want) || strings.Contains(err.Error(), "DO-NOT-PRINT") {
			t.Fatal("unsafe HTTP failure", err)
		}
	}
	var calls atomic.Int64
	client, clock := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "40")
			w.WriteHeader(429)
			return
		}
		writeList(w, []Entry{}, "")
	})
	if err := readAnime(client); !errors.Is(err, ErrRateLimited) || calls.Load() != 1 {
		t.Fatal("failed request was retried", err)
	}
	if err := readAnime(client); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(clock.waits, []time.Duration{40 * time.Second}) {
		t.Fatal("retry-after was shortened", clock.waits)
	}
}

func TestWebsiteCooldownDoesNotDelayOfficialListTracking(t *testing.T) {
	client, clock := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/profile/Reader" {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(503)
			return
		}
		writeList(w, []Entry{}, "")
	})
	if _, err := client.Profile(context.Background(), "Reader", true); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if err := readAnime(client); err != nil {
		t.Fatal(err)
	}
	if len(clock.waits) != 1 || clock.waits[0] != 3*time.Second {
		t.Fatal("website outage blocked official lists", clock.waits)
	}
	if _, err := client.Profile(context.Background(), "Reader", true); err == nil {
		t.Fatal("website cooldown was bypassed")
	}
	if len(clock.waits) != 1 {
		t.Fatal("optional scraper waited through its outage cooldown")
	}
}

func TestMALRedirectsCannotLeakCredentialsOrChangeListIdentity(t *testing.T) {
	var leaked atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	t.Cleanup(other.Close)
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) })
	if err := readAnime(client); err == nil {
		t.Fatal("API redirect followed")
	}
	if _, err := client.Profile(context.Background(), "Reader", true); err == nil {
		t.Fatal("cross-origin profile redirect followed")
	}
	if leaked.Load() != 0 {
		t.Fatal("request or credentials leaked to another host")
	}
}

func TestProfileFollowsOnlyBoundedSameOriginCanonicalRedirects(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/profile/Reader" {
			http.Redirect(w, r, "/profile/Renamed", 301)
			return
		}
		fmt.Fprint(w, profileHTML(101, "Renamed", ""))
	})
	profile, err := client.Profile(context.Background(), "Reader", true)
	if err != nil || profile.Username != "Renamed" {
		t.Fatal("canonical rename redirect failed", profile, err)
	}
}

func TestCanceledMALRateWaitNeverIssuesARequest(t *testing.T) {
	var calls atomic.Int64
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); writeList(w, []Entry{}, "") })
	if err := readAnime(client); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.options.Wait = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
	if _, err := client.ListPage(ctx, "Reader", "anime", 0, 100, DefaultFeatures); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("canceled pacing issued a request")
	}
}

func TestMALClientDefaultsAndInvalidOptions(t *testing.T) {
	client, err := NewClient(ClientOptions{ClientID: "test-client"})
	if err != nil {
		t.Fatal(err)
	}
	if client.options.RequestInterval != 3*time.Second || client.options.RequestTimeout != 15*time.Second || client.options.MaxResponseBytes != 1<<20 {
		t.Fatal("unbounded defaults")
	}
	for _, options := range []ClientOptions{{}, {ClientID: "secret\nheader"}, {ClientID: "test-client", APIEndpoint: "file:///invalid"}, {ClientID: "test-client", RequestInterval: -time.Second}, {ClientID: "test-client", RequestTimeout: -time.Second}, {ClientID: "test-client", MaxResponseBytes: -1}} {
		if _, err := NewClient(options); err == nil {
			t.Fatal("invalid MAL options accepted")
		}
	}
	if _, err := client.ListPage(context.Background(), "@me", "anime", 0, 100, DefaultFeatures); err == nil {
		t.Fatal("private authenticated identity allowed")
	}
}

func TestInvalidMALUsernamesNeverReachTheNetwork(t *testing.T) {
	var calls atomic.Int64
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(400)
	})
	for _, name := range []string{"@me", ".", "..", "Reader/Other", "Reader?token=secret"} {
		if _, err := client.Profile(context.Background(), name, false); !errors.Is(err, ErrMalformed) {
			t.Fatal("invalid profile username accepted", err)
		}
		if _, err := client.ListPage(context.Background(), name, "anime", 0, 100, DefaultFeatures); !errors.Is(err, ErrMalformed) {
			t.Fatal("invalid list username accepted", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid username made a public HTTP request")
	}
}

func TestMALInterruptedResponsesBackOffAndRespectLowerRequestBudgets(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		var calls atomic.Int64
		client, clock := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				if disconnect {
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					connection.Close()
				} else {
					w.Header().Set("Content-Length", "100")
					fmt.Fprint(w, `{"data":`)
				}
				return
			}
			writeList(w, []Entry{}, "")
		})
		client.options.RequestInterval = 30 * time.Second
		if err := readAnime(client); !errors.Is(err, ErrUnavailable) {
			t.Fatal("interrupted response was not a sanitized transport failure", err)
		}
		if err := readAnime(client); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(clock.waits, []time.Duration{30 * time.Second}) {
			t.Fatal("transport failure bypassed the lower request budget", clock.waits)
		}
	}
}

func TestMALHTTPDeadlineIncludesAStalledResponse(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	client.options.RequestTimeout = 20 * time.Millisecond
	if err := readAnime(client); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("HTTP deadline was lost", err)
	}
}

func TestConcurrentMALRequestsShareOnePacedTransport(t *testing.T) {
	var active, maximum atomic.Int64
	client, clock := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if old >= current || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		writeList(w, []Entry{}, "")
	})
	const workers = 8
	results := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Go(func() { results <- readAnime(client) })
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maximum.Load() != 1 || len(clock.waits) != workers-1 {
		t.Fatal("concurrent requests bypassed shared pacing")
	}
}
