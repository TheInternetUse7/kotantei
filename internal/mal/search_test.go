package mal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMALSearchFetchesBoundedOfficialCandidatesAndFiltersLocally(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v2/anime" || r.URL.Query().Get("q") != "Title" || r.URL.Query().Get("limit") != "25" || r.URL.Query().Get("nsfw") != "true" || r.Header.Get("X-MAL-CLIENT-ID") != "test-client" || strings.Contains(r.URL.Query().Get("fields"), "list_status") {
			t.Error("incorrect public catalogue request")
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"node": Media{ID: 41, Title: "Movie", Format: "movie"}}, {"node": Media{ID: 42, Title: "Title", Format: "tv"}}}, "paging": map[string]any{"next": "https://untrusted.example/not-followed"}})
	})
	results, err := client.Search(context.Background(), "anime", " Title ", "tv")
	if err != nil || len(results) != 1 || results[0].ID != 42 {
		t.Fatal(results, err)
	}
}

func TestMALSearchSupportsMangaAndDoesNotSilentlyAcceptMalformedCandidates(t *testing.T) {
	for _, valid := range []bool{true, false} {
		client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v2/manga" {
				t.Error("wrong manga search endpoint")
			}
			media := Media{ID: 43, Title: "Manga", Format: "manga"}
			if !valid {
				media.ID = 0
			}
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"node": media}}, "paging": map[string]any{}})
		})
		results, err := client.Search(context.Background(), "manga", "Manga", "")
		if valid {
			if err != nil || len(results) != 1 {
				t.Fatal(results, err)
			}
		} else if !errors.Is(err, ErrMalformed) {
			t.Fatal("malformed search candidate accepted", err)
		}
	}
}

func TestMALSearchUsesTheSharedOfficialCooldownAndRejectsInvalidInputBeforeHTTP(t *testing.T) {
	var calls atomic.Int64
	client, clock := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"data": []any{}, "paging": map[string]any{}})
	})
	for _, args := range [][3]string{{"anime", "", ""}, {"unknown", "Title", ""}, {"manga", "Title", "tv"}, {"anime", strings.Repeat("x", 201), ""}} {
		if _, err := client.Search(context.Background(), args[0], args[1], args[2]); !errors.Is(err, ErrMalformed) {
			t.Fatal("invalid search accepted", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid search reached MAL")
	}
	for range 2 {
		if _, err := client.Search(context.Background(), "anime", "Title", ""); err != nil {
			t.Fatal(err)
		}
	}
	if len(clock.waits) != 1 {
		t.Fatal("search bypassed shared official request pacing")
	}
}
