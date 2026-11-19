package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diamondburned/arikawa/v3/api"
	"github.com/diamondburned/arikawa/v3/discord"
)

func commandEvent(t *testing.T, id uint64, path string, values map[string]any) *discord.InteractionEvent {
	t.Helper()
	parts := strings.Fields(path)
	options := []map[string]any{}
	for key, value := range values {
		kind := 3
		switch value.(type) {
		case bool:
			kind = 5
		case int:
			kind = 4
		}
		if key == "channel" {
			kind = 7
		}
		if key == "user" {
			kind = 6
		}
		options = append(options, map[string]any{"name": key, "type": kind, "value": value})
	}
	if len(parts) == 3 {
		options = []map[string]any{{"name": parts[1], "type": 2, "options": []map[string]any{{"name": parts[2], "type": 1, "options": options}}}}
	} else if len(parts) == 2 {
		options = []map[string]any{{"name": parts[1], "type": 1, "options": options}}
	}
	encoded, err := json.Marshal(map[string]any{"id": fmt.Sprint(id), "application_id": "200", "token": "interaction-token", "type": 2, "version": 1, "guild_id": "10", "channel_id": "11", "member": map[string]any{"user": map[string]any{"id": "1", "username": "Actor"}, "roles": []string{}, "permissions": "0"}, "data": map[string]any{"id": "300", "name": parts[0], "type": 1, "options": options}})
	if err != nil {
		t.Fatal(err)
	}
	var event discord.InteractionEvent
	if err := json.Unmarshal(encoded, &event); err != nil {
		t.Fatal(err)
	}
	return &event
}

func TestHandlerDefersEphemerallyBeforeAnySettingsMutation(t *testing.T) {
	_, q := botStore(t)
	var callbacks, edits atomic.Int64
	network := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/callback") {
			callbacks.Add(1)
			var callback api.InteractionResponse
			if err := json.NewDecoder(r.Body).Decode(&callback); err != nil {
				t.Error(err)
			}
			state, err := q.Settings(context.Background(), "1", "anilist")
			if err != nil || state.Features != 127 {
				t.Error("work ran before acknowledgement")
			}
			if callback.Type != api.DeferredMessageInteractionWithSource || callback.Data.Flags != discord.EphemeralMessage {
				t.Error("reply was not deferred and private")
			}
			w.WriteHeader(204)
		} else {
			edits.Add(1)
			fmt.Fprint(w, `{"id":"1001"}`)
		}
	}, HTTPOptions{})
	service, err := NewService(q, network, ServiceOptions{Identity: Identity{ApplicationID: "200", UserID: "100", OwnerIDs: []string{"1"}}})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(service, network, HandlerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	handler.SetReady(true)
	if err := handler.Handle(context.Background(), commandEvent(t, 1, "anilist features enable", map[string]any{"feature": "4096"})); err != nil {
		t.Fatal(err)
	}
	state, err := q.Settings(context.Background(), "1", "anilist")
	if err != nil || state.Features != 4223 || callbacks.Load() != 1 || edits.Load() != 1 {
		t.Fatal("deferred command did not commit/reply", state, err)
	}
}

func TestHandlerAckFailureNeverRunsWorkOrLogsTokens(t *testing.T) {
	_, q := botStore(t)
	var logs bytes.Buffer
	network := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		fmt.Fprint(w, `{"message":"DO-NOT-LOG-TOKEN"}`)
	}, HTTPOptions{})
	service, _ := NewService(q, network, ServiceOptions{Identity: Identity{ApplicationID: "200", UserID: "100", OwnerIDs: []string{"1"}}})
	handler, err := NewHandler(service, network, HandlerOptions{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	handler.SetReady(true)
	if err := handler.Handle(context.Background(), commandEvent(t, 1, "anilist user remove", map[string]any{"confirm": true})); err == nil {
		t.Fatal("ack failure reported success")
	}
	if _, err := q.Settings(context.Background(), "1", "anilist"); err != nil {
		t.Fatal("unacknowledged command mutated storage", err)
	}
	if strings.Contains(logs.String(), "DO-NOT-LOG") || strings.Contains(logs.String(), "interaction-token") {
		t.Fatal("interaction failure leaked a token")
	}
}

func TestHandlerRejectsDMAndWrongApplicationEventsWithoutMutation(t *testing.T) {
	_, q := botStore(t)
	var calls atomic.Int64
	network := transportFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }, HTTPOptions{})
	service, _ := NewService(q, network, ServiceOptions{Identity: Identity{ApplicationID: "200", UserID: "100", OwnerIDs: []string{"1"}}})
	handler, _ := NewHandler(service, network, HandlerOptions{})
	handler.SetReady(true)
	event := commandEvent(t, 1, "anilist user remove", map[string]any{"confirm": true})
	event.AppID = 999
	if err := handler.Handle(context.Background(), event); !errors.Is(err, ErrInvalidCommand) || calls.Load() != 0 {
		t.Fatal("another application's event was accepted", err)
	}
	event.AppID = 200
	event.GuildID = 0
	event.User = &discord.User{ID: 1}
	event.Member = nil
	if err := handler.Handle(context.Background(), event); !errors.Is(err, ErrInvalidCommand) {
		t.Fatal("DM mutation was accepted", err)
	}
	if _, err := q.Settings(context.Background(), "1", "anilist"); err != nil {
		t.Fatal("invalid event unlinked an account", err)
	}
}

func TestHandlerPermissionErrorsAndUnknownUpstreamFailuresUseSafePrivateReplies(t *testing.T) {
	_, q := botStore(t)
	var replies []string
	var mu sync.Mutex
	network := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/callback") {
			w.WriteHeader(204)
			return
		}
		var message map[string]any
		json.NewDecoder(r.Body).Decode(&message)
		encoded, _ := json.Marshal(message)
		mu.Lock()
		replies = append(replies, string(encoded))
		mu.Unlock()
		fmt.Fprint(w, `{"id":"1001"}`)
	}, HTTPOptions{})
	service, _ := NewService(q, network, ServiceOptions{Identity: Identity{ApplicationID: "200", UserID: "100", OwnerIDs: []string{"1"}}, Providers: map[string]ProviderCommands{"anilist": {Search: func(context.Context, string, string, string) ([]SearchResult, error) {
		return nil, errors.New("DO-NOT-PRINT-UPSTREAM-SECRET")
	}}}})
	handler, _ := NewHandler(service, network, HandlerOptions{})
	handler.SetReady(true)
	event := commandEvent(t, 1, "admin cleanup", map[string]any{"confirm": true})
	event.Member.User.ID = 2
	event.Member.RoleIDs = []discord.RoleID{99}
	if err := handler.Handle(context.Background(), event); !errors.Is(err, ErrForbidden) {
		t.Fatal("guild admin became bot owner", err)
	}
	if err := handler.Handle(context.Background(), commandEvent(t, 2, "anilist search anime", map[string]any{"title": "Title"})); err == nil {
		t.Fatal("provider failure was hidden")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(replies) != 2 || strings.Contains(strings.Join(replies, ""), "DO-NOT-PRINT") {
		t.Fatal("private error response leaked upstream data", replies)
	}
}

func TestHandlerRestartIsAuthorizedAndOnlyRunsAfterSuccessfulResponseEdit(t *testing.T) {
	for _, editFails := range []bool{false, true} {
		_, q := botStore(t)
		var edited, stopped atomic.Bool
		network := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/callback") {
				w.WriteHeader(204)
				return
			}
			edited.Store(true)
			if editFails {
				w.WriteHeader(503)
			} else {
				fmt.Fprint(w, `{"id":"1001"}`)
			}
		}, HTTPOptions{})
		service, _ := NewService(q, network, ServiceOptions{Identity: Identity{ApplicationID: "200", UserID: "100", OwnerIDs: []string{"1"}}, Shutdown: func() {
			if !edited.Load() {
				t.Error("shutdown ran before its reply")
			}
			stopped.Store(true)
		}})
		handler, _ := NewHandler(service, network, HandlerOptions{})
		handler.SetReady(true)
		err := handler.Handle(context.Background(), commandEvent(t, 1, "admin restart", map[string]any{"confirm": true}))
		if stopped.Load() == editFails || (editFails && err == nil) {
			t.Fatal("restart ignored response failure ordering", stopped.Load(), err)
		}
	}
}

func TestHandlerDuplicateInteractionsDoNotReplayMutations(t *testing.T) {
	_, q := botStore(t)
	var callbacks atomic.Int64
	network := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/callback") {
			callbacks.Add(1)
			w.WriteHeader(204)
		} else {
			fmt.Fprint(w, `{"id":"1001"}`)
		}
	}, HTTPOptions{})
	service, _ := NewService(q, network, ServiceOptions{Identity: Identity{ApplicationID: "200", UserID: "100", OwnerIDs: []string{"1"}}})
	handler, _ := NewHandler(service, network, HandlerOptions{})
	handler.SetReady(true)
	event := commandEvent(t, 1, "anilist features enable", map[string]any{"feature": "4096"})
	for range 2 {
		if err := handler.Handle(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	if callbacks.Load() != 1 {
		t.Fatal("duplicate interaction was acknowledged/run twice")
	}
}

func TestHandlerReadinessBackpressureAndShutdownKeepConcurrencyBounded(t *testing.T) {
	_, q := botStore(t)
	started := make(chan struct{})
	busy := make(chan struct{}, 2)
	var once sync.Once
	network := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/callback") {
			var callback api.InteractionResponse
			json.NewDecoder(r.Body).Decode(&callback)
			if callback.Type == api.MessageInteractionWithSource {
				busy <- struct{}{}
			}
			w.WriteHeader(204)
		} else {
			fmt.Fprint(w, `{"id":"1001"}`)
		}
	}, HTTPOptions{})
	service, _ := NewService(q, network, ServiceOptions{Identity: Identity{ApplicationID: "200", UserID: "100", OwnerIDs: []string{"1"}}, Providers: map[string]ProviderCommands{"anilist": {Search: func(ctx context.Context, kind, query, format string) ([]SearchResult, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return nil, ctx.Err()
	}}}})
	handler, _ := NewHandler(service, network, HandlerOptions{MaxInFlight: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := handler.Handle(ctx, commandEvent(t, 1, "anilist features enable", map[string]any{"feature": "4096"})); !errors.Is(err, ErrStarting) {
		t.Fatal("work ran before readiness", err)
	}
	select {
	case <-busy:
	case <-time.After(10 * time.Second):
		t.Fatal("startup interaction was not rejected privately")
	}
	handler.SetReady(true)
	if !handler.Dispatch(ctx, commandEvent(t, 2, "anilist search anime", map[string]any{"title": "Title"})) {
		t.Fatal("ready work was not admitted")
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatched work did not start")
	}
	if handler.Dispatch(ctx, commandEvent(t, 3, "anilist features enable", map[string]any{"feature": "4096"})) {
		t.Fatal("work exceeded the handler concurrency bound")
	}
	select {
	case <-busy:
	case <-time.After(10 * time.Second):
		t.Fatal("overloaded interaction was not rejected privately")
	}
	handler.Close()
	cancel()
	done := make(chan struct{})
	go func() { handler.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("command work did not shut down")
	}
	state, err := q.Settings(context.Background(), "1", "anilist")
	if err != nil || state.Features != 127 {
		t.Fatal("startup/busy command mutated storage", state, err)
	}
}

func TestHandlerCommandDeadlineRepliesWithoutLeavingSearchWorkAlive(t *testing.T) {
	_, q := botStore(t)
	var active atomic.Int64
	var started atomic.Bool
	var edited atomic.Bool
	network := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/callback") {
			w.WriteHeader(204)
		} else {
			edited.Store(true)
			fmt.Fprint(w, `{"id":"1001"}`)
		}
	}, HTTPOptions{})
	service, _ := NewService(q, network, ServiceOptions{Identity: Identity{ApplicationID: "200", UserID: "100", OwnerIDs: []string{"1"}}, Providers: map[string]ProviderCommands{"anilist": {Search: func(ctx context.Context, kind, query, format string) ([]SearchResult, error) {
		started.Store(true)
		active.Add(1)
		defer active.Add(-1)
		<-ctx.Done()
		return nil, ctx.Err()
	}}}})
	handler, _ := NewHandler(service, network, HandlerOptions{CommandTimeout: 200 * time.Millisecond})
	handler.SetReady(true)
	if err := handler.Handle(context.Background(), commandEvent(t, 1, "anilist search anime", map[string]any{"title": "Title"})); !errors.Is(err, context.DeadlineExceeded) || !started.Load() || active.Load() != 0 || !edited.Load() {
		t.Fatal("expired command retained work or lost its timeout reply", active.Load(), err)
	}
}

func TestHandlerOptionsRequireSafeDeadlinesAndDependencies(t *testing.T) {
	if _, err := NewHandler(nil, nil, HandlerOptions{}); err == nil {
		t.Fatal("handler accepted missing dependencies")
	}
	_, _, service := serviceFixture(t, ServiceOptions{})
	network := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {}, HTTPOptions{})
	for _, options := range []HandlerOptions{{AckTimeout: 3 * time.Second}, {MaxInFlight: 17}, {DedupeEntries: -1}, {CommandTimeout: 16 * time.Minute}} {
		if _, err := NewHandler(service, network, options); err == nil {
			t.Fatal("unsafe handler limits accepted")
		}
	}
}

func TestHandlerManagerAuthorizationUsesRESTRatherThanOmittedInteractionPermissions(t *testing.T) {
	_, q := botStore(t)
	var permissionReads atomic.Int64
	network := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/callback"):
			w.WriteHeader(204)
		case strings.Contains(r.URL.Path, "/messages/@original"):
			fmt.Fprint(w, `{"id":"1001"}`)
		case strings.Contains(r.URL.Path, "/channels/"):
			fmt.Fprint(w, `{"id":"12","guild_id":"10","type":0}`)
		case strings.HasSuffix(r.URL.Path, "/members/2"):
			permissionReads.Add(1)
			fmt.Fprint(w, `{"user":{"id":"2"},"roles":["99"]}`)
		case strings.Contains(r.URL.Path, "/members/"):
			fmt.Fprint(w, `{"user":{"id":"100"},"roles":[]}`)
		default:
			fmt.Fprint(w, `{"id":"10","owner_id":"1","roles":[{"id":"10","permissions":"19456"},{"id":"99","permissions":"32"}]}`)
		}
	}, HTTPOptions{})
	service, _ := NewService(q, network, ServiceOptions{Identity: Identity{ApplicationID: "200", UserID: "100", OwnerIDs: []string{"1"}}})
	handler, _ := NewHandler(service, network, HandlerOptions{})
	handler.SetReady(true)
	event := commandEvent(t, 1, "sm update", map[string]any{"channel": "12"})
	event.Member.User.ID = 2
	if err := handler.Handle(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	status, err := q.GuildStatus(context.Background(), "10")
	if err != nil || status.ChannelID != "12" || permissionReads.Load() != 1 {
		t.Fatal("manager command did not revalidate authority", status, err)
	}
}

type blockingLog struct {
	entered, release chan struct{}
	once             sync.Once
}

func (w *blockingLog) Write(raw []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(raw), nil
}

func TestHandlerWaitJoinsFailureLoggingBeforeReturning(t *testing.T) {
	_, q := botStore(t)
	network := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/callback") {
			w.WriteHeader(204)
		} else {
			fmt.Fprint(w, `{"id":"1001"}`)
		}
	}, HTTPOptions{})
	service, _ := NewService(q, network, ServiceOptions{Identity: Identity{ApplicationID: "200", UserID: "100", OwnerIDs: []string{"1"}}, Providers: map[string]ProviderCommands{"anilist": {Search: func(context.Context, string, string, string) ([]SearchResult, error) {
		return nil, ErrDiscordUnavailable
	}}}})
	log := &blockingLog{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(log.release)
	handler, _ := NewHandler(service, network, HandlerOptions{Logger: slog.New(slog.NewJSONHandler(log, nil))})
	handler.SetReady(true)
	if !handler.Dispatch(context.Background(), commandEvent(t, 1, "anilist search anime", map[string]any{"title": "Title"})) {
		t.Fatal("command was not admitted")
	}
	select {
	case <-log.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("failure logging did not start")
	}
	handler.Close()
	done := make(chan struct{})
	go func() { handler.Wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("handler wait returned while its logging was still active")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestHandlerDedupeCacheIsBoundedAndNeverEvictsActiveWork(t *testing.T) {
	_, q := botStore(t)
	started := make(chan struct{})
	var once sync.Once
	var callbacks atomic.Int64
	network := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/callback") {
			callbacks.Add(1)
			w.WriteHeader(204)
		} else {
			fmt.Fprint(w, `{"id":"1001"}`)
		}
	}, HTTPOptions{})
	service, _ := NewService(q, network, ServiceOptions{Identity: Identity{ApplicationID: "200", UserID: "100", OwnerIDs: []string{"1"}}, Providers: map[string]ProviderCommands{"anilist": {Search: func(ctx context.Context, _, _, _ string) ([]SearchResult, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return nil, ctx.Err()
	}}}})
	handler, _ := NewHandler(service, network, HandlerOptions{MaxInFlight: 2, DedupeEntries: 4})
	handler.SetReady(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { handler.Close(); cancel(); handler.Wait() }()
	active := commandEvent(t, 1, "anilist search anime", map[string]any{"title": "Title"})
	if !handler.Dispatch(ctx, active) {
		t.Fatal("active search was not admitted")
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("search did not start")
	}
	for id := uint64(2); id <= 8; id++ {
		if err := handler.Handle(ctx, commandEvent(t, id, "about", nil)); err != nil {
			t.Fatal(err)
		}
	}
	handler.mu.Lock()
	retained, exists := handler.seen[1]
	count := len(handler.seen)
	handler.mu.Unlock()
	if !exists || !retained.active || count != 4 {
		t.Fatal("active work was evicted or dedupe cache grew", exists, retained, count)
	}
	if handler.Dispatch(ctx, active) || callbacks.Load() != 8 {
		t.Fatal("active interaction was replayed")
	}
}

func TestHandlerPermissionLookupFailureNeverChangesRouting(t *testing.T) {
	_, q := botStore(t)
	network := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/callback"):
			w.WriteHeader(204)
		case strings.Contains(r.URL.Path, "/messages/@original"):
			fmt.Fprint(w, `{"id":"1001"}`)
		default:
			w.WriteHeader(503)
			fmt.Fprint(w, `{"message":"DO-NOT-PRINT"}`)
		}
	}, HTTPOptions{})
	service, _ := NewService(q, network, ServiceOptions{Identity: Identity{ApplicationID: "200", UserID: "100", OwnerIDs: []string{"1"}}})
	handler, _ := NewHandler(service, network, HandlerOptions{})
	handler.SetReady(true)
	event := commandEvent(t, 1, "sm update", map[string]any{"channel": "12"})
	event.Member.User.ID = 2
	if err := handler.Handle(context.Background(), event); err == nil {
		t.Fatal("failed permission lookup was allowed")
	}
	status, err := q.GuildStatus(context.Background(), "10")
	if err != nil || status.ChannelID != "11" {
		t.Fatal("unverified manager changed routing", status, err)
	}
}

func TestHandlerCommandBudgetStartsAfterSuccessfulAcknowledgment(t *testing.T) {
	_, q := botStore(t)
	var executed, edited atomic.Bool
	network := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/callback") {
			timer := time.NewTimer(350 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
				w.WriteHeader(204)
			case <-r.Context().Done():
			}
			return
		}
		edited.Store(true)
		fmt.Fprint(w, `{"id":"1001"}`)
	}, HTTPOptions{})
	service, _ := NewService(q, network, ServiceOptions{Identity: Identity{ApplicationID: "200", UserID: "100", OwnerIDs: []string{"1"}}, Providers: map[string]ProviderCommands{"anilist": {Search: func(context.Context, string, string, string) ([]SearchResult, error) {
		executed.Store(true)
		return nil, nil
	}}}})
	handler, _ := NewHandler(service, network, HandlerOptions{CommandTimeout: 200 * time.Millisecond})
	handler.SetReady(true)
	if err := handler.Handle(context.Background(), commandEvent(t, 1, "anilist search anime", map[string]any{"title": "Title"})); err != nil || !executed.Load() || !edited.Load() {
		t.Fatal("ACK time consumed the separate operation budget", err, executed.Load(), edited.Load())
	}
}
