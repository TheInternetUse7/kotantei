package mal

import (
	"context"
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

	"kotantei/internal/update"
)

func testEnricher(t *testing.T, handler http.HandlerFunc, options EnricherOptions) (*Enricher, *testClock) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	clock := &testClock{now: time.Unix(200, 0).UTC()}
	options.Endpoint = server.URL + "/v1"
	options.HTTPClient = server.Client()
	options.Now = clock.Now
	options.Wait = clock.Wait
	enricher, err := NewEnricher(options)
	if err != nil {
		t.Fatal(err)
	}
	return enricher, clock
}
func tenraiMedia(id string, description string) string {
	return fmt.Sprintf(`{"data":{"mal_id":%s,"title":"Enriched title","type":"TV","status":"Finished Airing","episodes":12,"score":8.5,"synopsis":%q,"images":{"jpg":{"large_image_url":"https://cdn.myanimelist.net/cover.jpg"}},"themes":[{"name":"Theme"}],"demographics":[{"name":"Seinen"}]}}`, id, description)
}

func TestTenraiMediaAndJapaneseVoiceActorsAreProjectedWithoutCredentials(t *testing.T) {
	enricher, _ := testEnricher(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-MAL-CLIENT-ID") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Server-Key") != "" {
			t.Error("credentials leaked into optional enrichment")
		}
		if r.URL.Path == "/v1/anime/42" {
			fmt.Fprint(w, tenraiMedia("42", "Synopsis"))
			return
		}
		if r.URL.Path != "/v1/anime/42/characters" {
			t.Error("wrong enrichment endpoint", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":[{"voice_actors":[{"language":"Japanese","person":{"mal_id":7,"name":"Actor","url":"https://myanimelist.net/people/7"}},{"language":"English","person":{"mal_id":8,"name":"Dub","url":"https://myanimelist.net/people/8"}},{"language":"Japanese","person":{"mal_id":9,"name":"Unsafe","url":"https://untrusted.example/people/9"}}]}]}`)
	}, EnricherOptions{})
	extra, err := enricher.Lookup(context.Background(), update.Subject{Kind: update.AnimeSubject, ID: "42"}, Themes|Demographic|Seiyu|Synopsis)
	if err != nil {
		t.Fatal(err)
	}
	if extra.Media == nil || extra.Media.ID != 42 || extra.Media.Title != "Enriched title" || !reflect.DeepEqual(extra.Themes, []string{"Theme"}) || !reflect.DeepEqual(extra.Demographics, []string{"Seinen"}) || len(extra.VoiceActors) != 1 || extra.VoiceActors[0].Name != "Actor" {
		t.Fatalf("incorrect enrichment: %+v", extra)
	}
}

func TestTenraiFavoritesCoverCharactersPeopleCompaniesAndManga(t *testing.T) {
	enricher, _ := testEnricher(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/characters/44/full":
			fmt.Fprint(w, `{"data":{"mal_id":44,"about":"Character description","anime":[{"role":"Main","anime":{"title":"Work","url":"https://myanimelist.net/anime/42"}}]}}`)
		case "/v1/people/45/full":
			fmt.Fprint(w, `{"data":{"mal_id":45,"about":"Person description","voices":[{"role":"Main","anime":{"title":"Work","url":"https://myanimelist.net/anime/42"},"character":{"name":"Character"}}]}}`)
		case "/v1/anime":
			if r.URL.Query().Get("producers") != "46" || r.URL.Query().Get("limit") != "1" {
				t.Error("unbounded studio lookup")
			}
			fmt.Fprint(w, `{"data":[{"mal_id":42,"title":"Work","url":"https://myanimelist.net/anime/42"}]}`)
		case "/v1/manga/43":
			fmt.Fprint(w, tenraiMedia("43", "Manga synopsis"))
		default:
			t.Error("unexpected endpoint", r.URL.Path)
			w.WriteHeader(404)
		}
	}, EnricherOptions{})
	for _, subject := range []update.Subject{{Kind: update.CharacterSubject, ID: "44"}, {Kind: update.PersonSubject, ID: "45"}, {Kind: update.CompanySubject, ID: "46"}, {Kind: update.MangaSubject, ID: "43"}} {
		extra, err := enricher.Lookup(context.Background(), subject, Synopsis)
		if err != nil {
			t.Fatal(err)
		}
		if subject.Kind == update.MangaSubject {
			if extra.Media == nil {
				t.Fatal("manga details missing")
			}
		} else if extra.KnownFor == nil || extra.KnownFor.Title != "Work" {
			t.Fatal("favorite related work missing", extra)
		}
	}
}

func TestEnrichmentCacheHasTTLBoundedEvictionAndDetachedValues(t *testing.T) {
	var calls atomic.Int64
	enricher, clock := testEnricher(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		fmt.Fprint(w, tenraiMedia(id, "Synopsis"))
	}, EnricherOptions{MaxEntries: 1, TTL: time.Minute})
	first := update.Subject{Kind: update.AnimeSubject, ID: "42"}
	extra, err := enricher.Lookup(context.Background(), first, Themes)
	if err != nil {
		t.Fatal(err)
	}
	extra.Themes[0] = "Changed"
	extra.Media.Title = "Changed"
	again, err := enricher.Lookup(context.Background(), first, Themes)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || again.Themes[0] != "Theme" || again.Media.Title == "Changed" {
		t.Fatal("cache was bypassed or shared mutable data")
	}
	if _, err := enricher.Lookup(context.Background(), update.Subject{Kind: update.AnimeSubject, ID: "43"}, Themes); err != nil {
		t.Fatal(err)
	}
	if _, err := enricher.Lookup(context.Background(), first, Themes); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatal("entry capacity failed to evict")
	}
	clock.mu.Lock()
	clock.now = clock.now.Add(61 * time.Second)
	clock.mu.Unlock()
	if _, err := enricher.Lookup(context.Background(), first, Themes); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatal("cache TTL did not expire")
	}
}

func TestEnrichmentFailureCacheAndCooldownPreventHammering(t *testing.T) {
	var calls atomic.Int64
	enricher, clock := testEnricher(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(503)
		fmt.Fprint(w, "DO-NOT-PRINT-SECRET")
	}, EnricherOptions{})
	for range 3 {
		_, err := enricher.Lookup(context.Background(), update.Subject{Kind: update.AnimeSubject, ID: "42"}, Themes)
		if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "DO-NOT-PRINT") {
			t.Fatal("unsafe enrichment error", err)
		}
	}
	if _, err := enricher.Lookup(context.Background(), update.Subject{Kind: update.AnimeSubject, ID: "43"}, Themes); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(clock.waits) != 0 {
		t.Fatal("optional failures waited through cooldown or hammered the service")
	}
}

func TestOversizedOrWrongEnrichmentIsRejectedAndCacheBytesAreBounded(t *testing.T) {
	for _, body := range []string{tenraiMedia("999", "Description"), tenraiMedia("42", strings.Repeat("x", 70<<10)), `{"data":null}`, `{"data":{"mal_id":42},"error":"failed"}`} {
		enricher, _ := testEnricher(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }, EnricherOptions{})
		if _, err := enricher.Lookup(context.Background(), update.Subject{Kind: update.AnimeSubject, ID: "42"}, Synopsis); err == nil {
			t.Fatal("bad or oversized optional metadata accepted")
		}
	}
	var calls atomic.Int64
	enricher, _ := testEnricher(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		fmt.Fprint(w, tenraiMedia(id, "Synopsis"))
	}, EnricherOptions{MaxBytes: 500})
	for _, id := range []string{"42", "43", "42"} {
		if _, err := enricher.Lookup(context.Background(), update.Subject{Kind: update.AnimeSubject, ID: id}, Themes); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Fatal("cache byte limit failed to evict")
	}
}

func TestConcurrentEnrichmentUsesOneFetchPerCachedKey(t *testing.T) {
	var calls atomic.Int64
	enricher, _ := testEnricher(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, tenraiMedia("42", "Synopsis"))
	}, EnricherOptions{})
	const workers = 8
	results := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			_, err := enricher.Lookup(context.Background(), update.Subject{Kind: update.AnimeSubject, ID: "42"}, Themes)
			results <- err
		})
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("concurrent cache misses stampeded")
	}
}

func TestLocalCollectionBudgetDoesNotPoisonTheSharedEnrichmentCache(t *testing.T) {
	var calls atomic.Int64
	enricher, _ := testEnricher(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/characters") {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		fmt.Fprint(w, tenraiMedia("42", "Synopsis"))
	}, EnricherOptions{})
	budget := &collectionBudget{options: ProviderOptions{MaxRequests: 1}}
	ctx := context.WithValue(context.Background(), budgetKey{}, budget)
	subject := update.Subject{Kind: update.AnimeSubject, ID: "42"}
	if _, err := enricher.Lookup(ctx, subject, Seiyu); !errors.Is(err, ErrCollectionBudget) {
		t.Fatal("expected the account-local request budget to stop the second request", err)
	}
	if _, err := enricher.Lookup(context.Background(), subject, Seiyu); err != nil {
		t.Fatal("one account's budget failure poisoned another lookup", err)
	}
	if calls.Load() != 3 {
		t.Fatal("a fresh collection did not get a fresh lookup after a local budget failure")
	}
}

func TestTenraiMangaAuthorsAreAvailableToFavoriteFormatting(t *testing.T) {
	enricher, _ := testEnricher(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"mal_id":43,"title":"Manga","type":"Manga","authors":[{"mal_id":7,"name":"Author, Name"}]}}`)
	}, EnricherOptions{})
	extra, err := enricher.Lookup(context.Background(), update.Subject{Kind: update.MangaSubject, ID: "43"}, Mangakas)
	if err != nil {
		t.Fatal(err)
	}
	if extra.Media == nil || len(extra.Media.Authors) != 1 || extra.Media.Authors[0].Node.LastName != "Author" || extra.Media.Authors[0].Node.FirstName != "Name" {
		t.Fatal("manga favorite author metadata was discarded", extra)
	}
}

func TestEnricherRejectsInvalidLimitsAndCancellation(t *testing.T) {
	for _, options := range []EnricherOptions{{Endpoint: "file:///invalid"}, {MaxEntries: -1}, {MaxBytes: -1}, {RequestTimeout: -time.Second}, {TTL: -time.Second}} {
		if _, err := NewEnricher(options); err == nil {
			t.Fatal("invalid enrichment options accepted")
		}
	}
	enricher, _ := testEnricher(t, func(w http.ResponseWriter, r *http.Request) { t.Error("canceled request issued") }, EnricherOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := enricher.Lookup(ctx, update.Subject{Kind: update.AnimeSubject, ID: "42"}, Themes); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
