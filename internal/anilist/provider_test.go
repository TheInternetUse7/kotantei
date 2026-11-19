package anilist

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"kotantei/internal/notification"
	"kotantei/internal/store"
	"kotantei/internal/update"
)

type fixtureAPI struct {
	mutex         sync.Mutex
	user          User
	activities    []Activity
	reviews       []Review
	entries       []Entry
	favorites     map[string][]int64
	details       map[string][]FavoriteInfo
	enrichment    []Media
	requests      []requestJSON
	failOperation string
	failPage      int
	malformedPage bool
	onRequest     func(requestJSON)
}

func numberVariable(request requestJSON, key string) int64 {
	var number int64
	_ = json.Unmarshal(request.Variables[key], &number)
	return number
}
func boolVariable(request requestJSON, key string) bool {
	var value bool
	_ = json.Unmarshal(request.Variables[key], &value)
	return value
}
func stringVariable(request requestJSON, key string) string {
	var value string
	_ = json.Unmarshal(request.Variables[key], &value)
	return value
}
func idsVariable(request requestJSON) []int64 {
	var ids []int64
	_ = json.Unmarshal(request.Variables["ids"], &ids)
	return ids
}
func pageData[T any](items []T, number, size int) map[string]any {
	start := min(len(items), (number-1)*size)
	end := min(len(items), start+size)
	page := append([]T{}, items[start:end]...)
	return map[string]any{"pageInfo": map[string]any{"currentPage": number, "hasNextPage": end < len(items)}, "items": page}
}

func (api *fixtureAPI) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var request requestJSON
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Error(err)
		w.WriteHeader(400)
		return
	}
	api.mutex.Lock()
	defer api.mutex.Unlock()
	api.requests = append(api.requests, request)
	if api.onRequest != nil {
		api.onRequest(request)
	}
	if request.OperationName == api.failOperation && (api.failPage == 0 || numberVariable(request, "page") == int64(api.failPage)) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": nil, "errors": []any{map[string]any{"status": 503, "message": "fixture unavailable"}}})
		return
	}
	var data any
	size := int(numberVariable(request, "perPage"))
	if size < 1 {
		size = 25
	}
	pageNumber := int(numberVariable(request, "page"))
	if pageNumber < 1 {
		pageNumber = 1
	}
	switch {
	case request.OperationName == "KotanteiProfile":
		data = map[string]any{"User": api.user}
	case request.OperationName == "KotanteiActivities":
		if strings.Contains(request.Query, "isPrivate") {
			t.Error("ListActivity has no isPrivate GraphQL field")
		}
		var filtered []Activity
		since, until := numberVariable(request, "since"), numberVariable(request, "until")
		kind := stringVariable(request, "type")
		for _, activity := range api.activities {
			if activity.CreatedAt <= since || activity.CreatedAt >= until {
				continue
			}
			if activity.Media != nil && ((kind == "ANIME_LIST" && activity.Media.Type != "ANIME") || (kind == "MANGA_LIST" && activity.Media.Type != "MANGA")) {
				continue
			}
			filtered = append(filtered, activity)
		}
		slices.SortFunc(filtered, func(a, b Activity) int {
			if a.ID > b.ID {
				return -1
			}
			if a.ID < b.ID {
				return 1
			}
			return 0
		})
		page := pageData(filtered, pageNumber, size)
		if api.malformedPage {
			delete(page, "pageInfo")
		}
		data = map[string]any{"Page": page}
	case request.OperationName == "KotanteiReviews":
		rows := slices.Clone(api.reviews)
		slices.SortFunc(rows, func(a, b Review) int {
			if a.CreatedAt > b.CreatedAt {
				return -1
			}
			if a.CreatedAt < b.CreatedAt {
				return 1
			}
			return 0
		})
		data = map[string]any{"Page": pageData(rows, pageNumber, size)}
	case request.OperationName == "KotanteiEntries":
		if strings.Contains(request.Query, "MediaListCollection") {
			t.Error("polling fetched the entire list")
		}
		ids := idsVariable(request)
		var rows []Entry
		for _, entry := range api.entries {
			if slices.Contains(ids, entry.MediaID) {
				rows = append(rows, entry)
			}
		}
		data = map[string]any{"Page": pageData(rows, 1, size)}
	case request.OperationName == "KotanteiFavorites":
		connections := map[string]any{}
		for _, kind := range []string{"anime", "manga", "characters", "staff", "studios"} {
			if !boolVariable(request, kind+"Enabled") {
				continue
			}
			var rows []map[string]int64
			for _, id := range api.favorites[kind] {
				rows = append(rows, map[string]int64{"id": id})
			}
			connections[kind] = pageData(rows, int(numberVariable(request, kind+"Page")), size)
		}
		data = map[string]any{"User": map[string]any{"id": api.user.ID, "favourites": connections}}
	case strings.HasPrefix(request.OperationName, "KotanteiFavoriteDetails"):
		kind := strings.ToLower(strings.TrimPrefix(request.OperationName, "KotanteiFavoriteDetails"))
		ids := idsVariable(request)
		var rows []FavoriteInfo
		for _, info := range api.details[kind] {
			if slices.Contains(ids, info.ID) {
				rows = append(rows, info)
			}
		}
		data = map[string]any{"Page": pageData(rows, 1, size)}
	case request.OperationName == "KotanteiEnrichment":
		ids := idsVariable(request)
		var rows []Media
		for _, media := range api.enrichment {
			if slices.Contains(ids, media.ID) {
				rows = append(rows, media)
			}
		}
		data = map[string]any{"Page": pageData(rows, 1, size)}
	default:
		t.Errorf("unexpected operation %s", request.OperationName)
		w.WriteHeader(400)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func providerFixture(t *testing.T, seed bool, options ProviderOptions) (*store.Store, *store.Outbox, *Provider, *fixtureAPI) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "anilist.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.DB().Exec(`INSERT INTO guilds(id,channel_id) VALUES ('10','11'),('20','21')`); err != nil {
		t.Fatal(err)
	}
	if seed {
		if _, err := s.DB().Exec(`INSERT INTO users(id) VALUES ('1');INSERT INTO guild_users(guild_id,user_id) VALUES ('10','1'),('20','1');INSERT INTO accounts(id,provider,external_id,user_id,username) VALUES (1,'anilist','101','1','Reader');INSERT INTO checkpoints(account_id,kind,value) VALUES (1,'activity',100),(1,'review',100)`); err != nil {
			t.Fatal(err)
		}
	}
	queue, err := store.NewOutbox(s, store.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	api := &fixtureAPI{user: exampleUser(), favorites: map[string][]int64{}, details: map[string][]FavoriteInfo{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { api.serve(t, w, r) }))
	t.Cleanup(server.Close)
	clock := &testClock{now: time.Unix(200, 0).UTC()}
	client, err := NewClient(ClientOptions{Endpoint: server.URL, HTTPClient: server.Client(), Now: clock.Now, Wait: clock.Wait})
	if err != nil {
		t.Fatal(err)
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Unix(200, 0).UTC() }
	}
	provider, err := NewProvider(queue, client, options)
	if err != nil {
		t.Fatal(err)
	}
	return s, queue, provider, api
}

func activity(id, mediaID, at int64, kind string) Activity {
	media := exampleMedia()
	media.ID = mediaID
	media.Type = kind
	return Activity{ID: id, UserID: 101, CreatedAt: at, Typename: "ListActivity", Status: "watched episode", Progress: "3", Media: &media}
}
func listEntry(id int64, kind string) Entry {
	media := exampleMedia()
	media.ID = id
	media.Type = kind
	return Entry{UserID: 101, MediaID: id, Status: "CURRENT", Score: 8.5, Media: &media}
}
func countRecords(t *testing.T, s *store.Store, table string) int {
	t.Helper()
	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
func operationCount(api *fixtureAPI, name string) int {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	count := 0
	for _, request := range api.requests {
		if request.OperationName == name {
			count++
		}
	}
	return count
}

func TestLinkBaselinesExistingHistoryAndTimestampBoundaryWithoutMessages(t *testing.T) {
	s, queue, provider, api := providerFixture(t, false, ProviderOptions{PageSize: 1})
	api.activities = []Activity{activity(9, 42, 199, "ANIME"), activity(10, 43, 200, "MANGA")}
	media := exampleMedia()
	api.reviews = []Review{{ID: 20, UserID: 101, CreatedAt: 200, Media: &media}, {ID: 19, UserID: 101, CreatedAt: 199, Media: &media}}
	api.favorites["anime"] = []int64{42, 43}
	api.favorites["characters"] = []int64{44}
	result, err := provider.Link(context.Background(), "1", "10", "Reader")
	if err != nil {
		t.Fatal(err)
	}
	state, err := queue.Snapshot(context.Background(), result.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || state.ExternalID != "101" || state.Features != int64(DefaultFeatures) || state.Cursors[update.Activity].Value != 200 || !reflect.DeepEqual(state.Cursors[update.Activity].SourceIDs, []string{"10"}) || !reflect.DeepEqual(state.Cursors[update.ReviewStream].SourceIDs, []string{"20"}) || len(state.Favorites) != 3 {
		t.Fatalf("incorrect quiet baseline: %+v", state)
	}
	if countRecords(t, s, "events") != 0 {
		t.Fatal("linking posted history")
	}
	if _, err := provider.Check(context.Background(), result.AccountID); err != nil {
		t.Fatal(err)
	}
	if countRecords(t, s, "events") != 0 {
		t.Fatal("first poll replayed the linked baseline")
	}
}

func TestLinkingAnExistingAccountAddsMembershipWithoutFetchingOrResetting(t *testing.T) {
	_, queue, provider, api := providerFixture(t, true, ProviderOptions{})
	result, err := provider.Link(context.Background(), "1", "10", "")
	if err != nil {
		t.Fatal(err)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created || result.AccountID != 1 || state.Cursors[update.Activity].Value != 100 || len(api.requests) != 0 {
		t.Fatal("existing account was refetched or reset")
	}
}

func TestPollCollectsIndependentPagesAndConsolidatesOnlyActivityTitles(t *testing.T) {
	s, queue, provider, api := providerFixture(t, true, ProviderOptions{PageSize: 2})
	if _, err := s.DB().Exec(`UPDATE accounts SET features=features|4096 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	api.activities = []Activity{activity(1, 42, 101, "ANIME"), activity(2, 43, 102, "MANGA"), activity(3, 42, 103, "ANIME"), activity(4, 44, 103, "ANIME")}
	api.entries = []Entry{listEntry(42, "ANIME"), listEntry(43, "MANGA"), listEntry(44, "ANIME")}
	media := exampleMedia()
	api.reviews = []Review{{ID: 10, UserID: 101, CreatedAt: 103, Summary: "First", Media: &media}, {ID: 11, UserID: 101, CreatedAt: 103, Summary: "Second", Media: &media}, {ID: 12, UserID: 101, CreatedAt: 102, Summary: "Third", Media: &media}, {ID: 9, UserID: 101, CreatedAt: 99, Media: &media}}
	api.favorites["anime"] = []int64{50, 51, 52}
	api.favorites["staff"] = []int64{53}
	result, err := provider.Check(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 10 || result.Deliveries != 20 {
		t.Fatalf("lost activities/reviews/favorites: %+v", result)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.Cursors[update.Activity].SourceIDs, []string{"3", "4"}) || !reflect.DeepEqual(state.Cursors[update.ReviewStream].SourceIDs, []string{"10", "11"}) || len(state.Favorites) != 4 {
		t.Fatalf("incomplete checkpoint boundary: %+v", state)
	}
	if operationCount(api, "KotanteiActivities") != 2 || operationCount(api, "KotanteiReviews") != 2 || operationCount(api, "KotanteiFavorites") != 2 {
		t.Fatal("streams did not paginate independently")
	}
	if _, err := provider.Check(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if countRecords(t, s, "events") != 10 {
		t.Fatal("repeated poll replayed activity or favorite changes")
	}
}

func TestNoActivityMeansNoStandaloneScoreChangeOrFullListFetch(t *testing.T) {
	s, _, provider, api := providerFixture(t, true, ProviderOptions{})
	api.entries = []Entry{listEntry(42, "ANIME")}
	api.entries[0].Score = 9
	result, err := provider.Check(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 0 || operationCount(api, "KotanteiEntries") != 0 || operationCount(api, "KotanteiReviews") != 0 || countRecords(t, s, "events") != 0 {
		t.Fatal("score-only change or disabled reviews were tracked")
	}
}

func TestPrivateDraftReviewsAreConsumedWithoutPostingTheirContents(t *testing.T) {
	s, queue, provider, api := providerFixture(t, true, ProviderOptions{})
	if _, err := s.DB().Exec(`UPDATE accounts SET features=features|4096 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	media := exampleMedia()
	api.reviews = []Review{{ID: 10, UserID: 101, CreatedAt: 101, Private: true, Summary: "DO-NOT-EXPOSE", Media: &media}}
	result, err := provider.Check(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 0 || state.Cursors[update.ReviewStream].Value != 101 || countRecords(t, s, "events") != 0 {
		t.Fatal("private draft review was posted or blocked the cursor")
	}
}

func TestMalformedPaginationOrWrongProfileCannotAdvanceAnyCheckpoint(t *testing.T) {
	for _, wrongProfile := range []bool{false, true} {
		s, queue, provider, api := providerFixture(t, true, ProviderOptions{})
		api.activities = []Activity{activity(1, 42, 101, "ANIME")}
		api.entries = []Entry{listEntry(42, "ANIME")}
		if wrongProfile {
			api.user.ID = 102
		} else {
			api.malformedPage = true
		}
		if _, err := provider.Check(context.Background(), 1); !errors.Is(err, ErrMalformed) {
			t.Fatalf("incomplete/wrong data was accepted: %v", err)
		}
		state, err := queue.Snapshot(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if state.Revision != 0 || countRecords(t, s, "events") != 0 {
			t.Fatal("malformed collection committed")
		}
	}
}

func TestPublicFavoriteMetadataIsRetainedForAllFiveCategories(t *testing.T) {
	_, queue, provider, api := providerFixture(t, true, ProviderOptions{})
	anime := exampleMedia()
	manga := exampleMedia()
	manga.ID = 43
	manga.Type = "MANGA"
	api.favorites = map[string][]int64{"anime": {42}, "manga": {43}, "characters": {44}, "staff": {45}, "studios": {46}}
	api.details = map[string][]FavoriteInfo{
		"anime": {{Media: anime}}, "manga": {{Media: manga}},
		"characters": {{Media: Media{ID: 44}, Name: Name{Full: "Character"}}},
		"staff":      {{Media: Media{ID: 45}, Name: Name{Full: "Staff"}}},
		"studios":    {{Media: Media{ID: 46}, Label: "Studio"}},
	}
	result, err := provider.Check(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 5 || result.MetadataFailures != 0 || len(state.Favorites) != 5 {
		t.Fatalf("favorite metadata was lost: %+v", result)
	}
	for _, favorite := range state.Favorites {
		if favorite.Name == "" || len(favorite.Extra) < 3 {
			t.Fatalf("missing stored favorite display data: %+v", favorite)
		}
	}
}

func TestProviderBackpressureKeepsActivityAndFavoriteBaselinesUnchanged(t *testing.T) {
	s, queue, provider, api := providerFixture(t, true, ProviderOptions{})
	limits := store.DefaultLimits()
	limits.MaxPending = 1
	limited, err := store.NewOutbox(s, limits)
	if err != nil {
		t.Fatal(err)
	}
	provider.queue = limited
	api.activities = []Activity{activity(1, 42, 101, "ANIME")}
	api.entries = []Entry{listEntry(42, "ANIME")}
	api.favorites["anime"] = []int64{43}
	if _, err := provider.Check(context.Background(), 1); !errors.Is(err, store.ErrBackpressure) {
		t.Fatalf("active queue budget ignored: %v", err)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 0 || state.Cursors[update.Activity].Value != 100 || len(state.Favorites) != 0 || countRecords(t, s, "events") != 0 {
		t.Fatal("backpressure changed a baseline")
	}
}

func TestConcurrentAniListPollsCannotRecordTheSameActivityTwice(t *testing.T) {
	s, _, provider, api := providerFixture(t, true, ProviderOptions{})
	api.activities = []Activity{activity(1, 42, 101, "ANIME")}
	api.entries = []Entry{listEntry(42, "ANIME")}
	const workers = 8
	results := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Go(func() { _, err := provider.Check(context.Background(), 1); results <- err })
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, store.ErrStalePoll) {
			t.Fatal(err)
		}
	}
	if countRecords(t, s, "events") != 1 || countRecords(t, s, "deliveries") != 2 {
		t.Fatal("concurrent provider polls replayed an activity")
	}
}

func TestMissingAndPrivateListDataAreConsumedWithoutNotifications(t *testing.T) {
	s, queue, provider, api := providerFixture(t, true, ProviderOptions{})
	deleted := activity(3, 44, 103, "ANIME")
	deleted.Media = nil
	api.activities = []Activity{activity(1, 42, 101, "ANIME"), activity(2, 43, 102, "MANGA"), deleted}
	private := listEntry(43, "MANGA")
	private.Private = true
	private.Notes = "DO-NOT-EXPOSE"
	api.entries = []Entry{private}
	result, err := provider.Check(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 0 || state.Cursors[update.Activity].Value != 103 || countRecords(t, s, "events") != 0 {
		t.Fatal("unavailable list data blocked or leaked into tracking")
	}
}

func TestOptionalMetadataFailureDoesNotStopCoreActivityOrFavoriteTracking(t *testing.T) {
	for _, operation := range []string{"KotanteiEnrichment", "KotanteiFavoriteDetailsAnime"} {
		s, _, provider, api := providerFixture(t, true, ProviderOptions{})
		if _, err := s.DB().Exec(`UPDATE accounts SET features=features|128 WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		api.activities = []Activity{activity(1, 42, 101, "ANIME")}
		api.entries = []Entry{listEntry(42, "ANIME")}
		api.favorites["anime"] = []int64{43}
		api.failOperation = operation
		result, err := provider.Check(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if result.Events != 2 || result.MetadataFailures == 0 {
			t.Fatalf("optional failure stopped tracking or was hidden: %+v", result)
		}
	}
}

func TestUnavailableMetadataDoesNotWaitThroughCooldownBeforeRecordingCore(t *testing.T) {
	s, _, provider, api := providerFixture(t, true, ProviderOptions{PageSize: 1})
	if _, err := s.DB().Exec(`UPDATE accounts SET features=features|128 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	api.activities = []Activity{activity(1, 42, 101, "ANIME"), activity(2, 43, 102, "ANIME")}
	api.entries = []Entry{listEntry(42, "ANIME"), listEntry(43, "ANIME")}
	api.favorites["anime"] = []int64{44}
	api.failOperation = "KotanteiEnrichment"
	wait := provider.client.options.Wait
	cooldownWaits := 0
	provider.client.options.Wait = func(ctx context.Context, delay time.Duration) error {
		if delay >= 5*time.Second {
			cooldownWaits++
		}
		return wait(ctx, delay)
	}
	result, err := provider.Check(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 3 || result.MetadataFailures == 0 {
		t.Fatalf("metadata failure lost core notifications: %+v", result)
	}
	if cooldownWaits != 0 || operationCount(api, "KotanteiEnrichment") != 1 || operationCount(api, "KotanteiFavoriteDetailsAnime") != 0 {
		t.Fatal("optional requests waited through an outage cooldown instead of recording core notifications")
	}
}

func TestOversizedOptionalFavoriteMetadataFallsBackWithoutStoppingTracking(t *testing.T) {
	s, queue, provider, api := providerFixture(t, true, ProviderOptions{})
	if _, err := s.DB().Exec(`UPDATE accounts SET features=features|128 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	api.activities = []Activity{activity(1, 42, 101, "ANIME")}
	api.entries = []Entry{listEntry(42, "ANIME")}
	api.favorites["anime"] = []int64{43}
	media := exampleMedia()
	media.ID = 43
	media.Description = strings.Repeat("x", 70<<10)
	api.details["anime"] = []FavoriteInfo{{Media: media}}
	result, err := provider.Check(context.Background(), 1)
	if err != nil {
		t.Fatalf("optional metadata stopped core tracking: %v", err)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 2 || result.MetadataFailures == 0 || len(state.Favorites) != 1 || len(state.Favorites[0].Extra) > 64<<10 {
		t.Fatalf("oversized favorite metadata was retained or hid updates: %+v", result)
	}
}

func TestOptionalMediaMetadataCannotConsumeTheCoreMessageBudget(t *testing.T) {
	s, _, provider, api := providerFixture(t, true, ProviderOptions{})
	if _, err := s.DB().Exec(`UPDATE accounts SET features=features|128 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	row := activity(1, 42, 101, "ANIME")
	entry := listEntry(42, "ANIME")
	extra := Media{ID: 42, Description: strings.Repeat("x", 400)}
	api.activities = []Activity{row}
	api.entries = []Entry{entry}
	api.enrichment = []Media{extra}
	state := exampleState()
	state.Features |= int64(Description)
	base, err := activityPayload(row, entry, api.user, state)
	if err != nil {
		t.Fatal(err)
	}
	bytes := len(base) - 1
	for _, value := range []any{api.user, row, entry, extra} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		bytes += len(encoded)
	}
	provider.options.MaxBytes = bytes
	result, err := provider.Check(context.Background(), 1)
	if err != nil {
		t.Fatalf("optional metadata consumed space needed for the core notification: %v", err)
	}
	if result.Events != 1 {
		t.Fatal("core activity was lost under metadata pressure")
	}
}

func TestARequiredPageFailureDoesNotCommitAnyStream(t *testing.T) {
	for _, operation := range []string{"KotanteiActivities", "KotanteiReviews", "KotanteiFavorites", "KotanteiEntries"} {
		s, queue, provider, api := providerFixture(t, true, ProviderOptions{PageSize: 1})
		if _, err := s.DB().Exec(`UPDATE accounts SET features=features|4096 WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		api.activities = []Activity{activity(1, 42, 101, "ANIME"), activity(2, 43, 102, "ANIME")}
		api.entries = []Entry{listEntry(42, "ANIME"), listEntry(43, "ANIME")}
		media := exampleMedia()
		api.reviews = []Review{{ID: 10, UserID: 101, CreatedAt: 101, Media: &media}}
		api.favorites["anime"] = []int64{50, 51}
		api.failOperation = operation
		if operation == "KotanteiActivities" {
			api.failPage = 2
		}
		if _, err := provider.Check(context.Background(), 1); err == nil {
			t.Fatalf("%s failure ignored", operation)
		}
		state, err := queue.Snapshot(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if state.Revision != 0 || state.Cursors[update.Activity].Value != 100 || len(state.Favorites) != 0 || countRecords(t, s, "events") != 0 {
			t.Fatal("failed collection partly committed")
		}
	}
}

func TestPollRejectsIncompletePaginationBudgetsAndStaleSettings(t *testing.T) {
	for _, options := range []ProviderOptions{{MaxPages: 1}, {MaxRecords: 1}, {MaxFavorites: 1}, {MaxBytes: 10}} {
		s, queue, provider, api := providerFixture(t, true, options)
		api.activities = []Activity{activity(1, 42, 101, "ANIME"), activity(2, 43, 102, "ANIME")}
		api.entries = []Entry{listEntry(42, "ANIME"), listEntry(43, "ANIME")}
		api.favorites["anime"] = []int64{50, 51}
		if _, err := provider.Check(context.Background(), 1); !errors.Is(err, ErrCollectionBudget) {
			t.Fatalf("budget violation ignored: %v", err)
		}
		state, err := queue.Snapshot(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if state.Revision != 0 || countRecords(t, s, "events") != 0 {
			t.Fatal("oversized poll committed")
		}
	}
	s, queue, provider, api := providerFixture(t, true, ProviderOptions{})
	api.activities = []Activity{activity(1, 42, 101, "ANIME")}
	api.entries = []Entry{listEntry(42, "ANIME")}
	api.onRequest = func(request requestJSON) {
		if request.OperationName == "KotanteiProfile" {
			if _, err := s.DB().Exec(`UPDATE accounts SET revision=revision+1,features=0 WHERE id=1`); err != nil {
				t.Error(err)
			}
		}
	}
	if _, err := provider.Check(context.Background(), 1); !errors.Is(err, store.ErrStalePoll) {
		t.Fatalf("stale provider poll accepted: %v", err)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Features != 0 || countRecords(t, s, "events") != 0 {
		t.Fatal("poll overwrote concurrent settings")
	}
}

func TestPollingByStableIDRecoversRenamesAndUsesOnlyChangedMediaIDs(t *testing.T) {
	s, queue, provider, api := providerFixture(t, true, ProviderOptions{})
	api.user.Name = "Renamed"
	api.activities = []Activity{activity(1, 42, 101, "ANIME")}
	api.entries = []Entry{listEntry(42, "ANIME"), listEntry(43, "ANIME")}
	if _, err := provider.Check(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Username != "Renamed" {
		t.Fatal("rename was not recovered")
	}
	for _, request := range api.requests {
		if request.OperationName == "KotanteiProfile" && numberVariable(request, "id") != 101 {
			t.Fatal("polling depended on username")
		}
		if request.OperationName == "KotanteiEntries" && !reflect.DeepEqual(idsVariable(request), []int64{42}) {
			t.Fatal("unrelated list entries were fetched")
		}
	}
	var raw string
	if err := s.DB().QueryRow(`SELECT payload FROM events`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var message notification.Message
	if err := json.Unmarshal([]byte(raw), &message); err != nil {
		t.Fatal(err)
	}
	if message.Embeds[0].Author.Name != "Renamed" {
		t.Fatal("embed used stale account name")
	}
}

func TestCanceledProviderCollectionDoesNotCommit(t *testing.T) {
	s, queue, provider, api := providerFixture(t, true, ProviderOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api.onRequest = func(request requestJSON) { cancel() }
	if _, err := provider.Check(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was lost: %v", err)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 0 || countRecords(t, s, "events") != 0 {
		t.Fatal("canceled collection committed")
	}
}

func TestProviderRejectsInvalidOptions(t *testing.T) {
	_, queue, provider, _ := providerFixture(t, true, ProviderOptions{})
	if _, err := NewProvider(nil, provider.client, ProviderOptions{}); err == nil {
		t.Fatal("missing outbox accepted")
	}
	for _, options := range []ProviderOptions{{PageSize: 26}, {MaxPages: -1}, {MaxBytes: -1}, {CheckTimeout: -time.Second}} {
		if _, err := NewProvider(queue, provider.client, options); err == nil {
			t.Fatal("invalid provider limits accepted")
		}
	}
}
