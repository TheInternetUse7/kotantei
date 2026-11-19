package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diamondburned/arikawa/v3/discord"

	"kotantei/internal/anilist"
	"kotantei/internal/buildinfo"
	"kotantei/internal/config"
	"kotantei/internal/mal"
	"kotantei/internal/notification"
	"kotantei/internal/scheduler"
	"kotantei/internal/store"
	"kotantei/internal/update"
)

// The real REST transport/providers/storage remain in runtime tests. Only the
// gateway event source is scripted; websocket/resume behavior has its own tests.
type scriptedDiscord struct {
	*Arikawa
	connect func(context.Context, Identity, func(*discord.InteractionEvent), func(), GatewayOptions) error
	send    func(context.Context, store.Delivery) (string, error)
	closed  func()
}

func (d *scriptedDiscord) Connect(ctx context.Context, id Identity, dispatch func(*discord.InteractionEvent), ready func(), options GatewayOptions) error {
	return d.connect(ctx, id, dispatch, ready, options)
}
func (d *scriptedDiscord) Close() {
	if d.closed != nil {
		d.closed()
	}
	d.Arikawa.Close()
}
func (d *scriptedDiscord) Send(ctx context.Context, item store.Delivery) (string, error) {
	if d.send != nil {
		return d.send(ctx, item)
	}
	return d.Arikawa.Send(ctx, item)
}

func runtimeConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Discord.Token = "test-token"
	cfg.MAL.ClientID = "test-client"
	cfg.MAL.CycleDelay = config.Duration(time.Hour)
	cfg.AniList.CycleDelay = config.Duration(time.Hour)
	return cfg
}
func waitRuntime(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal(what)
	}
}
func runtimeREST(t *testing.T, w http.ResponseWriter, r *http.Request) bool {
	t.Helper()
	switch {
	case strings.HasSuffix(r.URL.Path, "/users/@me"):
		fmt.Fprint(w, `{"id":"100","bot":true}`)
	case strings.Contains(r.URL.Path, "/oauth2/applications/@me"):
		fmt.Fprint(w, `{"id":"200","owner":{"id":"1"}}`)
	case strings.HasSuffix(r.URL.Path, "/applications/200/commands"):
		if r.Method != "PUT" {
			t.Error("runtime did not register global commands")
		}
		fmt.Fprint(w, `[]`)
	case strings.Contains(r.URL.Path, "/members/1"):
		fmt.Fprint(w, `{"user":{"id":"1"},"roles":[]}`)
	default:
		return false
	}
	return true
}
func publicRuntimeProfile(w http.ResponseWriter) {
	fmt.Fprint(w, `<html><head><meta property="og:url" content="https://myanimelist.net/profile/Reader"></head><body><div id="contentWrapper"><h1><a href="/modules.php?go=report&amp;type=profile&amp;id=201">Report</a></h1></div></body></html>`)
}
func runtimeProviderOptions(server *httptest.Server) (anilist.ClientOptions, mal.ClientOptions) {
	now := func() time.Time { return time.Unix(200, 0).UTC() }
	wait := func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return anilist.ClientOptions{Endpoint: server.URL + "/graphql", HTTPClient: server.Client(), Now: now, Wait: wait}, mal.ClientOptions{APIEndpoint: server.URL + "/v2", ProfileEndpoint: server.URL, HTTPClient: server.Client(), Now: now, Wait: wait}
}

func TestRuntimeStartsOneWorkerSetAfterReadinessRegistrationAndInitialMembershipCheck(t *testing.T) {
	s, q := botStore(t)
	if _, err := s.DB().Exec(`UPDATE accounts SET features=0`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connected, allowReady := make(chan struct{}), make(chan struct{})
	membersStarted, allowMembers := make(chan struct{}), make(chan struct{})
	providerCalls := make(chan string, 4)
	var membershipOnce sync.Once
	var readyCount, registered atomic.Int64
	var checkedAll, closed atomic.Bool
	network := &scriptedDiscord{Arikawa: transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/commands") {
			if readyCount.Load() == 0 {
				t.Error("registration preceded gateway readiness")
			}
			registered.Add(1)
		}
		if strings.Contains(r.URL.Path, "/members/") {
			if registered.Load() != 1 {
				t.Error("membership reconciliation preceded command registration")
			}
			membershipOnce.Do(func() {
				close(membersStarted)
				select {
				case <-allowMembers:
				case <-r.Context().Done():
				}
			})
			if strings.Contains(r.URL.Path, "/guilds/20/") {
				checkedAll.Store(true)
			}
		}
		if !runtimeREST(t, w, r) {
			t.Error("unexpected runtime Discord request", r.URL.Path)
		}
	}, HTTPOptions{})}
	network.connect = func(ctx context.Context, id Identity, dispatch func(*discord.InteractionEvent), ready func(), _ GatewayOptions) error {
		if id.UserID != "100" || id.ApplicationID != "200" {
			t.Error("gateway used an unverified identity")
		}
		close(connected)
		select {
		case <-allowReady:
		case <-ctx.Done():
			return ctx.Err()
		}
		for range 3 {
			readyCount.Add(1)
			ready()
		}
		<-ctx.Done()
		return ctx.Err()
	}
	network.closed = func() { closed.Store(true) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !checkedAll.Load() {
			t.Error("provider request preceded initial membership reconciliation")
		}
		switch r.URL.Path {
		case "/graphql":
			providerCalls <- "anilist"
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"User": anilist.User{ID: 101, Name: "AniReader", MediaListOptions: anilist.ListOptions{ScoreFormat: "POINT_10"}}}})
		case "/profile/Reader":
			providerCalls <- "mal"
			publicRuntimeProfile(w)
		default:
			t.Error("unexpected provider request", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	al, malOptions := runtimeProviderOptions(server)
	options := RuntimeOptions{Discord: network, AniList: al, MAL: malOptions, Reconcile: scheduler.ReconcileOptions{RequestInterval: time.Nanosecond}}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, q, runtimeConfig(t), options) }()
	waitRuntime(t, connected, "runtime did not connect after identity verification")
	select {
	case <-providerCalls:
		t.Fatal("provider started before readiness")
	default:
	}
	close(allowReady)
	waitRuntime(t, membersStarted, "runtime did not perform initial membership reconciliation")
	select {
	case <-providerCalls:
		t.Fatal("provider started while initial reconciliation was incomplete")
	default:
	}
	close(allowMembers)
	seen := map[string]int{}
	for range 2 {
		select {
		case provider := <-providerCalls:
			seen[provider]++
		case <-time.After(10 * time.Second):
			t.Fatal("provider workers were not started")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runtime failed to join workers")
	}
	if seen["anilist"] != 1 || seen["mal"] != 1 || registered.Load() != 1 || !closed.Load() {
		t.Fatal("repeated readiness duplicated workers or leaked transport", seen, registered.Load(), closed.Load())
	}
}

func TestRuntimeStartupFailureDoesNotStartProvidersOrExposeUpstreamErrors(t *testing.T) {
	for _, stage := range []string{"identity", "registration", "gateway"} {
		t.Run(stage, func(t *testing.T) {
			_, q := botStore(t)
			var logs bytes.Buffer
			var providerCalls, memberCalls atomic.Int64
			network := &scriptedDiscord{Arikawa: transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if (stage == "identity" && strings.HasSuffix(r.URL.Path, "/users/@me")) || (stage == "registration" && strings.HasSuffix(r.URL.Path, "/commands")) {
					w.WriteHeader(503)
					fmt.Fprint(w, `{"message":"DO-NOT-PRINT-UPSTREAM"}`)
					return
				}
				if strings.Contains(r.URL.Path, "/members/") {
					memberCalls.Add(1)
				}
				runtimeREST(t, w, r)
			}, HTTPOptions{})}
			network.connect = func(ctx context.Context, _ Identity, _ func(*discord.InteractionEvent), ready func(), _ GatewayOptions) error {
				if stage == "identity" {
					t.Error("gateway started after rejected identity")
				}
				if stage == "gateway" {
					return errors.New("DO-NOT-PRINT-UPSTREAM")
				}
				ready()
				<-ctx.Done()
				return ctx.Err()
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { providerCalls.Add(1) }))
			t.Cleanup(server.Close)
			al, malOptions := runtimeProviderOptions(server)
			err := Run(context.Background(), q, runtimeConfig(t), RuntimeOptions{Discord: network, AniList: al, MAL: malOptions, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
			if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrStarting) || providerCalls.Load() != 0 || memberCalls.Load() != 0 || strings.Contains(err.Error()+logs.String(), "DO-NOT-PRINT") {
				t.Fatal("failed startup ran work, hid its cause, or leaked upstream text", err, logs.String())
			}
		})
	}
}

func TestRuntimeReadinessTimeoutCancelsAndJoinsTheGateway(t *testing.T) {
	_, q := botStore(t)
	var joined atomic.Bool
	network := &scriptedDiscord{Arikawa: transportFixture(t, func(w http.ResponseWriter, r *http.Request) { runtimeREST(t, w, r) }, HTTPOptions{})}
	network.connect = func(ctx context.Context, _ Identity, _ func(*discord.InteractionEvent), _ func(), _ GatewayOptions) error {
		<-ctx.Done()
		joined.Store(true)
		return ctx.Err()
	}
	network.closed = func() {
		if !joined.Load() {
			t.Error("transport closed before gateway cleanup completed")
		}
	}
	cfg := runtimeConfig(t)
	cfg.MAL.Enabled = false
	err := Run(context.Background(), q, cfg, RuntimeOptions{Discord: network, StartupTimeout: 20 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) || !joined.Load() {
		t.Fatal("readiness timeout was lost or left gateway work alive", err)
	}
}

func TestRuntimeRecoversAnExpiredDeliveryLeaseAfterReady(t *testing.T) {
	s, q := botStore(t)
	if _, err := s.DB().Exec(`UPDATE accounts SET features=0`); err != nil {
		t.Fatal(err)
	}
	payload, err := notification.Payload(notification.Embed{Title: "Recovered", Color: 0}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Record(context.Background(), store.Poll{AccountID: 1, Now: time.Now().Add(-2 * time.Minute), Events: []update.Event{{Stream: update.Activity, Kind: update.List, SourceID: "1", Subject: update.Subject{Kind: update.AnimeSubject, ID: "42"}, OccurredAt: time.Unix(101, 0), Payload: payload}}}); err != nil {
		t.Fatal(err)
	}
	if lease, err := q.Claim(context.Background(), time.Now().Add(-time.Minute), 30*time.Second); err != nil || lease == nil {
		t.Fatal("could not stage an expired lease", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var posts atomic.Int64
	network := &scriptedDiscord{Arikawa: transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.Contains(r.URL.Path, "/channels/") {
			posts.Add(1)
			fmt.Fprint(w, `{"id":"1001"}`)
			return
		}
		if !runtimeREST(t, w, r) {
			t.Error("unexpected recovery request", r.URL.Path)
		}
	}, HTTPOptions{})}
	network.connect = func(ctx context.Context, _ Identity, _ func(*discord.InteractionEvent), ready func(), _ GatewayOptions) error {
		ready()
		<-ctx.Done()
		return ctx.Err()
	}
	network.send = func(ctx context.Context, item store.Delivery) (string, error) {
		id, err := network.Arikawa.Send(ctx, item)
		if err == nil && posts.Load() == 2 {
			cancel()
		}
		return id, err
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"User": anilist.User{ID: 101, Name: "AniReader", MediaListOptions: anilist.ListOptions{ScoreFormat: "POINT_10"}}}})
	}))
	t.Cleanup(server.Close)
	al, _ := runtimeProviderOptions(server)
	cfg := runtimeConfig(t)
	cfg.MAL.Enabled = false
	err = Run(ctx, q, cfg, RuntimeOptions{Discord: network, AniList: al, Reconcile: scheduler.ReconcileOptions{RequestInterval: time.Nanosecond}})
	if !errors.Is(err, context.Canceled) || posts.Load() != 2 {
		t.Fatal("runtime did not recover both channel deliveries", err, posts.Load())
	}
	status, err := q.Status(context.Background())
	if err != nil || status.Pending != 0 || status.Sent != 2 || status.Leased != 0 {
		t.Fatal("shutdown lost delivery receipts", status, err)
	}
}

func TestRuntimeOwnerRestartRepliesBeforeJoiningAndExitsCleanly(t *testing.T) {
	s, q := botStore(t)
	if _, err := s.DB().Exec(`UPDATE accounts SET features=0`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	polled := make(chan struct{})
	var once sync.Once
	var edited, joined atomic.Bool
	network := &scriptedDiscord{Arikawa: transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/callback"):
			w.WriteHeader(204)
		case strings.Contains(r.URL.Path, "/messages/@original"):
			edited.Store(true)
			fmt.Fprint(w, `{"id":"1001"}`)
		default:
			runtimeREST(t, w, r)
		}
	}, HTTPOptions{})}
	network.connect = func(ctx context.Context, _ Identity, dispatch func(*discord.InteractionEvent), ready func(), _ GatewayOptions) error {
		ready()
		select {
		case <-polled:
		case <-ctx.Done():
			return ctx.Err()
		}
		dispatch(commandEvent(t, 1, "admin restart", map[string]any{"confirm": true}))
		<-ctx.Done()
		joined.Store(true)
		return ctx.Err()
	}
	network.closed = func() {
		if !edited.Load() || !joined.Load() {
			t.Error("restart closed resources before reply/gateway cleanup")
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"User": anilist.User{ID: 101, Name: "AniReader", MediaListOptions: anilist.ListOptions{ScoreFormat: "POINT_10"}}}})
		once.Do(func() { close(polled) })
	}))
	t.Cleanup(server.Close)
	al, _ := runtimeProviderOptions(server)
	cfg := runtimeConfig(t)
	cfg.MAL.Enabled = false
	err := Run(ctx, q, cfg, RuntimeOptions{Discord: network, AniList: al, Reconcile: scheduler.ReconcileOptions{RequestInterval: time.Nanosecond}})
	if err != nil || !edited.Load() || !joined.Load() || ctx.Err() != nil {
		t.Fatal("owner restart was not an orderly clean exit", err)
	}
}

func TestRuntimeTenraiOptInAndOutageDoNotChangeCoreDelivery(t *testing.T) {
	for _, mode := range []string{"disabled", "enabled", "outage"} {
		t.Run(mode, func(t *testing.T) {
			s, q := botStore(t)
			if _, err := s.DB().Exec(`UPDATE accounts SET features=16385 WHERE provider='mal'`); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var posts, enrichment atomic.Int64
			var hadTheme atomic.Bool
			network := &scriptedDiscord{Arikawa: transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" && strings.Contains(r.URL.Path, "/channels/") {
					posts.Add(1)
					var message notification.Message
					if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
						t.Error(err)
					}
					for _, field := range message.Embeds[0].Fields {
						if strings.Contains(field.Value, "Adventure") {
							hadTheme.Store(true)
						}
					}
					fmt.Fprint(w, `{"id":"1001"}`)
					return
				}
				runtimeREST(t, w, r)
			}, HTTPOptions{})}
			network.connect = func(ctx context.Context, _ Identity, _ func(*discord.InteractionEvent), ready func(), _ GatewayOptions) error {
				ready()
				<-ctx.Done()
				return ctx.Err()
			}
			network.send = func(ctx context.Context, item store.Delivery) (string, error) {
				id, err := network.Arikawa.Send(ctx, item)
				if err == nil && posts.Load() == 2 {
					cancel()
				}
				return id, err
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/profile/Reader":
					publicRuntimeProfile(w)
				case "/v2/users/Reader/animelist":
					if r.Header.Get("X-MAL-CLIENT-ID") != "test-client" {
						t.Error("runtime lost configured MAL client ID")
					}
					score, progress := 7, 3
					json.NewEncoder(w).Encode(map[string]any{"data": []mal.Entry{{Node: &mal.Media{ID: 42, Title: "Title", Format: "tv", Episodes: 12}, ListStatus: &mal.ListStatus{Status: "watching", Score: &score, EpisodesWatched: &progress, UpdatedAt: time.Unix(101, 0).UTC()}}}, "paging": map[string]any{}})
				case "/tenrai/anime/42":
					enrichment.Add(1)
					if r.Header.Get("X-MAL-CLIENT-ID") != "" || r.Header.Get("Authorization") != "" {
						t.Error("enrichment received provider/Discord credentials")
					}
					if mode == "outage" {
						w.WriteHeader(503)
						fmt.Fprint(w, `{"message":"DO-NOT-PRINT"}`)
					} else {
						fmt.Fprint(w, `{"data":{"mal_id":42,"title":"Title","type":"TV","themes":[{"mal_id":1,"name":"Adventure"}]}}`)
					}
				default:
					t.Error("unexpected MAL/enrichment request", r.URL.Path)
				}
			}))
			t.Cleanup(server.Close)
			_, malOptions := runtimeProviderOptions(server)
			cfg := runtimeConfig(t)
			cfg.AniList.Enabled = false
			cfg.MAL.TenraiEnabled = mode != "disabled"
			options := RuntimeOptions{Discord: network, MAL: malOptions, AniList: anilist.ClientOptions{Endpoint: "://invalid-disabled-endpoint"}, Enrichment: mal.EnricherOptions{Endpoint: server.URL + "/tenrai", HTTPClient: server.Client()}, Reconcile: scheduler.ReconcileOptions{RequestInterval: time.Nanosecond}}
			err := Run(ctx, q, cfg, options)
			status, statusErr := q.Status(context.Background())
			if !errors.Is(err, context.Canceled) || statusErr != nil || status.Sent != 2 || posts.Load() != 2 || hadTheme.Load() != (mode == "enabled") {
				t.Fatal("optional enrichment changed core delivery", err, status, hadTheme.Load())
			}
			if (mode == "disabled" && enrichment.Load() != 0) || (mode != "disabled" && enrichment.Load() != 1) {
				t.Fatal("enrichment opt-in was not respected", enrichment.Load())
			}
		})
	}
}

func TestRuntimeSearchSharesTheEnabledProvidersTrackingClients(t *testing.T) {
	for _, provider := range []string{"anilist", "mal"} {
		t.Run(provider, func(t *testing.T) {
			s, q := botStore(t)
			if _, err := s.DB().Exec(`UPDATE accounts SET features=0`); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			polled := make(chan struct{})
			var once sync.Once
			var waits, searches atomic.Int64
			network := &scriptedDiscord{Arikawa: transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/callback"):
					w.WriteHeader(204)
				case strings.Contains(r.URL.Path, "/messages/@original"):
					var message notification.Message
					if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
						t.Error(err)
					}
					if len(message.Embeds) != 1 || len(message.Embeds[0].Fields) != 1 || !strings.Contains(message.Embeds[0].Fields[0].Value, "/anime/42") {
						t.Error("runtime search adapter lost its catalogue result")
					}
					fmt.Fprint(w, `{"id":"1001"}`)
				default:
					runtimeREST(t, w, r)
				}
			}, HTTPOptions{})}
			network.connect = func(ctx context.Context, _ Identity, dispatch func(*discord.InteractionEvent), ready func(), _ GatewayOptions) error {
				ready()
				select {
				case <-polled:
				case <-ctx.Done():
					return ctx.Err()
				}
				values := map[string]any{"title": "Title"}
				if provider == "mal" {
					values = map[string]any{"query": "Title"}
				}
				dispatch(commandEvent(t, 1, provider+" search anime", values))
				<-ctx.Done()
				return ctx.Err()
			}
			// Cancel only after the real transport receives the final response.
			networkEdit := &cancelingRuntimeDiscord{scriptedDiscord: network, cancel: cancel}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/graphql" {
					var input struct {
						OperationName string `json:"operationName"`
					}
					json.NewDecoder(r.Body).Decode(&input)
					if input.OperationName == "KotanteiSearch" {
						searches.Add(1)
						fmt.Fprint(w, `{"data":{"Page":{"pageInfo":{"currentPage":1,"hasNextPage":false},"items":[{"id":42,"type":"ANIME","title":{"romaji":"Title"},"format":"TV"}]}}}`)
					} else {
						json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"User": anilist.User{ID: 101, Name: "AniReader", MediaListOptions: anilist.ListOptions{ScoreFormat: "POINT_10"}}}})
						once.Do(func() { close(polled) })
					}
				} else if r.URL.Path == "/v2/anime" {
					searches.Add(1)
					fmt.Fprint(w, `{"data":[{"node":{"id":42,"title":"Title","media_type":"tv"}}],"paging":{}}`)
				} else {
					publicRuntimeProfile(w)
					once.Do(func() { close(polled) })
				}
			}))
			t.Cleanup(server.Close)
			al, malOptions := runtimeProviderOptions(server)
			al.Wait = func(ctx context.Context, _ time.Duration) error { waits.Add(1); return ctx.Err() }
			malOptions.Wait = al.Wait
			cfg := runtimeConfig(t)
			cfg.MAL.Enabled = provider == "mal"
			cfg.AniList.Enabled = provider == "anilist"
			err := Run(ctx, q, cfg, RuntimeOptions{Discord: networkEdit, AniList: al, MAL: malOptions, Reconcile: scheduler.ReconcileOptions{RequestInterval: time.Nanosecond}})
			if !errors.Is(err, context.Canceled) || searches.Load() != 1 || waits.Load() == 0 {
				t.Fatal("runtime search bypassed tracking's shared client/pacing", err, searches.Load(), waits.Load())
			}
		})
	}
}

type cancelingRuntimeDiscord struct {
	*scriptedDiscord
	cancel context.CancelFunc
}

func TestRuntimeAboutUsesTheOperatorsSourceOfferInsteadOfTheCompiledDefault(t *testing.T) {
	s, q := botStore(t)
	if _, err := s.DB().Exec(`UPDATE accounts SET features=0`); err != nil {
		t.Fatal(err)
	}
	compiled := buildinfo.SourceURL
	t.Cleanup(func() { buildinfo.SourceURL = compiled })
	buildinfo.SourceURL = "https://example.com/upstream-source.tar.gz"
	want := "https://example.com/operator-modified-source.tar.gz"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	polled := make(chan struct{})
	var once sync.Once
	var replied atomic.Bool
	network := &scriptedDiscord{Arikawa: transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/callback"):
			w.WriteHeader(204)
		case strings.Contains(r.URL.Path, "/messages/@original"):
			var message notification.Message
			if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
				t.Error(err)
			}
			if len(message.Embeds) != 1 || message.Embeds[0].URL != want {
				t.Error("runtime lost the operator's matching-source override", message)
			}
			replied.Store(true)
			fmt.Fprint(w, `{"id":"1001"}`)
		default:
			runtimeREST(t, w, r)
		}
	}, HTTPOptions{})}
	network.connect = func(ctx context.Context, _ Identity, dispatch func(*discord.InteractionEvent), ready func(), _ GatewayOptions) error {
		ready()
		select {
		case <-polled:
		case <-ctx.Done():
			return ctx.Err()
		}
		dispatch(commandEvent(t, 1, "about", nil))
		<-ctx.Done()
		return ctx.Err()
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"User": anilist.User{ID: 101, Name: "AniReader", MediaListOptions: anilist.ListOptions{ScoreFormat: "POINT_10"}}}})
		once.Do(func() { close(polled) })
	}))
	t.Cleanup(server.Close)
	al, _ := runtimeProviderOptions(server)
	cfg := runtimeConfig(t)
	cfg.MAL.Enabled = false
	cfg.Project.SourceURL = want
	err := Run(ctx, q, cfg, RuntimeOptions{Discord: &cancelingRuntimeDiscord{scriptedDiscord: network, cancel: cancel}, AniList: al, Reconcile: scheduler.ReconcileOptions{RequestInterval: time.Nanosecond}})
	if !errors.Is(err, context.Canceled) || !replied.Load() {
		t.Fatal("about did not complete through the running command service", err)
	}
}

func (d *cancelingRuntimeDiscord) Edit(ctx context.Context, event *discord.InteractionEvent, raw json.RawMessage) error {
	err := d.Arikawa.Edit(ctx, event, raw)
	if err == nil {
		d.cancel()
	}
	return err
}
