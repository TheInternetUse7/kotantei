// Package bot integrates Arikawa interactions, delivery, and the bot runtime.
package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/diamondburned/arikawa/v3/api"
	"github.com/diamondburned/arikawa/v3/discord"
	"github.com/diamondburned/arikawa/v3/utils/httputil"
	"github.com/diamondburned/arikawa/v3/utils/httputil/httpdriver"
	"github.com/diamondburned/arikawa/v3/utils/json/option"

	"kotantei/internal/delivery"
	"kotantei/internal/notification"
	"kotantei/internal/store"
)

var (
	ErrDiscordUnavailable = errors.New("Discord is unavailable; retry later")
	ErrDiscordAuth        = errors.New("Discord bot authentication was rejected")
	ErrDiscordNotFound    = errors.New("Discord resource was not found")
	ErrDiscordMalformed   = errors.New("Discord returned malformed or oversized data")
	ErrDiscordRateLimited = errors.New("Discord request budget exhausted; retry after its cooldown")
	ErrChannelPermissions = errors.New("the bot needs View Channel, Send Messages, and Embed Links in the selected channel")
	ErrChannelType        = errors.New("select a text or announcement channel in this server, not a category or thread")
)

type HTTPOptions struct {
	HTTPClient       *http.Client
	Timeout          time.Duration
	MaxResponseBytes int64
	MaxRateBuckets   int
}
type Identity struct {
	ApplicationID, UserID string
	OwnerIDs              []string
}
type Arikawa struct {
	client  *api.Client
	http    *http.Client
	options HTTPOptions
	limits  *discordRateLimits
}

func NewArikawa(token string, options HTTPOptions) (*Arikawa, error) {
	token = strings.TrimPrefix(strings.TrimSpace(token), "Bot ")
	if token == "" || len(token) > 512 || strings.IndexFunc(token, func(r rune) bool { return r <= 32 || r > 126 }) >= 0 {
		return nil, ErrDiscordAuth
	}
	if options.Timeout == 0 {
		options.Timeout = 15 * time.Second
	}
	if options.MaxResponseBytes == 0 {
		options.MaxResponseBytes = 1 << 20
	}
	if options.MaxRateBuckets == 0 {
		options.MaxRateBuckets = 256
	}
	if options.Timeout <= 0 || options.MaxResponseBytes < 1 || options.MaxResponseBytes > 16<<20 || options.MaxRateBuckets < 1 || options.MaxRateBuckets > 4096 {
		return nil, errors.New("invalid Discord HTTP limits")
	}
	var client http.Client
	if options.HTTPClient != nil {
		client = *options.HTTPClient
	} else {
		base := http.DefaultTransport.(*http.Transport).Clone()
		base.MaxIdleConns = 4
		base.MaxIdleConnsPerHost = 4
		base.MaxConnsPerHost = 8
		base.MaxResponseHeaderBytes = 32 << 10
		base.ResponseHeaderTimeout = options.Timeout
		base.IdleConnTimeout = time.Minute
		client.Transport = base
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = &discordHTTP{base: base, maxBytes: options.MaxResponseBytes}
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Timeout == 0 || client.Timeout > options.Timeout {
		client.Timeout = options.Timeout
	}
	driver := httputil.NewClient()
	driver.Client = httpdriver.WrapClient(client)
	driver.Timeout = options.Timeout
	// The durable outbox owns retries. SDK retries can duplicate POSTs after an
	// ambiguous response and can retain unclosed failed bodies between attempts.
	driver.Retries = 1
	clientAPI := api.NewCustomClient("Bot "+token, driver)
	limits := newDiscordRateLimits(options.MaxRateBuckets)
	// SDK rate buckets have no eviction and webhook routes retain interaction
	// tokens. Keep one SDK limiter per bounded route entry instead, preserving
	// active cooldowns and enforcing a shared conservative global request pace.
	clientAPI.Client.OnRequest = []httputil.RequestOption{func(request httpdriver.Request) error {
		request.AddHeader(http.Header{"Authorization": {clientAPI.Session.Token}, "User-Agent": {clientAPI.Session.UserAgent}})
		return limits.acquire(request)
	}}
	clientAPI.Client.OnResponse = []httputil.ResponseFunc{func(request httpdriver.Request, response httpdriver.Response) error {
		return limits.release(request, httpdriver.OptHeader(response))
	}}
	return &Arikawa{client: clientAPI, http: &client, options: options, limits: limits}, nil
}

func (a *Arikawa) Identity(ctx context.Context) (Identity, error) {
	client, trace := a.request(ctx)
	user, err := client.Me()
	if err != nil {
		return Identity{}, a.requestError(ctx, "identity_user", trace, err)
	}
	if user == nil || user.ID == 0 || !user.Bot {
		return Identity{}, ErrDiscordAuth
	}
	client, trace = a.request(ctx)
	app, err := client.CurrentApplication()
	if err != nil {
		return Identity{}, a.requestError(ctx, "identity_application", trace, err)
	}
	if app == nil || app.ID == 0 {
		return Identity{}, ErrDiscordMalformed
	}
	owner := discord.UserID(0)
	if app.Team != nil {
		owner = app.Team.OwnerID
	} else if app.Owner != nil {
		owner = app.Owner.ID
	}
	if owner == 0 {
		return Identity{}, ErrDiscordMalformed
	}
	return Identity{ApplicationID: app.ID.String(), UserID: user.ID.String(), OwnerIDs: []string{owner.String()}}, nil
}

// Send preserves captured payloads. Arikawa v3.6's high-level embed validator
// counts UTF-8 bytes and replaces black with a default color, so validated
// Discord-UTF-16 payloads use its authenticated, rate-limited RequestJSON API.
func (a *Arikawa) Send(ctx context.Context, item store.Delivery) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !numericID(item.ChannelID) || !numericID(item.UserID) {
		return "", &delivery.Failure{Reason: store.InvalidMessage}
	}
	if err := validatePayload(item.Event.Payload, item.UserID); err != nil {
		return "", &delivery.Failure{Reason: store.InvalidMessage}
	}
	client, trace := a.request(ctx)
	var message *discord.Message
	err := client.RequestJSON(&message, "POST", api.EndpointChannels+item.ChannelID+"/messages", httputil.WithJSONBody(item.Event.Payload))
	if err == nil {
		if message == nil || message.ID == 0 {
			return "", &delivery.Failure{Reason: store.SendFailed}
		}
		return message.ID.String(), nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "", context.DeadlineExceeded
	}
	failure := &delivery.Failure{Reason: store.SendFailed}
	var httpErr *httputil.HTTPError
	if errors.Is(err, ErrDiscordRateLimited) {
		failure.RetryAfter = trace.retryAfter
	}
	if errors.As(err, &httpErr) {
		switch {
		case httpErr.Status == 403 || httpErr.Code == 50013 || httpErr.Code == 50001:
			failure.Reason = store.MissingPermissions
		case httpErr.Status == 404 || httpErr.Code == 10003:
			failure.Reason = store.UnknownChannel
		case httpErr.Status == 400:
			failure.Reason = store.InvalidMessage
		case httpErr.Status == 429:
			failure.RetryAfter = trace.retryAfter
			var body struct {
				After float64 `json:"retry_after"`
			}
			if json.Unmarshal(httpErr.Body, &body) == nil && body.After > 0 && !math.IsInf(body.After, 0) && body.After < float64(math.MaxInt64)/float64(time.Second) {
				failure.RetryAfter = max(failure.RetryAfter, time.Duration(body.After*float64(time.Second)))
			}
		}
	}
	return "", failure
}

func (a *Arikawa) Respond(ctx context.Context, event *discord.InteractionEvent, response api.InteractionResponse) error {
	if !validInteraction(event) {
		return ErrDiscordMalformed
	}
	if response.Data != nil {
		data := *response.Data
		data.AllowedMentions = noMentions()
		response.Data = &data
	}
	client, _ := a.request(ctx)
	return discordError(ctx, client.RespondInteraction(event.ID, event.Token, response))
}
func (a *Arikawa) Edit(ctx context.Context, event *discord.InteractionEvent, payload json.RawMessage) error {
	if !validInteraction(event) {
		return ErrDiscordMalformed
	}
	if err := validatePayload(payload, ""); err != nil {
		return err
	}
	client, _ := a.request(ctx)
	var message *discord.Message
	err := client.RequestJSON(&message, "PATCH", api.EndpointWebhooks+event.AppID.String()+"/"+event.Token+"/messages/@original", httputil.WithJSONBody(payload))
	return discordError(ctx, err)
}
func (a *Arikawa) Register(ctx context.Context, appID string, commands []api.CreateCommandData) error {
	if !numericID(appID) || len(commands) == 0 || len(commands) > 100 {
		return ErrDiscordMalformed
	}
	id, _ := strconv.ParseUint(appID, 10, 64)
	client, trace := a.request(ctx)
	_, err := client.BulkOverwriteCommands(discord.AppID(id), commands)
	return a.requestError(ctx, "register_commands", trace, err)
}

func (a *Arikawa) CheckChannel(ctx context.Context, guildID, channelID, userID string) error {
	if !numericID(guildID) || !numericID(channelID) || !numericID(userID) {
		return ErrChannelType
	}
	g, _ := strconv.ParseUint(guildID, 10, 64)
	c, _ := strconv.ParseUint(channelID, 10, 64)
	u, _ := strconv.ParseUint(userID, 10, 64)
	client, _ := a.request(ctx)
	channel, err := client.Channel(discord.ChannelID(c))
	if err != nil {
		return discordError(ctx, err)
	}
	if channel == nil || channel.ID != discord.ChannelID(c) || channel.GuildID != discord.GuildID(g) || (channel.Type != discord.GuildText && channel.Type != discord.GuildNews) {
		return ErrChannelType
	}
	guild, err := client.Guild(discord.GuildID(g))
	if err != nil {
		return discordError(ctx, err)
	}
	member, err := client.Member(discord.GuildID(g), discord.UserID(u))
	if err != nil {
		return discordError(ctx, err)
	}
	if guild == nil || guild.ID != discord.GuildID(g) || member == nil || member.User.ID != discord.UserID(u) || !completeRoles(guild, member) {
		return ErrDiscordMalformed
	}
	permissions := discord.CalcOverrides(*guild, *channel, *member, guild.Roles)
	if !permissions.Has(discord.PermissionViewChannel | discord.PermissionSendMessages | discord.PermissionEmbedLinks) {
		return ErrChannelPermissions
	}
	return nil
}

func (a *Arikawa) MemberPresent(ctx context.Context, guildID, userID string) (bool, error) {
	if !numericID(guildID) || !numericID(userID) {
		return false, ErrDiscordMalformed
	}
	g, _ := strconv.ParseUint(guildID, 10, 64)
	u, _ := strconv.ParseUint(userID, 10, 64)
	client, trace := a.request(ctx)
	member, err := client.Member(discord.GuildID(g), discord.UserID(u))
	if err != nil {
		if ctx.Err() != nil {
			return false, a.requestError(ctx, "membership_user", trace, ctx.Err())
		}
		var httpErr *httputil.HTTPError
		if errors.As(err, &httpErr) && httpErr.Status == 404 && httpErr.Code == 10007 {
			return false, nil
		}
		return false, a.requestError(ctx, "membership_user", trace, err)
	}
	if member == nil || member.User.ID != discord.UserID(u) {
		return false, a.requestError(ctx, "membership_user", trace, ErrDiscordMalformed)
	}
	return true, nil
}

// Arikawa's Member model does not retain interaction member.permissions. Fetch
// guild/member roles to revalidate manager authority instead of trusting zero.
func (a *Arikawa) GuildPermissions(ctx context.Context, guildID, userID string) (discord.Permissions, error) {
	if !numericID(guildID) || !numericID(userID) {
		return 0, ErrDiscordMalformed
	}
	g, _ := strconv.ParseUint(guildID, 10, 64)
	u, _ := strconv.ParseUint(userID, 10, 64)
	client, _ := a.request(ctx)
	guild, err := client.Guild(discord.GuildID(g))
	if err != nil {
		return 0, discordError(ctx, err)
	}
	member, err := client.Member(discord.GuildID(g), discord.UserID(u))
	if err != nil {
		return 0, discordError(ctx, err)
	}
	if guild == nil || guild.ID != discord.GuildID(g) || member == nil || member.User.ID != discord.UserID(u) || !completeRoles(guild, member) {
		return 0, ErrDiscordMalformed
	}
	return discord.CalcOverrides(*guild, discord.Channel{}, *member, guild.Roles), nil
}

func completeRoles(guild *discord.Guild, member *discord.Member) bool {
	if guild.Roles == nil || member.RoleIDs == nil {
		return false
	}
	roles := make(map[discord.RoleID]bool, len(guild.Roles))
	for _, role := range guild.Roles {
		if role.ID == 0 || roles[role.ID] {
			return false
		}
		roles[role.ID] = true
	}
	if !roles[discord.RoleID(guild.ID)] {
		return false
	}
	for _, id := range member.RoleIDs {
		if !roles[id] {
			return false
		}
	}
	return true
}
func (a *Arikawa) Close() {
	if a.http != nil {
		a.http.CloseIdleConnections()
	}
}

func validatePayload(raw json.RawMessage, userID string) error {
	if len(raw) > 64<<10 || !utf8.Valid(raw) {
		return notification.ErrInvalidMessage
	}
	var message notification.Message
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&message) != nil {
		return notification.ErrInvalidMessage
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return notification.ErrInvalidMessage
	}
	if len(message.Embeds) != 1 || message.AllowedMentions.Parse == nil || message.AllowedMentions.Users == nil || len(message.AllowedMentions.Parse) != 0 || message.AllowedMentions.RepliedUser || len(message.AllowedMentions.Users) > 1 {
		return notification.ErrInvalidMessage
	}
	mention := ""
	if len(message.AllowedMentions.Users) == 1 {
		mention = message.AllowedMentions.Users[0]
		if userID == "" || mention != userID {
			return notification.ErrInvalidMessage
		}
	}
	bounded, err := notification.Payload(message.Embeds[0], mention)
	if err != nil {
		return notification.ErrInvalidMessage
	}
	encoded, err := json.Marshal(message)
	if err != nil || !bytes.Equal(encoded, bounded) {
		return notification.ErrInvalidMessage
	}
	return nil
}
func noMentions() *api.AllowedMentions {
	return &api.AllowedMentions{Parse: []api.AllowedMentionType{}, RepliedUser: option.False}
}
func numericID(id string) bool {
	number, err := strconv.ParseUint(id, 10, 64)
	return err == nil && number > 0 && strconv.FormatUint(number, 10) == id
}
func validInteraction(event *discord.InteractionEvent) bool {
	if event == nil || event.ID == 0 || event.AppID == 0 || event.Token == "" || len(event.Token) > 512 {
		return false
	}
	for _, r := range event.Token {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}
func discordError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, ErrDiscordRateLimited) {
		return ErrDiscordRateLimited
	}
	var httpErr *httputil.HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.Status {
		case 401:
			return ErrDiscordAuth
		case 403:
			return ErrChannelPermissions
		case 404:
			return ErrDiscordNotFound
		case 429:
			return ErrDiscordRateLimited
		}
	}
	if errors.Is(err, ErrDiscordMalformed) {
		return ErrDiscordMalformed
	}
	return ErrDiscordUnavailable
}

type traceKey struct{}
type requestTrace struct {
	retryAfter     time.Duration
	bucket         *discordRateBucket
	held, acquired bool
	started        time.Time
	diagnostics    requestDiagnostics
}

func (a *Arikawa) request(ctx context.Context) (*api.Client, *requestTrace) {
	trace := &requestTrace{started: time.Now(), diagnostics: requestDiagnostics{phase: "rate_limit"}}
	return a.client.WithContext(context.WithValue(ctx, traceKey{}, trace)), trace
}

type discordHTTP struct {
	base     http.RoundTripper
	maxBytes int64
}

func (d *discordHTTP) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != "discord.com" {
		return nil, ErrDiscordMalformed
	}
	trace, _ := request.Context().Value(traceKey{}).(*requestTrace)
	if trace != nil {
		trace.diagnostics.setPhase("request")
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace.diagnostics.httpTrace()))
	}
	response, err := d.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if trace != nil {
		trace.diagnostics.response(response.StatusCode)
		raw := response.Header.Get("Retry-After")
		if seconds, err := strconv.ParseFloat(raw, 64); err == nil && seconds > 0 && seconds < float64(math.MaxInt64)/float64(time.Second) {
			trace.retryAfter = time.Duration(seconds * float64(time.Second))
		} else if at, err := http.ParseTime(raw); err == nil {
			trace.retryAfter = max(0, time.Until(at))
		}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, d.maxBytes+1))
	response.Body.Close()
	if trace != nil {
		trace.diagnostics.body(len(body))
	}
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > d.maxBytes || !utf8.Valid(body) {
		return nil, ErrDiscordMalformed
	}
	normalizeDiscordLimits(response, body)
	response.Body = io.NopCloser(bytes.NewReader(body))
	if trace != nil {
		trace.diagnostics.setPhase("decode")
	}
	return response, nil
}
func (d *discordHTTP) CloseIdleConnections() {
	if closer, ok := d.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
