package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diamondburned/arikawa/v3/discord"

	"kotantei/internal/mal"
	"kotantei/internal/notification"
	"kotantei/internal/scheduler"
	"kotantei/internal/store"
)

func botStore(t *testing.T) (*store.Store, *store.Outbox) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.DB().Exec(`INSERT INTO users(id) VALUES ('1'),('2');INSERT INTO guilds(id,channel_id) VALUES ('10','11'),('20','21');INSERT INTO guild_users(guild_id,user_id) VALUES ('10','1'),('20','1');
		INSERT INTO accounts(id,provider,external_id,user_id,username) VALUES (1,'anilist','101','1','AniReader'),(2,'mal','201','1','Reader');
		INSERT INTO checkpoints(account_id,kind,value) VALUES (1,'activity',100),(1,'review',100),(2,'anime',100000000),(2,'manga',100000000);`); err != nil {
		t.Fatal(err)
	}
	q, err := store.NewOutbox(s, store.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return s, q
}
func request(path string, values map[string]any) Request {
	result := Request{GuildID: "10", ChannelID: "11", UserID: "1", Path: strings.Fields(path), Values: map[string]json.RawMessage{}}
	for key, value := range values {
		encoded, _ := json.Marshal(value)
		result.Values[key] = encoded
	}
	return result
}
func serviceFixture(t *testing.T, options ServiceOptions) (*store.Store, *store.Outbox, *Service) {
	t.Helper()
	s, q := botStore(t)
	if options.Identity.ApplicationID == "" {
		options.Identity = Identity{ApplicationID: "200", UserID: "100", OwnerIDs: []string{"1"}}
	}
	network := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/channels/"):
			fmt.Fprint(w, `{"id":"12","guild_id":"10","type":0,"permission_overwrites":[]}`)
		case strings.Contains(r.URL.Path, "/members/"):
			fmt.Fprint(w, `{"user":{"id":"100","bot":true},"roles":[]}`)
		default:
			fmt.Fprint(w, `{"id":"10","owner_id":"1","roles":[{"id":"10","permissions":"19456"}]}`)
		}
	}, HTTPOptions{})
	service, err := NewService(q, network, options)
	if err != nil {
		t.Fatal(err)
	}
	return s, q, service
}
func replyEmbed(t *testing.T, result Result) notification.Embed {
	t.Helper()
	var message notification.Message
	if err := json.Unmarshal(result.Payload, &message); err != nil {
		t.Fatal(err)
	}
	if len(message.Embeds) != 1 || len(message.AllowedMentions.Parse) != 0 || len(message.AllowedMentions.Users) != 0 {
		t.Fatal("command reply allows mentions or lacks an embed")
	}
	return message.Embeds[0]
}

func TestAboutDisplaysLicenseBuildVersionAndSourceOfferWithoutMentions(t *testing.T) {
	_, _, service := serviceFixture(t, ServiceOptions{})
	result, err := service.Execute(context.Background(), request("about", nil))
	if err != nil {
		t.Fatal(err)
	}
	embed := replyEmbed(t, result)
	if !strings.Contains(embed.Description, "AGPL-3.0-only") || !strings.Contains(strings.ToLower(embed.Description), "no warranty") {
		t.Fatal("about omitted licensing notices", embed)
	}
	var version, source bool
	for _, field := range embed.Fields {
		if field.Name == "Version" && field.Value != "" {
			version = true
		}
		if field.Name == "Corresponding source" && field.Value != "" {
			source = true
		}
	}
	if !version || !source {
		t.Fatal("about omitted the running build identity or source offer", embed)
	}
}

func TestAboutPreservesTheCompleteConfiguredSourceDownloadURL(t *testing.T) {
	source := "https://example.com/source?download=" + strings.Repeat("x", 1500)
	_, _, service := serviceFixture(t, ServiceOptions{SourceURL: source})
	result, err := service.Execute(context.Background(), request("about", nil))
	if err != nil {
		t.Fatal(err)
	}
	embed := replyEmbed(t, result)
	if embed.URL != source {
		t.Fatal("source-download link was missing or clipped", embed.URL)
	}
}

func TestServiceRejectsUnsafeSourceOffersWithoutEchoingValues(t *testing.T) {
	_, q := botStore(t)
	network := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {}, HTTPOptions{})
	for _, source := range []string{"http://example.com/DO-NOT-PRINT", "https://name:DO-NOT-PRINT@example.com/source", "https://:443/DO-NOT-PRINT"} {
		_, err := NewService(q, network, ServiceOptions{Identity: Identity{ApplicationID: "200", UserID: "100"}, SourceURL: source})
		if err == nil || strings.Contains(err.Error(), "DO-NOT-PRINT") {
			t.Fatal("unsafe source offer accepted or echoed", err)
		}
	}
}

func TestServiceManagerAndOwnerPermissionsAreCheckedBeforeMutation(t *testing.T) {
	_, q, service := serviceFixture(t, ServiceOptions{})
	denied := request("sm update", map[string]any{"channel": "12"})
	denied.UserID = "2"
	if _, err := service.Execute(context.Background(), denied); !errors.Is(err, ErrForbidden) {
		t.Fatal("ordinary user changed server routing", err)
	}
	status, err := q.GuildStatus(context.Background(), "10")
	if err != nil || status.ChannelID != "11" {
		t.Fatal("forbidden command changed storage")
	}
	denied.Permissions = discord.PermissionManageGuild
	if _, err := service.Execute(context.Background(), denied); err != nil {
		t.Fatal("server manager was denied", err)
	}
	denied = request("admin cleanup", map[string]any{"confirm": true})
	denied.UserID = "2"
	denied.Permissions = discord.PermissionAdministrator
	if _, err := service.Execute(context.Background(), denied); !errors.Is(err, ErrForbidden) {
		t.Fatal("guild administrator was treated as the application owner", err)
	}
}

func TestServiceSettingsAreSelfOnlyAndPreserveImportedBaselines(t *testing.T) {
	_, q, service := serviceFixture(t, ServiceOptions{})
	before, err := q.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Execute(context.Background(), request("anilist features enable", map[string]any{"feature": "4096"})); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Execute(context.Background(), request("anilist colors set", map[string]any{"updatetype": "14", "color": "#000000"})); err != nil {
		t.Fatal(err)
	}
	after, err := q.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if after.Features != 4223 || after.Revision != 2 || len(after.Colors) != 1 || after.Colors[0].ColorValue != 0 || !reflect.DeepEqual(before.Cursors, after.Cursors) {
		t.Fatal("command settings reset imported state", after)
	}
	if _, err := service.Execute(context.Background(), request("anilist features disable", map[string]any{"feature": "1", "user": "2"})); !errors.Is(err, ErrInvalidCommand) {
		t.Fatal("a self-only command accepted a target user", err)
	}
	result, err := service.Execute(context.Background(), request("anilist features enabled", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(replyEmbed(t, result).Description, "reviews") {
		t.Fatal("enabled features not shown")
	}
}

func TestServiceDestructiveCommandsNeedConfirmationAndHaveDocumentedScope(t *testing.T) {
	_, q, service := serviceFixture(t, ServiceOptions{})
	if _, err := service.Execute(context.Background(), request("anilist user remove", map[string]any{"confirm": false})); !errors.Is(err, ErrConfirmation) {
		t.Fatal("unlinking did not require confirmation", err)
	}
	if _, err := service.Execute(context.Background(), request("anilist user removehere", map[string]any{"confirm": true})); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"anilist", "mal"} {
		accounts, _, err := q.ListAccounts(context.Background(), "10", provider, 0, 25)
		if err != nil || len(accounts) != 0 {
			t.Fatal("removehere did not stop both providers", accounts, err)
		}
	}
	accounts, _, err := q.ListAccounts(context.Background(), "20", "mal", 0, 25)
	if err != nil || len(accounts) != 1 {
		t.Fatal("removehere affected another server", accounts, err)
	}
	if _, err := service.Execute(context.Background(), request("anilist user remove", map[string]any{"confirm": true})); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Settings(context.Background(), "1", "mal"); err != nil {
		t.Fatal("global AniList unlink removed MAL", err)
	}
}

func TestServiceRejectsInvalidAndPrivateCommandScopesBeforeAnyWrite(t *testing.T) {
	_, q, service := serviceFixture(t, ServiceOptions{})
	for _, input := range []Request{request("mal colors set", map[string]any{"updatetype": "14", "color": "#FFFFFF"}), request("mal features enable", map[string]any{"feature": "3"}), request("mal colors set", map[string]any{"updatetype": "0", "color": "private-secret"}), request("unknown", nil)} {
		if _, err := service.Execute(context.Background(), input); !errors.Is(err, ErrInvalidCommand) {
			t.Fatal("invalid option reached storage", err)
		}
	}
	input := request("anilist user remove", map[string]any{"confirm": true})
	input.GuildID = ""
	if _, err := service.Execute(context.Background(), input); !errors.Is(err, ErrInvalidCommand) {
		t.Fatal("DM command was not rejected", err)
	}
	if _, err := q.Settings(context.Background(), "1", "anilist"); err != nil {
		t.Fatal("invalid command deleted the account", err)
	}
}

func TestServiceMALLinkingUsesQuietProviderAPIsAndPreservesExistingAccounts(t *testing.T) {
	s, q := botStore(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.HasPrefix(r.URL.Path, "/profile/") {
			fmt.Fprint(w, `<html><head><meta property="og:url" content="https://myanimelist.net/profile/Reader"></head><body><div id="contentWrapper"><h1><a class="header-right" href="/modules.php?go=report&amp;type=profile&amp;id=203">Report</a></h1><h2><a href="/profile/Reader/favorites">All Favorites</a>Favorites</h2></div></body></html>`)
		} else {
			fmt.Fprint(w, `{"data":[],"paging":{}}`)
		}
	}))
	t.Cleanup(server.Close)
	client, err := mal.NewClient(mal.ClientOptions{ClientID: "test-client", APIEndpoint: server.URL + "/v2", ProfileEndpoint: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return time.Unix(200, 0) }, Wait: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := mal.NewProvider(q, client, mal.ProviderOptions{Now: func() time.Time { return time.Unix(200, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(q, transportFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("linking made a Discord data request") }, HTTPOptions{}), ServiceOptions{Identity: Identity{ApplicationID: "200", UserID: "100", OwnerIDs: []string{"1"}}, Providers: map[string]ProviderCommands{"mal": {Linker: provider}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Execute(context.Background(), request("mal user add", nil)); err != nil || calls.Load() != 0 {
		t.Fatal("existing link refetched or reset state", err)
	}
	input := request("mal user add", map[string]any{"username": "Reader"})
	input.UserID = "3"
	result, err := service.Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	replyEmbed(t, result)
	state, err := q.ForUser(context.Background(), "3", "mal")
	if err != nil || state.ExternalID != "203" || state.Features != 127 {
		t.Fatal("new quiet link failed", state, err)
	}
	var events int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&events); err != nil || events != 0 {
		t.Fatal("command linking replayed historical posts")
	}
}

func TestServiceUserListsAndSearchRepliesAreBoundedAndDoNotPing(t *testing.T) {
	_, _, service := serviceFixture(t, ServiceOptions{Providers: map[string]ProviderCommands{"anilist": {Search: func(ctx context.Context, kind, query, format string) ([]SearchResult, error) {
		if kind != "anime" || query != "Title" || format != "TV" {
			t.Error("wrong search request")
		}
		return []SearchResult{{Title: "Title @everyone", URL: "https://anilist.co/anime/42", Format: "TV", Score: 85, Description: strings.Repeat("語", 8000)}}, nil
	}}}})
	result, err := service.Execute(context.Background(), request("anilist user list", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(replyEmbed(t, result).Description, "AniReader") {
		t.Fatal("tracked users were not listed")
	}
	result, err = service.Execute(context.Background(), request("anilist search anime", map[string]any{"title": "Title", "format": "TV"}))
	if err != nil {
		t.Fatal(err)
	}
	embed := replyEmbed(t, result)
	if len(embed.Fields) != 1 || !strings.Contains(embed.Fields[0].Value, "https://anilist.co/anime/42") {
		t.Fatal("bounded search links missing", embed)
	}
}

func TestServiceReportsCurrentChannelFailuresWithoutLeakingPayloads(t *testing.T) {
	_, _, service := serviceFixture(t, ServiceOptions{})
	result, err := service.Execute(context.Background(), request("status", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(replyEmbed(t, result).Description, "11") {
		t.Fatal("status did not show the configured channel")
	}
}

func TestAboutKeepsProjectIdentityAndLicenseWithoutComparisons(t *testing.T) {
	_, _, service := serviceFixture(t, ServiceOptions{})
	result, err := service.Execute(context.Background(), request("about", nil))
	if err != nil {
		t.Fatal(err)
	}
	text := replyEmbed(t, result).Description
	if !strings.Contains(text, "theinternetuser") || !strings.Contains(text, "AGPL-3.0-only") || !strings.Contains(text, "no warranty") {
		t.Fatal("about omitted the maintainer or license", text)
	}
	for _, comparison := range []string{"Tantei", "Shikimori", "independent", "no website", "did not write"} {
		if strings.Contains(text, comparison) {
			t.Fatal("about repeated attribution or missing-feature comparisons", text)
		}
	}
}

func TestServiceFeatureColorReadsAndResetsKeepOtherOverrides(t *testing.T) {
	_, q, service := serviceFixture(t, ServiceOptions{})
	for _, input := range []Request{request("mal features list", nil), request("anilist colors list", nil), request("about", nil)} {
		result, err := service.Execute(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		replyEmbed(t, result)
	}
	for _, input := range []Request{request("mal colors set", map[string]any{"updatetype": "0", "color": "abcdef"}), request("mal colors set", map[string]any{"updatetype": "1", "color": "#123456"}), request("mal colors remove", map[string]any{"updatetype": "0"}), request("mal features disable", map[string]any{"feature": "1"})} {
		if _, err := service.Execute(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	state, err := q.Settings(context.Background(), "1", "mal")
	if err != nil || state.Features != 126 || len(state.Colors) != 1 || state.Colors[0].UpdateType != 1 {
		t.Fatal("reset affected another override", state, err)
	}
}

func TestOwnerCheckQueuesACycleAndRestartWaitsForTheResponse(t *testing.T) {
	_, q := botStore(t)
	var checks, stops atomic.Int64
	schedule, err := scheduler.New(q, []scheduler.Provider{{Name: "anilist", Delay: time.Hour, Check: func(context.Context, int64) (scheduler.Counts, error) { checks.Add(1); return scheduler.Counts{}, nil }}}, scheduler.Options{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(q, transportFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("owner command made an unexpected HTTP request") }, HTTPOptions{}), ServiceOptions{Identity: Identity{ApplicationID: "200", UserID: "100", OwnerIDs: []string{"1"}}, Scheduler: schedule, Shutdown: func() { stops.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Execute(context.Background(), request("admin check", map[string]any{"name": "anilist"})); err != nil || checks.Load() != 0 {
		t.Fatal("force check restarted or blocked on an inline cycle", err)
	}
	result, err := service.Execute(context.Background(), request("admin restart", map[string]any{"confirm": true}))
	if err != nil || !result.Stop || stops.Load() != 0 {
		t.Fatal("restart ran before its confirmation response", result, err)
	}
	service.Shutdown()
	if stops.Load() != 1 {
		t.Fatal("shutdown callback did not run")
	}
	if _, err := service.Execute(context.Background(), request("admin cleanup", map[string]any{"confirm": true})); err != nil {
		t.Fatal(err)
	}
}

func TestServiceSayHasManagerChecksAndNeverAllowsMentions(t *testing.T) {
	_, q := botStore(t)
	var posts atomic.Int64
	network := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST":
			posts.Add(1)
			var message notification.Message
			if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
				t.Error(err)
			}
			if len(message.AllowedMentions.Parse) != 0 || len(message.AllowedMentions.Users) != 0 {
				t.Error("say allows arbitrary mentions")
			}
			fmt.Fprint(w, `{"id":"1001"}`)
		case strings.Contains(r.URL.Path, "/channels/"):
			fmt.Fprint(w, `{"id":"12","guild_id":"10","type":0}`)
		case strings.Contains(r.URL.Path, "/members/"):
			fmt.Fprint(w, `{"user":{"id":"100"},"roles":[]}`)
		default:
			fmt.Fprint(w, `{"id":"10","roles":[{"id":"10","permissions":"19456"}]}`)
		}
	}, HTTPOptions{})
	service, err := NewService(q, network, ServiceOptions{Identity: Identity{ApplicationID: "200", UserID: "100", OwnerIDs: []string{"1"}}})
	if err != nil {
		t.Fatal(err)
	}
	input := request("say", map[string]any{"channel": "12", "text": "@everyone <@2> hello"})
	input.UserID = "2"
	if _, err := service.Execute(context.Background(), input); !errors.Is(err, ErrForbidden) {
		t.Fatal("ordinary user used say", err)
	}
	input.Permissions = discord.PermissionManageGuild
	result, err := service.Execute(context.Background(), input)
	if err != nil || posts.Load() != 1 {
		t.Fatal(result, err)
	}
	replyEmbed(t, result)
}

func TestServiceOwnerServerRemovalAndIDOptionsPreserveFullSnowflakePrecision(t *testing.T) {
	s, q, service := serviceFixture(t, ServiceOptions{})
	if _, err := s.DB().Exec(`INSERT INTO guilds(id,channel_id) VALUES ('18446744073709551614','22');INSERT INTO guild_users(guild_id,user_id) VALUES ('10','2')`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Execute(context.Background(), request("sm forceremoveuserbyid", map[string]any{"userid": "2", "confirm": true})); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Execute(context.Background(), request("admin forcetoleave", map[string]any{"guildid": "18446744073709551614", "confirm": true})); err != nil {
		t.Fatal(err)
	}
	if _, err := q.GuildStatus(context.Background(), "18446744073709551614"); !errors.Is(err, store.ErrGuildNotConfigured) {
		t.Fatal("large snowflake target was rounded", err)
	}
	if _, err := service.Execute(context.Background(), request("sm removeserver", map[string]any{"confirm": true})); err != nil {
		t.Fatal(err)
	}
	if _, err := q.GuildStatus(context.Background(), "20"); err != nil {
		t.Fatal("server removal affected another guild", err)
	}
}
