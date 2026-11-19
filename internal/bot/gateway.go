package bot

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/diamondburned/arikawa/v3/api"
	"github.com/diamondburned/arikawa/v3/discord"
	"github.com/diamondburned/arikawa/v3/gateway"
	"github.com/diamondburned/arikawa/v3/utils/json/option"
	"github.com/diamondburned/arikawa/v3/utils/ws"
	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"
)

var (
	ErrGatewaySessionLimit  = errors.New("Discord session-start limit is exhausted; retry after its reset")
	ErrGatewayConfiguration = errors.New("Discord requires an unsupported shard, gateway version, or intent configuration")
)

type GatewayOptions struct {
	Dialer                    *websocket.Dialer
	DialTimeout, CloseTimeout time.Duration
	MaxMessageBytes           int64
	Logger                    *slog.Logger
}

// Connect lets Arikawa own heartbeats, resume/identify pacing, and reconnection.
// Only READY identity, session state, and incoming interactions are retained;
// no state cache, guild member chunks, messages, or presence data are kept.
func (a *Arikawa) Connect(ctx context.Context, identity Identity, dispatch func(*discord.InteractionEvent), ready func(), options GatewayOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !numericID(identity.ApplicationID) || !numericID(identity.UserID) || dispatch == nil || ready == nil {
		return ErrDiscordMalformed
	}
	if options.DialTimeout == 0 {
		options.DialTimeout = 10 * time.Second
	}
	if options.CloseTimeout == 0 {
		options.CloseTimeout = 2 * time.Second
	}
	if options.MaxMessageBytes == 0 {
		options.MaxMessageBytes = 4 << 20
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.DialTimeout <= 0 || options.CloseTimeout <= 0 || options.MaxMessageBytes < 1 || options.MaxMessageBytes > 16<<20 {
		return ErrDiscordMalformed
	}
	rejectDiscovery := func(field string, details ...any) error {
		// Field names, reason labels, and numeric details are fixed below; never
		// log the token, URL, or response body.
		options.Logger.Error("gateway discovery rejected", append([]any{"field", field}, details...)...)
		return ErrDiscordMalformed
	}
	client, trace := a.request(ctx)
	// Preserve reset_after presence: the SDK's value field maps missing/null
	// to zero, which cannot be distinguished from an explicitly advertised zero.
	var data *struct {
		URL        string `json:"url"`
		Shards     int    `json:"shards"`
		StartLimit *struct {
			Total          int                   `json:"total"`
			Remaining      int                   `json:"remaining"`
			ResetAfter     *discord.Milliseconds `json:"reset_after"`
			MaxConcurrency int                   `json:"max_concurrency"`
		} `json:"session_start_limit"`
	}
	err := client.RequestJSON(&data, http.MethodGet, api.EndpointGatewayBot)
	if err != nil {
		return a.requestError(ctx, "gateway_discovery", trace, err)
	}
	if data == nil || data.StartLimit == nil {
		return rejectDiscovery("session_start_limit")
	}
	u, err := url.Parse(data.URL)
	if err != nil || u.Scheme != "wss" || u.Host != "gateway.discord.gg" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return rejectDiscovery("url")
	}
	limit := data.StartLimit
	if data.Shards != 1 {
		return ErrGatewayConfiguration
	}
	if limit.Total < 1 {
		return rejectDiscovery("total")
	}
	if limit.Remaining < 0 || limit.Remaining > limit.Total {
		return rejectDiscovery("remaining")
	}
	if limit.MaxConcurrency < 1 {
		return rejectDiscovery("max_concurrency")
	}
	if limit.ResetAfter == nil {
		return rejectDiscovery("reset_after", "reason", "missing_or_null")
	}
	resetAfter := float64(*limit.ResetAfter)
	// Bound the SDK's millisecond-to-duration conversion, not the reset period.
	// Exclude the boundary too: float64 rounds MaxInt64 up in nanoseconds.
	if math.IsNaN(resetAfter) || math.IsInf(resetAfter, 0) || (resetAfter != 0 && resetAfter < 1) || resetAfter >= float64(math.MaxInt64)/float64(time.Millisecond) {
		return rejectDiscovery("reset_after", "reset_after_ms", resetAfter)
	}
	if limit.Remaining == 0 {
		return ErrGatewaySessionLimit
	}
	options.Logger.Info("gateway discovery validated", "shards", data.Shards, "session_starts_remaining", limit.Remaining, "reset_after_ms", resetAfter, "identify_refill", resetAfter > 0)
	id := gateway.NewIdentifier(gateway.IdentifyCommand{
		Token:          strings.TrimPrefix(a.client.Session.Token, "Bot "),
		Properties:     gateway.IdentifyProperties{gateway.IdentifyOS: gateway.DefaultIdentity[gateway.IdentifyOS], gateway.IdentifyBrowser: "Kotantei (Arikawa)", gateway.IdentifyDevice: "Kotantei (Arikawa)"},
		Intents:        option.NewUint(uint(gateway.IntentGuilds)),
		LargeThreshold: 50,
	})
	// Conservatively refill one identify per positive reset period. For zero,
	// retain the advertised initial allowance without inventing a refill period:
	// rate.Every(0) would return Inf and bypass the session-start budget entirely.
	var identifyRate rate.Limit
	if resetAfter > 0 {
		identifyRate = rate.Every(limit.ResetAfter.Duration())
	}
	id.IdentifyGlobalLimit = rate.NewLimiter(identifyRate, limit.Remaining)
	params := u.Query()
	params.Set("v", gateway.Version)
	params.Set("encoding", "json")
	u.RawQuery = params.Encode()
	dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: options.DialTimeout, ReadBufferSize: 4096, WriteBufferSize: 4096}
	if options.Dialer != nil {
		dialer = *options.Dialer
	}
	dialer.EnableCompression = false
	if dialer.HandshakeTimeout == 0 || dialer.HandshakeTimeout > options.DialTimeout {
		dialer.HandshakeTimeout = options.DialTimeout
	}
	conn := &gatewaySocket{dialer: dialer, maxBytes: options.MaxMessageBytes, dialTimeout: options.DialTimeout, closeTimeout: options.CloseTimeout}
	opts := gateway.DefaultGatewayOpts
	opts.DialTimeout = options.DialTimeout
	g := gateway.FromWebsocketGateway(ws.NewGateway(ws.NewCustomWebsocket(conn, u.String()), &opts), gateway.State{Identifier: id})
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	ops := g.Connect(runContext)
	var failure error
	verified := false
	// Always drain through gateway cleanup, including after cancellation. SDK
	// finalization can emit an error before closing its bounded event channel.
	for op := range ops {
		if ctx.Err() != nil {
			cancel()
			continue
		}
		if failure != nil {
			continue
		}
		switch event := op.Data.(type) {
		case *gateway.ReadyEvent:
			if event.User.ID.String() != identity.UserID || !event.User.Bot || event.Application.ID.String() != identity.ApplicationID || event.SessionID == "" {
				failure = ErrDiscordAuth
				cancel()
				continue
			}
			if !verified {
				verified = true
				ready()
			}
		case *gateway.InteractionCreateEvent:
			if verified {
				dispatch(&event.InteractionEvent)
			}
		case *ws.CloseEvent:
			if errors.Is(event.Err, ErrDiscordMalformed) {
				failure = ErrDiscordMalformed
				cancel()
			} else if event.Code == 4004 {
				failure = ErrDiscordAuth
				cancel()
			} else if opts.ErrorIsFatalClose(event) {
				failure = ErrGatewayConfiguration
				cancel()
			}
			options.Logger.Warn("gateway connection closed", "reason", gatewayErrorCode(event))
		case *ws.BackgroundErrorEvent:
			options.Logger.Warn("gateway connection interrupted", "reason", gatewayErrorCode(event))
		}
	}
	if failure != nil {
		return failure
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	err = g.LastError()
	if err == nil {
		return ErrDiscordUnavailable
	}
	return safeError(err)
}

func gatewayErrorCode(err error) string {
	if errors.Is(err, ErrDiscordMalformed) {
		return "malformed"
	}
	var closeEvent *ws.CloseEvent
	if errors.As(err, &closeEvent) && closeEvent.Code == 4004 {
		return "authentication"
	}
	return "connection"
}

type ignoredGatewayEvent struct {
	code ws.OpCode
	kind ws.EventType
}

func (e *ignoredGatewayEvent) Op() ws.OpCode           { return e.code }
func (e *ignoredGatewayEvent) EventType() ws.EventType { return e.kind }

// Decode only session/control/interaction data. Large GUILD_CREATE payloads are
// consumed within the packet limit without allocating guild or member caches.
func decodeGatewayPacket(raw []byte) (ws.Op, error) {
	var packet struct {
		Code     *int            `json:"op"`
		Kind     ws.EventType    `json:"t"`
		Sequence *int64          `json:"s"`
		Data     json.RawMessage `json:"d"`
	}
	if json.Unmarshal(raw, &packet) != nil || packet.Code == nil || *packet.Code < 0 || len(packet.Kind) > 128 {
		return ws.Op{}, ErrDiscordMalformed
	}
	op := ws.Op{Code: ws.OpCode(*packet.Code), Type: packet.Kind}
	if packet.Sequence != nil {
		op.Sequence = *packet.Sequence
	}
	if op.Code == 0 && (packet.Sequence == nil || op.Sequence < 0 || packet.Kind == "") {
		return ws.Op{}, ErrDiscordMalformed
	}
	if op.Code == 0 && packet.Kind == "READY" {
		var data struct {
			Version     int          `json:"v"`
			User        discord.User `json:"user"`
			Session     string       `json:"session_id"`
			Application struct {
				ID    discord.AppID            `json:"id"`
				Flags discord.ApplicationFlags `json:"flags"`
			} `json:"application"`
		}
		if json.Unmarshal(packet.Data, &data) != nil || len(data.Session) > 256 {
			return ws.Op{}, ErrDiscordMalformed
		}
		event := &gateway.ReadyEvent{Version: data.Version, User: data.User, SessionID: data.Session}
		event.Application = data.Application
		op.Data = event
		return op, nil
	}
	if op.Code == 0 && packet.Kind != "RESUMED" && packet.Kind != "INTERACTION_CREATE" {
		op.Data = &ignoredGatewayEvent{code: op.Code, kind: op.Type}
		return op, nil
	}
	constructor := gateway.OpUnmarshalers.Lookup(op.Code, op.Type)
	if constructor == nil {
		op.Data = &ignoredGatewayEvent{code: op.Code, kind: op.Type}
		return op, nil
	}
	op.Data = constructor()
	// ACK and reconnect are data-free controls. Discord may omit d entirely;
	// required event data must still pass the normal decoder and validation.
	if len(packet.Data) == 0 && (op.Code == 7 || op.Code == 11) {
		return op, nil
	}
	if json.Unmarshal(packet.Data, op.Data) != nil {
		return ws.Op{}, ErrDiscordMalformed
	}
	if hello, ok := op.Data.(*gateway.HelloEvent); ok && (int64(hello.HeartbeatInterval) < 1000 || int64(hello.HeartbeatInterval) > int64((10*time.Minute)/time.Millisecond)) {
		return ws.Op{}, ErrDiscordMalformed
	}
	return op, nil
}
