package anilist

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func searchClient(t *testing.T, handler http.HandlerFunc) (*Client, *testClock) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	// Reuse the normal client test clock so searches prove shared pacing.
	now := &testClock{now: time.Unix(200, 0).UTC()}
	client, err := NewClient(ClientOptions{Endpoint: server.URL, HTTPClient: server.Client(), Now: now.Now, Wait: now.Wait})
	if err != nil {
		t.Fatal(err)
	}
	return client, now
}

func TestAniListSearchUsesBoundedPublicGraphQLAndNativeFormatFiltering(t *testing.T) {
	client, _ := searchClient(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			OperationName, Query string
			Variables            map[string]any
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.OperationName != "KotanteiSearch" || request.Variables["type"] != "ANIME" || request.Variables["format"] != "TV" || request.Variables["perPage"] != float64(5) || request.Variables["search"] != "Title" || r.Header.Get("Authorization") != "" {
			t.Error("search did not use bounded public catalogue variables")
		}
		if strings.Contains(request.Query, "MediaList") || strings.Contains(request.Query, "mutation") {
			t.Error("search touched account state")
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"Page": map[string]any{"pageInfo": map[string]any{"currentPage": 1, "hasNextPage": false}, "items": []Media{{ID: 42, Type: "ANIME", Format: "TV", Title: Title{Romaji: "Title"}, SiteURL: "https://anilist.co/anime/42"}}}}})
	})
	items, err := client.Search(context.Background(), "anime", " Title ", "TV")
	if err != nil || len(items) != 1 || items[0].ID != 42 {
		t.Fatal(items, err)
	}
}

func TestAniListSearchAllowsMangaAndRejectsUntrustedResults(t *testing.T) {
	for _, items := range [][]Media{{{ID: 43, Type: "MANGA", Format: "MANGA", Title: Title{English: "Manga"}}}, {{ID: 43, Type: "ANIME", Title: Title{Romaji: "Wrong kind"}}}, {{ID: 0, Type: "MANGA", Title: Title{Romaji: "Wrong ID"}}}, {{ID: 43, Type: "MANGA"}}} {
		client, _ := searchClient(t, func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"Page": map[string]any{"pageInfo": map[string]any{"currentPage": 1, "hasNextPage": false}, "items": items}}})
		})
		results, err := client.Search(context.Background(), "manga", "Manga", "")
		if items[0].Type == "MANGA" && items[0].ID > 0 && items[0].Title.English != "" {
			if err != nil || len(results) != 1 {
				t.Fatal(results, err)
			}
		} else if !errors.Is(err, ErrMalformed) {
			t.Fatal("malformed search row accepted", err)
		}
	}
}

func TestAniListSearchValidationHappensBeforeHTTPAndSharesPacing(t *testing.T) {
	var calls atomic.Int64
	client, now := searchClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"Page": map[string]any{"pageInfo": map[string]any{"currentPage": 1, "hasNextPage": false}, "items": []Media{}}}})
	})
	for _, args := range [][3]string{{"anime", "", ""}, {"unknown", "Title", ""}, {"manga", "Title", "TV"}, {"anime", strings.Repeat("x", 201), ""}} {
		if _, err := client.Search(context.Background(), args[0], args[1], args[2]); !errors.Is(err, ErrMalformed) {
			t.Fatal("invalid search accepted", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid search made a request")
	}
	for range 2 {
		if _, err := client.Search(context.Background(), "anime", "Title", ""); err != nil {
			t.Fatal(err)
		}
	}
	if len(now.waits) != 1 {
		t.Fatal("search bypassed the shared provider rate limiter")
	}
}
