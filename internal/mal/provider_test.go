package mal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kotantei/internal/notification"
	"kotantei/internal/store"
	"kotantei/internal/update"
)

type fixtureProfile struct {
	id        int64
	favorites string
}
type fixtureAPI struct {
	mu            sync.Mutex
	profiles      map[string]fixtureProfile
	profileStatus map[string]int
	profileBody   string
	recovered     string
	entries       map[string][]Entry
	listStatus    map[string]int
	failOffset    map[string]int
	requests      []string
	badNext       bool
	onRequest     func(*http.Request)
}

func (api *fixtureAPI) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	api.mu.Lock()
	defer api.mu.Unlock()
	api.requests = append(api.requests, r.URL.Path+"?"+r.URL.RawQuery)
	if api.onRequest != nil {
		api.onRequest(r)
	}
	if strings.HasPrefix(r.URL.Path, "/profile/") {
		name := strings.TrimPrefix(r.URL.Path, "/profile/")
		if status := api.profileStatus[name]; status != 0 {
			w.WriteHeader(status)
			return
		}
		profile, ok := api.profiles[name]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if api.profileBody != "" {
			fmt.Fprint(w, api.profileBody)
		} else {
			fmt.Fprint(w, profileHTML(profile.id, name, profile.favorites))
		}
		return
	}
	if r.URL.Path == "/comments.php" {
		if api.recovered == "" {
			w.WriteHeader(404)
			return
		}
		fmt.Fprintf(w, `<div id="content"><div class="borderClass"><div><a href="/profile/%s">%s's Profile</a></div></div></div>`, api.recovered, api.recovered)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v2" || parts[1] != "users" {
		t.Error("unexpected path", r.URL.Path)
		w.WriteHeader(404)
		return
	}
	kind := strings.TrimSuffix(parts[3], "list")
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 {
		t.Error("missing page limit")
		w.WriteHeader(400)
		return
	}
	if status := api.listStatus[kind]; status != 0 && (api.failOffset[kind] < 0 || offset == api.failOffset[kind]) {
		w.WriteHeader(status)
		return
	}
	rows := slices.Clone(api.entries[kind])
	slices.SortStableFunc(rows, func(a, b Entry) int { return b.ListStatus.UpdatedAt.Compare(a.ListStatus.UpdatedAt) })
	start := min(offset, len(rows))
	end := min(start+limit, len(rows))
	next := ""
	if end < len(rows) {
		q := r.URL.Query()
		q.Set("offset", strconv.Itoa(offset+limit))
		next = "http://" + r.Host + r.URL.Path + "?" + q.Encode()
	}
	if api.badNext {
		next = "https://untrusted.example/list?offset=100"
	}
	writeList(w, append([]Entry{}, rows[start:end]...), next)
}

func providerFixture(t *testing.T, seed bool, options ProviderOptions) (*store.Store, *store.Outbox, *Provider, *fixtureAPI) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "mal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.DB().Exec(`INSERT INTO guilds(id,channel_id) VALUES ('10','11'),('20','21')`); err != nil {
		t.Fatal(err)
	}
	if seed {
		if _, err := s.DB().Exec(`INSERT INTO users(id) VALUES ('1');INSERT INTO guild_users(guild_id,user_id) VALUES ('10','1'),('20','1');INSERT INTO accounts(id,provider,external_id,user_id,username) VALUES (1,'mal','101','1','Reader');INSERT INTO checkpoints(account_id,kind,value) VALUES (1,'anime',100000000),(1,'manga',100000000)`); err != nil {
			t.Fatal(err)
		}
	}
	queue, err := store.NewOutbox(s, store.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	api := &fixtureAPI{profiles: map[string]fixtureProfile{"Reader": {id: 101}}, profileStatus: map[string]int{}, entries: map[string][]Entry{}, listStatus: map[string]int{}, failOffset: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { api.serve(t, w, r) }))
	t.Cleanup(server.Close)
	clock := &testClock{now: time.Unix(200, 0).UTC()}
	client, err := NewClient(ClientOptions{ClientID: "test-client", APIEndpoint: server.URL + "/v2", ProfileEndpoint: server.URL, HTTPClient: server.Client(), Now: clock.Now, Wait: clock.Wait})
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
func countRows(t *testing.T, s *store.Store, table string) int {
	t.Helper()
	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
func requestCount(api *fixtureAPI, prefix string) int {
	api.mu.Lock()
	defer api.mu.Unlock()
	count := 0
	for _, path := range api.requests {
		if strings.HasPrefix(path, prefix) {
			count++
		}
	}
	return count
}

func TestMALLinkQuietlyBaselinesTimestampTiesAndFavorites(t *testing.T) {
	s, queue, provider, api := providerFixture(t, false, ProviderOptions{PageSize: 1})
	api.entries["anime"] = []Entry{sampleEntry(42, 200, "anime"), sampleEntry(43, 200, "anime"), sampleEntry(44, 199, "anime")}
	api.entries["manga"] = []Entry{sampleEntry(45, 199, "manga")}
	api.profiles["Reader"] = fixtureProfile{id: 101, favorites: favoriteSection("anime", "Anime", "/anime/42", "Favorite", "TV&middot;2020")}
	linked, err := provider.Link(context.Background(), "1", "10", "Reader")
	if err != nil {
		t.Fatal(err)
	}
	state, err := queue.Snapshot(context.Background(), linked.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if !linked.Created || state.ExternalID != "101" || state.Features != 127 || state.Cursors[update.Anime].Value != 200000000 || len(state.Cursors[update.Anime].SourceIDs) != 2 || state.Cursors[update.Manga].Value != 200000000 || len(state.Favorites) != 1 {
		t.Fatalf("wrong quiet baseline: %+v", state)
	}
	if countRows(t, s, "events") != 0 {
		t.Fatal("linking replayed history")
	}
	if _, err := provider.Check(context.Background(), linked.AccountID); err != nil {
		t.Fatal(err)
	}
	if countRows(t, s, "events") != 0 {
		t.Fatal("first poll replayed linked history")
	}
}

func TestMALQuietLinkIncludesExistingFractionalSecondRevisions(t *testing.T) {
	s, _, provider, api := providerFixture(t, false, ProviderOptions{Now: func() time.Time {
		return time.Unix(200, 500000000).UTC()
	}})
	entry := sampleEntry(42, 200, "anime")
	entry.ListStatus.UpdatedAt = entry.ListStatus.UpdatedAt.Add(100 * time.Microsecond)
	api.entries["anime"] = []Entry{entry}
	linked, err := provider.Link(context.Background(), "1", "10", "Reader")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Check(context.Background(), linked.AccountID); err != nil {
		t.Fatal(err)
	}
	if countRows(t, s, "events") != 0 {
		t.Fatal("quiet linking replayed an update already present within the linking second")
	}
}

func TestExistingMALLinkNeverRefetchesOrResetsImportedState(t *testing.T) {
	_, queue, provider, api := providerFixture(t, true, ProviderOptions{})
	linked, err := provider.Link(context.Background(), "1", "10", "")
	if err != nil {
		t.Fatal(err)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if linked.Created || linked.AccountID != 1 || state.Cursors[update.Anime].Value != 100000000 || len(api.requests) != 0 {
		t.Fatal("existing account refetched or reset")
	}
}

func TestMALIndependentPagesConsumeTiesAndConsolidateRepeatedTitles(t *testing.T) {
	s, queue, provider, api := providerFixture(t, true, ProviderOptions{PageSize: 1})
	api.entries["anime"] = []Entry{sampleEntry(42, 101, "anime"), sampleEntry(42, 103, "anime"), sampleEntry(43, 103, "anime"), sampleEntry(99, 99, "anime")}
	api.entries["manga"] = []Entry{sampleEntry(44, 102, "manga"), sampleEntry(45, 102, "manga")}
	result, err := provider.Check(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 4 || result.Deliveries != 8 {
		t.Fatalf("wrong consolidated events: %+v", result)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Cursors[update.Anime].SourceIDs) != 2 || state.Cursors[update.Anime].Value != 103000000 || len(state.Cursors[update.Manga].SourceIDs) != 2 {
		t.Fatal("timestamp identities lost", state)
	}
	if _, err := provider.Check(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if countRows(t, s, "events") != 4 {
		t.Fatal("unchanged poll replayed list entries")
	}
}

func TestMALSameTimestampScoreChangesHaveStableRevisionIdentities(t *testing.T) {
	s, queue, provider, api := providerFixture(t, true, ProviderOptions{})
	api.entries["anime"] = []Entry{sampleEntry(42, 100, "anime")}
	if _, err := provider.Check(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	api.entries["anime"][0].ListStatus.Score = integer(9)
	if _, err := provider.Check(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	api.entries["anime"][0].Node.Title = "Renamed title"
	if _, err := provider.Check(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if countRows(t, s, "events") != 2 || len(state.Cursors[update.Anime].SourceIDs) != 2 {
		t.Fatal("score revision lost or display metadata caused replay")
	}
}

func TestMALProfileOutagePreservesFavoritesAndDoesNotStopListTracking(t *testing.T) {
	for _, status := range []int{403, 503} {
		s, queue, provider, api := providerFixture(t, true, ProviderOptions{})
		api.profileStatus["Reader"] = status
		api.entries["anime"] = []Entry{sampleEntry(42, 101, "anime")}
		if _, err := s.DB().Exec(`INSERT INTO favorites(account_id,kind,external_id,name) VALUES (1,'anime','77','Imported favorite')`); err != nil {
			t.Fatal(err)
		}
		result, err := provider.Check(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		state, err := queue.Snapshot(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if result.Events != 1 || result.ProfileFailures == 0 || len(state.Favorites) != 1 || state.Favorites[0].Subject.ID != "77" {
			t.Fatal("scraper outage erased favorites or blocked core", result, state)
		}
	}
}

func TestIncompleteFavoritePreviewIsNotASetOfRemovals(t *testing.T) {
	s, queue, provider, api := providerFixture(t, true, ProviderOptions{})
	api.profiles["Reader"] = fixtureProfile{id: 101, favorites: strings.Replace(favoriteSection("anime", "Anime", "/anime/42", "Title", "TV&middot;2020"), "Anime (1)", "Anime (10)", 1)}
	api.entries["anime"] = []Entry{sampleEntry(42, 101, "anime")}
	if _, err := s.DB().Exec(`INSERT INTO favorites(account_id,kind,external_id,name) VALUES (1,'anime','77','Imported favorite')`); err != nil {
		t.Fatal(err)
	}
	result, err := provider.Check(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 1 || result.FavoriteFailures == 0 || len(state.Favorites) != 1 || state.Favorites[0].Subject.ID != "77" {
		t.Fatal("partial favorites erased an imported baseline")
	}
	_, _, newProvider, newAPI := providerFixture(t, false, ProviderOptions{})
	newAPI.profiles["Reader"] = api.profiles["Reader"]
	if _, err := newProvider.Link(context.Background(), "1", "10", "Reader"); !errors.Is(err, ErrIncompleteFavorites) {
		t.Fatal("new link accepted an incomplete baseline", err)
	}
}

func TestMALFavoriteAddRemoveCycleAndDisabledFavorites(t *testing.T) {
	s, queue, provider, api := providerFixture(t, true, ProviderOptions{})
	api.profiles["Reader"] = fixtureProfile{id: 101, favorites: favoriteSection("anime", "Anime", "/anime/42", "Title", "TV&middot;2020")}
	for _, favorites := range []string{api.profiles["Reader"].favorites, "", api.profiles["Reader"].favorites} {
		api.profiles["Reader"] = fixtureProfile{id: 101, favorites: favorites}
		if _, err := provider.Check(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
	}
	if countRows(t, s, "events") != 3 {
		t.Fatal("favorite add/remove/add was deduplicated incorrectly")
	}
	if _, err := s.DB().Exec(`UPDATE accounts SET revision=revision+1,features=1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	api.profiles["Reader"] = fixtureProfile{id: 101}
	if _, err := provider.Check(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Favorites) != 1 || countRows(t, s, "events") != 3 {
		t.Fatal("disabled favorites changed the saved baseline")
	}
}

func TestMALPrivateListAndDeletedMediaDoNotLeakOrBlockOtherStreams(t *testing.T) {
	s, queue, provider, api := providerFixture(t, true, ProviderOptions{})
	api.listStatus["anime"] = 403
	api.failOffset["anime"] = -1
	deleted := sampleEntry(43, 102, "manga")
	deleted.Node.Title = ""
	deleted.ListStatus.Comments = "DO-NOT-POST"
	api.entries["manga"] = []Entry{sampleEntry(42, 101, "manga"), deleted}
	result, err := provider.Check(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 1 || result.PrivateLists != 1 || state.Cursors[update.Anime].Value != 100000000 || state.Cursors[update.Manga].Value != 102000000 || countRows(t, s, "events") != 1 {
		t.Fatal("private/deleted list handling lost data", result, state)
	}
}

func TestMALRenameRecoveryVerifiesTheSavedIDBeforePolling(t *testing.T) {
	for _, reused := range []bool{false, true} {
		s, queue, provider, api := providerFixture(t, true, ProviderOptions{})
		api.recovered = "Renamed"
		api.profiles["Renamed"] = fixtureProfile{id: 101}
		api.entries["anime"] = []Entry{sampleEntry(42, 101, "anime")}
		if reused {
			api.profiles["Reader"] = fixtureProfile{id: 999}
		} else {
			api.profileStatus["Reader"] = 404
		}
		result, err := provider.Check(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		state, err := queue.Snapshot(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Renamed || state.Username != "Renamed" || result.Events != 1 || requestCount(api, "/v2/users/Reader/") != 0 || countRows(t, s, "events") != 1 {
			t.Fatal("rename was not verified and committed atomically", result, state)
		}
	}
}

func TestUnrecoverableOrReusedMALIdentityDoesNotPostAnotherUsersList(t *testing.T) {
	for _, recoveredID := range []int64{0, 999} {
		s, queue, provider, api := providerFixture(t, true, ProviderOptions{})
		api.profiles["Reader"] = fixtureProfile{id: 999}
		api.entries["anime"] = []Entry{sampleEntry(42, 101, "anime")}
		if recoveredID != 0 {
			api.recovered = "Renamed"
			api.profiles["Renamed"] = fixtureProfile{id: recoveredID}
		}
		if _, err := provider.Check(context.Background(), 1); !errors.Is(err, ErrRenameRecovery) {
			t.Fatal("wrong identity accepted", err)
		}
		state, err := queue.Snapshot(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if state.Username != "Reader" || state.Revision != 0 || requestCount(api, "/v2/") != 0 || countRows(t, s, "events") != 0 {
			t.Fatal("failed recovery changed state")
		}
	}
}

func TestMALRequiredFailureBudgetAndStaleSnapshotAreAtomic(t *testing.T) {
	for _, options := range []ProviderOptions{{MaxRequests: 1}, {MaxRecords: 1}, {MaxBytes: 10}} {
		s, queue, provider, api := providerFixture(t, true, options)
		api.entries["anime"] = []Entry{sampleEntry(42, 101, "anime"), sampleEntry(43, 102, "anime")}
		if _, err := provider.Check(context.Background(), 1); !errors.Is(err, ErrCollectionBudget) {
			t.Fatal("MAL collection budget ignored", err)
		}
		state, err := queue.Snapshot(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if state.Revision != 0 || countRows(t, s, "events") != 0 {
			t.Fatal("oversized collection partly committed")
		}
	}
	s, queue, provider, api := providerFixture(t, true, ProviderOptions{PageSize: 1})
	api.entries["anime"] = []Entry{sampleEntry(42, 101, "anime"), sampleEntry(43, 102, "anime")}
	api.listStatus["anime"] = 503
	api.failOffset["anime"] = 1
	if _, err := provider.Check(context.Background(), 1); err == nil {
		t.Fatal("required later page failure ignored")
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 0 || countRows(t, s, "events") != 0 {
		t.Fatal("partial pages advanced cursor")
	}
	api.listStatus["anime"] = 0
	api.onRequest = func(r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/profile/") {
			if _, err := s.DB().Exec(`UPDATE accounts SET revision=revision+1,features=0 WHERE id=1`); err != nil {
				t.Error(err)
			}
		}
	}
	if _, err := provider.Check(context.Background(), 1); !errors.Is(err, store.ErrStalePoll) {
		t.Fatal("stale snapshot accepted", err)
	}
}

func TestMALOptionalEnrichmentFailureOrOversizeKeepsCoreEvents(t *testing.T) {
	for _, body := range []string{"", tenraiMedia("42", strings.Repeat("x", 70<<10))} {
		s, _, provider, api := providerFixture(t, true, ProviderOptions{})
		api.entries["anime"] = []Entry{sampleEntry(42, 101, "anime")}
		if _, err := s.DB().Exec(`UPDATE accounts SET features=features|16384 WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		enricher, _ := testEnricher(t, func(w http.ResponseWriter, r *http.Request) {
			if body == "" {
				w.WriteHeader(503)
			} else {
				fmt.Fprint(w, body)
			}
		}, EnricherOptions{})
		provider.options.Enricher = enricher
		result, err := provider.Check(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if result.Events != 1 || result.MetadataFailures == 0 {
			t.Fatal("optional failure lost core", result)
		}
	}
}

func TestMALBackpressureAndCancellationLeaveBaselinesUntouched(t *testing.T) {
	s, queue, provider, api := providerFixture(t, true, ProviderOptions{})
	api.entries["anime"] = []Entry{sampleEntry(42, 101, "anime")}
	limits := store.DefaultLimits()
	limits.MaxPending = 1
	limited, err := store.NewOutbox(s, limits)
	if err != nil {
		t.Fatal(err)
	}
	provider.queue = limited
	if _, err := provider.Check(context.Background(), 1); !errors.Is(err, store.ErrBackpressure) {
		t.Fatal("backpressure ignored", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api.onRequest = func(*http.Request) { cancel() }
	if _, err := provider.Check(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 0 || countRows(t, s, "events") != 0 {
		t.Fatal("failed poll committed")
	}
}

func TestConcurrentMALPollsRecordOneRevision(t *testing.T) {
	s, _, provider, api := providerFixture(t, true, ProviderOptions{})
	api.entries["anime"] = []Entry{sampleEntry(42, 101, "anime")}
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
	if countRows(t, s, "events") != 1 || countRows(t, s, "deliveries") != 2 {
		t.Fatal("concurrent polls replayed updates")
	}
}

func TestMALProviderDefaultsAndLimitsAreValidated(t *testing.T) {
	_, queue, provider, _ := providerFixture(t, true, ProviderOptions{})
	if _, err := NewProvider(nil, provider.client, ProviderOptions{}); err == nil {
		t.Fatal("missing outbox accepted")
	}
	for _, options := range []ProviderOptions{{PageSize: 1001}, {MaxRequests: -1}, {MaxBytes: -1}, {CheckTimeout: -time.Second}} {
		if _, err := NewProvider(queue, provider.client, options); err == nil {
			t.Fatal("invalid provider options accepted")
		}
	}
}

func TestMalformedMALProfileFallsBackWithoutRewritingFavorites(t *testing.T) {
	s, queue, provider, api := providerFixture(t, true, ProviderOptions{})
	api.profileBody = `<html><body>Verify you are human</body></html>`
	api.entries["anime"] = []Entry{sampleEntry(42, 101, "anime")}
	if _, err := s.DB().Exec(`INSERT INTO favorites(account_id,kind,external_id,name) VALUES (1,'anime','77','Imported')`); err != nil {
		t.Fatal(err)
	}
	result, err := provider.Check(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	state, err := queue.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 1 || result.ProfileFailures != 1 || len(state.Favorites) != 1 {
		t.Fatal("HTML layout/challenge failure damaged core tracking", result)
	}
}

func TestMALMetadataDeadlineFallsBackWithoutCancelingThePoll(t *testing.T) {
	s, _, provider, api := providerFixture(t, true, ProviderOptions{MetadataTimeout: 20 * time.Millisecond})
	api.entries["anime"] = []Entry{sampleEntry(42, 101, "anime")}
	if _, err := s.DB().Exec(`UPDATE accounts SET features=features|16384 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer close(release)
	enricher, _ := testEnricher(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}, EnricherOptions{})
	provider.options.Enricher = enricher
	result, err := provider.Check(context.Background(), 1)
	if err != nil || result.Events != 1 || result.MetadataFailures == 0 {
		t.Fatal("optional deadline canceled core recording", result, err)
	}
}

func TestMALMetadataCannotConsumeReservedCorePayloadCapacity(t *testing.T) {
	s, _, provider, api := providerFixture(t, true, ProviderOptions{})
	entry := sampleEntry(42, 101, "anime")
	api.entries["anime"] = []Entry{entry}
	if _, err := s.DB().Exec(`UPDATE accounts SET features=features|16384 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	profile, err := parseProfile([]byte(profileHTML(101, "Reader", "")), true)
	if err != nil {
		t.Fatal(err)
	}
	state := sampleState()
	state.Features |= int64(Themes)
	base, err := listPayload("anime", entry, profile, state, Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	capacity := len(base) + 100
	for _, value := range []any{profile, entry, []store.Favorite{}} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		capacity += len(encoded)
	}
	provider.options.MaxBytes = capacity
	enricher, _ := testEnricher(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Replace(tenraiMedia("42", "Synopsis"), `"Theme"`, strconv.Quote(strings.Repeat("T", 300)), 1))
	}, EnricherOptions{})
	provider.options.Enricher = enricher
	result, err := provider.Check(context.Background(), 1)
	if err != nil || result.Events != 1 || result.MetadataFailures == 0 {
		t.Fatal("optional display fields consumed core notification capacity", result, err)
	}
	var payload string
	if err := s.DB().QueryRow(`SELECT payload FROM events`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var message notification.Message
	if err := json.Unmarshal([]byte(payload), &message); err != nil {
		t.Fatal(err)
	}
	if field(message.Embeds[0], "Themes") != "" {
		t.Fatal("oversized optional enrichment did not fall back to its reserved core payload")
	}
}
