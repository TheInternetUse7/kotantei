package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diamondburned/arikawa/v3/api"
	"github.com/diamondburned/arikawa/v3/discord"
	"github.com/diamondburned/arikawa/v3/utils/httputil"

	"kotantei/internal/delivery"
	"kotantei/internal/notification"
	"kotantei/internal/store"
	"kotantei/internal/update"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
func transportFixture(t *testing.T, handler http.HandlerFunc, options HTTPOptions) *Arikawa {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	base, _ := url.Parse(server.URL)
	wire := server.Client().Transport
	if wire == nil {
		wire = http.DefaultTransport
	}
	options.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		u := *r.URL
		u.Scheme, u.Host = base.Scheme, base.Host
		clone.URL = &u
		return wire.RoundTrip(clone)
	})}
	transport, err := NewArikawa("test-token", options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(transport.Close)
	return transport
}
func queuedMessage(t *testing.T, embed notification.Embed, userID string) store.Delivery {
	t.Helper()
	payload, err := notification.Payload(embed, userID)
	if err != nil {
		t.Fatal(err)
	}
	return store.Delivery{ChannelID: "11", UserID: "1", Event: update.Event{Payload: payload}}
}

func TestArikawaSenderPreservesUTF16TextBlackAndCapturedMentions(t *testing.T) {
	item := queuedMessage(t, notification.Embed{Title: strings.Repeat("語", 256), Description: strings.Repeat("語", 3000), Color: 0}, "1")
	var sent notification.Message
	transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v9/channels/11/messages" || r.Header.Get("Authorization") != "Bot test-token" {
			t.Error("wrong authenticated Arikawa delivery route")
		}
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"id":"1001","channel_id":"11"}`)
	}, HTTPOptions{})
	id, err := transport.Send(context.Background(), item)
	if err != nil || id != "1001" {
		t.Fatal(id, err)
	}
	if len([]rune(sent.Embeds[0].Title)) != 256 || sent.Embeds[0].Color != 0 || len([]rune(sent.Embeds[0].Description)) != 3000 || len(sent.AllowedMentions.Parse) != 0 || len(sent.AllowedMentions.Users) != 1 || sent.AllowedMentions.Users[0] != "1" || sent.AllowedMentions.RepliedUser {
		t.Fatal("SDK transport changed the bounded queued payload or mentions", sent)
	}
}

func TestArikawaSenderRejectsUnsafeQueuedPayloadBeforeHTTP(t *testing.T) {
	var calls atomic.Int64
	transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }, HTTPOptions{})
	for _, payload := range []string{`{"title":"not a notification"}`, `{"embeds":[{"title":"Title","color":1}],"allowed_mentions":{"parse":["everyone"],"users":[],"replied_user":false}}`, `{"embeds":[{"title":"Title","color":1}],"allowed_mentions":{"parse":[],"users":["2"],"replied_user":false}}`, `{"embeds":[{"title":"Title","url":"file:///private","color":1}],"allowed_mentions":{"parse":[],"users":[],"replied_user":false}}`} {
		_, err := transport.Send(context.Background(), store.Delivery{ChannelID: "11", UserID: "1", Event: update.Event{Payload: json.RawMessage(payload)}})
		var failure *delivery.Failure
		if !errors.As(err, &failure) || failure.Reason != store.InvalidMessage {
			t.Fatal("unsafe payload was not a terminal message error", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid or unsafe payload reached Discord")
	}
}

func TestArikawaDeliveryErrorsAreClassifiedWithoutHiddenRetriesOrRawBodies(t *testing.T) {
	for _, tc := range []struct {
		status, code int
		want         store.Reason
	}{{403, 50013, store.MissingPermissions}, {404, 10003, store.UnknownChannel}, {400, 50035, store.InvalidMessage}, {429, 0, store.SendFailed}, {503, 0, store.SendFailed}} {
		var calls atomic.Int64
		transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if tc.status == 429 {
				w.Header().Set("Retry-After", "40")
			}
			w.WriteHeader(tc.status)
			fmt.Fprintf(w, `{"code":%d,"message":"DO-NOT-PRINT-SECRET","retry_after":40}`, tc.code)
		}, HTTPOptions{})
		_, err := transport.Send(context.Background(), queuedMessage(t, notification.Embed{Title: "Title", Color: 1}, ""))
		var failure *delivery.Failure
		if !errors.As(err, &failure) || failure.Reason != tc.want || calls.Load() != 1 || strings.Contains(err.Error(), "DO-NOT-PRINT") {
			t.Fatal("unsafe delivery error or SDK automatic retry", err, calls.Load())
		}
		if tc.status == 429 && failure.RetryAfter < 40*time.Second {
			t.Fatal("Discord retry-after was shortened")
		}
	}
}

func TestArikawaDeliveryDeadlineAndResponseBudgetAreBounded(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}, HTTPOptions{Timeout: 20 * time.Millisecond})
	_, err := transport.Send(context.Background(), queuedMessage(t, notification.Embed{Title: "Title", Color: 1}, ""))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("HTTP deadline was lost", err)
	}
	transport = transportFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", 1000)) }, HTTPOptions{MaxResponseBytes: 16})
	if _, err := transport.Send(context.Background(), queuedMessage(t, notification.Embed{Title: "Title", Color: 1}, "")); err == nil || strings.Contains(err.Error(), "xxxx") {
		t.Fatal("oversized Discord response accepted or leaked", err)
	}
}

func TestArikawaIdentityUsesApplicationOrTeamOwnerNotEveryTeamMember(t *testing.T) {
	for _, team := range []bool{false, true} {
		transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/users/@me") {
				fmt.Fprint(w, `{"id":"100","bot":true}`)
				return
			}
			if team {
				fmt.Fprint(w, `{"id":"200","owner":{"id":"999"},"team":{"owner_user_id":"1","members":[{"user":{"id":"2"},"membership_state":2}]}}`)
			} else {
				fmt.Fprint(w, `{"id":"200","owner":{"id":"1"}}`)
			}
		}, HTTPOptions{})
		identity, err := transport.Identity(context.Background())
		if err != nil || identity.ApplicationID != "200" || identity.UserID != "100" || len(identity.OwnerIDs) != 1 || identity.OwnerIDs[0] != "1" {
			t.Fatal("application identity or owner authority was inferred incorrectly", identity, err)
		}
	}
}

func TestArikawaRejectsNonBotIdentitiesAndCrossOriginRedirects(t *testing.T) {
	transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/users/@me") {
			fmt.Fprint(w, `{"id":"100","bot":false}`)
		} else {
			fmt.Fprint(w, `{"id":"200","owner":{"id":"1"}}`)
		}
	}, HTTPOptions{})
	if _, err := transport.Identity(context.Background()); !errors.Is(err, ErrDiscordAuth) {
		t.Fatal("user-account self-bot token accepted", err)
	}
	var leaked atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	t.Cleanup(other.Close)
	transport = transportFixture(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) }, HTTPOptions{})
	if _, err := transport.Identity(context.Background()); err == nil || leaked.Load() != 0 {
		t.Fatal("Discord credentials followed a cross-origin redirect", err)
	}
}

func TestArikawaChannelValidationChecksGuildTypeAndAllRequiredBotPermissions(t *testing.T) {
	for _, tc := range []struct {
		guild     string
		kind      int
		denyEmbed bool
		want      error
	}{{"10", 0, false, nil}, {"99", 0, false, ErrChannelType}, {"10", 4, false, ErrChannelType}, {"10", 11, false, ErrChannelType}, {"10", 0, true, ErrChannelPermissions}} {
		transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/channels/"):
				overwrites := "[]"
				if tc.denyEmbed {
					overwrites = `[{"id":"100","type":1,"allow":"0","deny":"16384"}]`
				}
				fmt.Fprintf(w, `{"id":"11","guild_id":%q,"type":%d,"permission_overwrites":%s}`, tc.guild, tc.kind, overwrites)
			case strings.Contains(r.URL.Path, "/members/"):
				fmt.Fprint(w, `{"user":{"id":"100","bot":true},"roles":[]}`)
			default:
				fmt.Fprint(w, `{"id":"10","owner_id":"1","roles":[{"id":"10","permissions":"19456"}]}`)
			}
		}, HTTPOptions{})
		err := transport.CheckChannel(context.Background(), "10", "11", "100")
		if tc.want == nil {
			if err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, tc.want) {
			t.Fatal("invalid posting target was accepted", tc, err)
		}
	}
}

func TestArikawaMemberChecksDistinguishPositiveAbsenceFromBlockedGuilds(t *testing.T) {
	for _, tc := range []struct {
		code   int
		absent bool
	}{{10007, true}, {10004, false}, {50001, false}} {
		transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
			fmt.Fprintf(w, `{"code":%d,"message":"DO-NOT-PRINT"}`, tc.code)
		}, HTTPOptions{})
		present, err := transport.MemberPresent(context.Background(), "10", "1")
		if present || (tc.absent && err != nil) || (!tc.absent && err == nil) || (err != nil && strings.Contains(err.Error(), "DO-NOT-PRINT")) {
			t.Fatal("unverified absence was treated as a member departure", present, err)
		}
	}
	transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"user":{"id":"2"}}`) }, HTTPOptions{})
	if _, err := transport.MemberPresent(context.Background(), "10", "1"); !errors.Is(err, ErrDiscordMalformed) {
		t.Fatal("wrong member identity was trusted", err)
	}
}

func TestArikawaDeferredResponseAndEditsPreservePrivacyAndBoundedUnicode(t *testing.T) {
	var callback api.InteractionResponse
	var edited notification.Message
	transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/callback") {
			if err := json.NewDecoder(r.Body).Decode(&callback); err != nil {
				t.Error(err)
			}
			w.WriteHeader(204)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&edited); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"id":"1001"}`)
	}, HTTPOptions{})
	event := &discord.InteractionEvent{ID: 1, AppID: 200, Token: "interaction-token"}
	if err := transport.Respond(context.Background(), event, api.InteractionResponse{Type: api.DeferredMessageInteractionWithSource, Data: &api.InteractionResponseData{Flags: discord.EphemeralMessage}}); err != nil {
		t.Fatal(err)
	}
	item := queuedMessage(t, notification.Embed{Title: strings.Repeat("語", 256), Color: 0}, "")
	if err := transport.Edit(context.Background(), event, item.Event.Payload); err != nil {
		t.Fatal(err)
	}
	if callback.Type != api.DeferredMessageInteractionWithSource || callback.Data.Flags != discord.EphemeralMessage || edited.Embeds[0].Color != 0 || len([]rune(edited.Embeds[0].Title)) != 256 || len(edited.AllowedMentions.Parse) != 0 || len(edited.AllowedMentions.Users) != 0 {
		t.Fatal("response lost privacy or legal Unicode text")
	}
}

func TestArikawaOptionsValidateBeforeStartingAnyNetworkWork(t *testing.T) {
	for _, token := range []string{"", "secret\nheader", "Bearer user-token"} {
		if _, err := NewArikawa(token, HTTPOptions{}); err == nil {
			t.Fatal("invalid bot token accepted")
		}
	}
	if _, err := NewArikawa("test-token", HTTPOptions{Timeout: -time.Second}); err == nil {
		t.Fatal("negative timeout accepted")
	}
	if _, err := NewArikawa("test-token", HTTPOptions{MaxResponseBytes: -1}); err == nil {
		t.Fatal("negative response budget accepted")
	}
}

func TestArikawaGuildPermissionsUseVerifiedMemberRoles(t *testing.T) {
	transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/members/") {
			fmt.Fprint(w, `{"user":{"id":"2"},"roles":["99"]}`)
		} else {
			fmt.Fprint(w, `{"id":"10","owner_id":"1","roles":[{"id":"10","permissions":"0"},{"id":"99","permissions":"32"}]}`)
		}
	}, HTTPOptions{})
	permissions, err := transport.GuildPermissions(context.Background(), "10", "2")
	if err != nil || !permissions.Has(discord.PermissionManageGuild) {
		t.Fatal("server manager roles were not revalidated", permissions, err)
	}
}

func TestArikawaRateBucketCapacityNeverEvictsAnActiveCooldown(t *testing.T) {
	var requests atomic.Int64
	transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%.3f", float64(time.Now().Add(time.Minute).UnixMilli())/1000))
		fmt.Fprint(w, `{"id":"1001"}`)
	}, HTTPOptions{MaxRateBuckets: 2})
	for _, channel := range []string{"11", "12"} {
		item := queuedMessage(t, notification.Embed{Title: "Title", Color: 1}, "")
		item.ChannelID = channel
		if _, err := transport.Send(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
	item := queuedMessage(t, notification.Embed{Title: "Title", Color: 1}, "")
	item.ChannelID = "13"
	_, err := transport.Send(context.Background(), item)
	var failure *delivery.Failure
	if !errors.As(err, &failure) || failure.RetryAfter <= 0 || requests.Load() != 2 {
		t.Fatal("full cooldown cache grew or forgot an upstream limit", err, requests.Load())
	}
}

func TestArikawaRateDeadlineDoesNotLeaveTheSDKBucketLocked(t *testing.T) {
	var requests atomic.Int64
	transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"retry_after":1}`)
		} else {
			fmt.Fprint(w, `{"id":"1001"}`)
		}
	}, HTTPOptions{})
	item := queuedMessage(t, notification.Embed{Title: "Title", Color: 1}, "")
	if _, err := transport.Send(context.Background(), item); err == nil {
		t.Fatal("fixture did not establish a cooldown")
	}
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if _, err := transport.Send(short, item); err == nil {
		t.Fatal("request ignored a cooldown beyond its deadline")
	}
	cancel()
	long, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if id, err := transport.Send(long, item); err != nil || id != "1001" {
		t.Fatal("early rate-limit timeout permanently locked later requests", id, err)
	}
}

func TestArikawaHonorsFractionalAndBodyGlobalDiscordCooldowns(t *testing.T) {
	var requests atomic.Int64
	transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "0.2")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"retry_after":0.2,"global":true}`)
	}, HTTPOptions{})
	item := queuedMessage(t, notification.Embed{Title: "Title", Color: 1}, "")
	_, err := transport.Send(context.Background(), item)
	var failure *delivery.Failure
	if !errors.As(err, &failure) || failure.RetryAfter < 200*time.Millisecond {
		t.Fatal("fractional retry-after was lost", err)
	}
	item.ChannelID = "12"
	short, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = transport.Send(short, item)
	if err == nil || requests.Load() != 1 {
		t.Fatal("body-reported global cooldown did not cover other buckets", err, requests.Load())
	}
}

func TestArikawaRateCacheEvictsOnlyReusableEntriesAndRemainsBounded(t *testing.T) {
	transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"id":"1001"}`) }, HTTPOptions{MaxRateBuckets: 2})
	for index := 1; index <= 10; index++ {
		item := queuedMessage(t, notification.Embed{Title: "Title", Color: 1}, "")
		item.ChannelID = fmt.Sprint(index)
		if _, err := transport.Send(context.Background(), item); err != nil {
			t.Fatal("completed routes could not be evicted", err)
		}
	}
	transport.limits.mu.Lock()
	defer transport.limits.mu.Unlock()
	if len(transport.limits.buckets) != 2 {
		t.Fatal("REST route cache grew beyond its hard cap", len(transport.limits.buckets))
	}
}

type observedBody struct{ closed atomic.Bool }

func (*observedBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *observedBody) Close() error           { b.closed.Store(true); return nil }

func TestArikawaRateRejectionClosesAnUnsentRequestBody(t *testing.T) {
	transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "30")
		fmt.Fprint(w, `{"id":"1001"}`)
	}, HTTPOptions{MaxRateBuckets: 1})
	if _, err := transport.Send(context.Background(), queuedMessage(t, notification.Embed{Title: "Title", Color: 1}, "")); err != nil {
		t.Fatal(err)
	}
	client, _ := transport.request(context.Background())
	body := &observedBody{}
	if err := client.RequestJSON(nil, "POST", api.EndpointChannels+"12/messages", httputil.WithBody(body)); err == nil {
		t.Fatal("fixture did not reject the request before transport")
	}
	if !body.closed.Load() {
		t.Fatal("rate rejection left an unsent SDK request body open")
	}
}

func TestArikawaPermissionChecksRejectIncompleteRoleData(t *testing.T) {
	for _, member := range []string{`{"user":{"id":"2"}}`, `{"user":{"id":"2"},"roles":["99"]}`} {
		transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/members/") {
				fmt.Fprint(w, member)
			} else {
				fmt.Fprint(w, `{"id":"10","owner_id":"1","roles":[{"id":"10","permissions":"32"}]}`)
			}
		}, HTTPOptions{})
		if _, err := transport.GuildPermissions(context.Background(), "10", "2"); !errors.Is(err, ErrDiscordMalformed) {
			t.Fatal("incomplete membership roles were trusted", err)
		}
	}
}
