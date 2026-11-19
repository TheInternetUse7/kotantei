package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diamondburned/arikawa/v3/discord"
	"github.com/diamondburned/arikawa/v3/gateway"
	"github.com/gorilla/websocket"
)

func gatewayFixture(t *testing.T, serve func(*websocket.Conn), discovery string) (*Arikawa, GatewayOptions) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("encoding") != "json" || r.URL.Query().Get("v") != "9" || r.Header.Get("Authorization") != "" || r.Header.Get("Sec-WebSocket-Extensions") != "" {
			t.Error("unexpected gateway handshake, credentials, or compression")
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		serve(conn)
	}))
	t.Cleanup(server.Close)
	address, _ := url.Parse(server.URL)
	tlsConfig := server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsConfig.ServerName = address.Hostname()
	dialer := &websocket.Dialer{
		TLSClientConfig: tlsConfig,
		NetDialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address.Host)
		},
	}
	if discovery == "" {
		discovery = `{"url":"wss://gateway.discord.gg","shards":1,"session_start_limit":{"total":1000,"remaining":999,"reset_after":86400000,"max_concurrency":1}}`
	}
	transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v9/gateway/bot" || r.Header.Get("Authorization") != "Bot test-token" {
			t.Error("gateway discovery bypassed the bounded authenticated client")
		}
		fmt.Fprint(w, discovery)
	}, HTTPOptions{})
	return transport, GatewayOptions{Dialer: dialer, CloseTimeout: 100 * time.Millisecond}
}

func readGatewayLogin(t *testing.T, conn *websocket.Conn, wantOp int) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := conn.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}}); err != nil {
		t.Error(err)
		return
	}
	var login struct {
		Op   int `json:"op"`
		Data struct {
			Token    string `json:"token"`
			Intents  uint   `json:"intents"`
			Compress bool   `json:"compress"`
			Session  string `json:"session_id"`
			Sequence int64  `json:"seq"`
		} `json:"d"`
	}
	if err := conn.ReadJSON(&login); err != nil {
		t.Error(err)
		return
	}
	if login.Op != wantOp || login.Data.Token != "test-token" || login.Data.Compress {
		t.Error("unexpected gateway authentication packet")
	}
	if wantOp == 2 && login.Data.Intents != uint(gateway.IntentGuilds) {
		t.Error("privileged or unrelated gateway intent requested")
	}
	if wantOp == 6 && (login.Data.Session != "fixture-session" || login.Data.Sequence < 1) {
		t.Error("reconnect did not resume the captured gateway session")
	}
}

func sendGatewayReady(t *testing.T, conn *websocket.Conn, user string) {
	t.Helper()
	packet := fmt.Sprintf(`{"op":0,"t":"READY","s":1,"d":{"v":9,"user":{"id":%q,"bot":true},"session_id":"fixture-session","guilds":[],"application":{"id":"200","flags":0}}}`, user)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(packet)); err != nil {
		t.Error(err)
	}
}

func TestGatewaySignalsVerifiedReadinessAndDispatchesInteractions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	event := commandEvent(t, 1, "status", nil)
	transport, options := gatewayFixture(t, func(conn *websocket.Conn) {
		readGatewayLogin(t, conn, 2)
		sendGatewayReady(t, conn, "100")
		if err := conn.WriteJSON(map[string]any{"op": 0, "t": "INTERACTION_CREATE", "s": 2, "d": event}); err != nil {
			t.Error(err)
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}, "")
	var ready, interactions atomic.Int64
	err := transport.Connect(ctx, Identity{ApplicationID: "200", UserID: "100"}, func(received *discord.InteractionEvent) {
		if ready.Load() != 1 || received.ID != 1 {
			t.Error("interaction preceded verified readiness or changed")
		}
		interactions.Add(1)
		cancel()
	}, func() { ready.Add(1) }, options)
	if !errors.Is(err, context.Canceled) || ready.Load() != 1 || interactions.Load() != 1 {
		t.Fatal("gateway did not deliver one verified session", err, ready.Load(), interactions.Load())
	}
}

func TestGatewayReconnectResumesWithoutRepeatingReadiness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var connections, ready atomic.Int64
	transport, options := gatewayFixture(t, func(conn *websocket.Conn) {
		if connections.Add(1) == 1 {
			readGatewayLogin(t, conn, 2)
			sendGatewayReady(t, conn, "100")
			conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1001, "fixture interruption"), time.Now().Add(time.Second))
			return
		}
		readGatewayLogin(t, conn, 6)
		conn.WriteMessage(websocket.TextMessage, []byte(`{"op":0,"t":"RESUMED","s":2,"d":{}}`))
		conn.WriteJSON(map[string]any{"op": 0, "t": "INTERACTION_CREATE", "s": 3, "d": commandEvent(t, 2, "status", nil)})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}, "")
	err := transport.Connect(ctx, Identity{ApplicationID: "200", UserID: "100"}, func(*discord.InteractionEvent) { cancel() }, func() { ready.Add(1) }, options)
	if !errors.Is(err, context.Canceled) || connections.Load() != 2 || ready.Load() != 1 {
		t.Fatal("reconnect identified again or repeated startup", err, connections.Load(), ready.Load())
	}
}

func TestGatewayRejectsMismatchedReadyIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	transport, options := gatewayFixture(t, func(conn *websocket.Conn) {
		readGatewayLogin(t, conn, 2)
		sendGatewayReady(t, conn, "999")
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}, "")
	var ready atomic.Bool
	err := transport.Connect(ctx, Identity{ApplicationID: "200", UserID: "100"}, func(*discord.InteractionEvent) { t.Error("mismatched session dispatched an interaction") }, func() { ready.Store(true) }, options)
	if !errors.Is(err, ErrDiscordAuth) || ready.Load() {
		t.Fatal("gateway trusted a different bot identity", err)
	}
}

func TestGatewayDiscoveryAcceptsResetDurations(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resetAfter float64
	}{
		{"zero", 0},
		{"one millisecond", 1},
		{"just over seven days", 604800001},
		{"thirty days", 2592000000},
		{"fractional milliseconds", 2592000000.5},
		{"maximum safe duration", math.Nextafter(float64(math.MaxInt64)/float64(time.Millisecond), 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			discovery := fmt.Sprintf(`{"url":"wss://gateway.discord.gg","shards":1,"session_start_limit":{"total":1000,"remaining":999,"reset_after":%g,"max_concurrency":1}}`, tc.resetAfter)
			transport, options := gatewayFixture(t, func(conn *websocket.Conn) {
				readGatewayLogin(t, conn, 2)
				sendGatewayReady(t, conn, "100")
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
				}
			}, discovery)
			var logs bytes.Buffer
			options.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			var ready atomic.Int64
			err := transport.Connect(ctx, Identity{ApplicationID: "200", UserID: "100"}, func(*discord.InteractionEvent) {}, func() {
				ready.Add(1)
				cancel()
			}, options)
			if !errors.Is(err, context.Canceled) || ready.Load() != 1 {
				t.Fatal("valid reset prevented gateway readiness", err, ready.Load(), logs.String())
			}
		})
	}
}

func TestGatewayDiscoveryRejectsInvalidResetDurationsBeforeDial(t *testing.T) {
	for _, tc := range []struct{ name, resetAfter string }{
		{"negative", "-1"},
		{"less than one millisecond", "0.5"},
		{"null", "null"},
		{"duration overflow boundary", fmt.Sprintf("%g", float64(math.MaxInt64)/float64(time.Millisecond))},
		{"duration overflow", "9223372036855"},
		{"huge finite value", "1e300"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			discovery := fmt.Sprintf(`{"url":"wss://gateway.discord.gg","shards":1,"session_start_limit":{"total":1000,"remaining":999,"reset_after":%s,"max_concurrency":1}}`, tc.resetAfter)
			var dials atomic.Int64
			transport, options := gatewayFixture(t, func(*websocket.Conn) { dials.Add(1) }, discovery)
			var logs bytes.Buffer
			options.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := transport.Connect(ctx, Identity{ApplicationID: "200", UserID: "100"}, func(*discord.InteractionEvent) {}, func() {}, options)
			if !errors.Is(err, ErrDiscordMalformed) || dials.Load() != 0 {
				t.Fatal("invalid reset duration was not rejected before dial", err, dials.Load())
			}
		})
	}
}

func TestGatewayDiscoveryLogsRejectedResetDurationWithoutExposingData(t *testing.T) {
	var logs bytes.Buffer
	discovery := `{"url":"wss://gateway.discord.gg","shards":1,"session_start_limit":{"total":1000,"remaining":999,"reset_after":-1,"max_concurrency":1},"debug":"DO-NOT-PRINT-RESPONSE"}`
	transport, options := gatewayFixture(t, func(*websocket.Conn) { t.Error("invalid reset duration dialed the gateway") }, discovery)
	options.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	err := transport.Connect(context.Background(), Identity{ApplicationID: "200", UserID: "100"}, func(*discord.InteractionEvent) {}, func() {}, options)
	if !errors.Is(err, ErrDiscordMalformed) {
		t.Fatal("invalid reset duration was not rejected", err)
	}
	var entry struct {
		Field      string   `json:"field"`
		ResetAfter *float64 `json:"reset_after_ms"`
	}
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal("reset duration diagnostic was not valid JSON", err, logs.String())
	}
	if entry.Field != "reset_after" || entry.ResetAfter == nil || *entry.ResetAfter != -1 {
		t.Fatal("reset rejection omitted its numeric millisecond value", logs.String())
	}
	for _, secret := range []string{"DO-NOT-PRINT", "test-token", "gateway.discord.gg"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("reset duration diagnostic exposed credentials, URL, or response body", logs.String())
		}
	}
}

func TestGatewayDiscoveryRejectsMissingOrNullResetDurations(t *testing.T) {
	for _, tc := range []struct{ name, response string }{
		{"missing", `{"url":"wss://gateway.discord.gg","shards":1,"session_start_limit":{"total":1000,"remaining":999,"max_concurrency":1}}`},
		{"null", `{"url":"wss://gateway.discord.gg","shards":1,"session_start_limit":{"total":1000,"remaining":999,"reset_after":null,"max_concurrency":1}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			transport, options := gatewayFixture(t, func(*websocket.Conn) { t.Error("missing reset period dialed the gateway") }, tc.response)
			options.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			err := transport.Connect(context.Background(), Identity{ApplicationID: "200", UserID: "100"}, func(*discord.InteractionEvent) {}, func() {}, options)
			if !errors.Is(err, ErrDiscordMalformed) {
				t.Fatal("missing or null reset period was accepted", err)
			}
			var entry struct {
				Field      string   `json:"field"`
				Reason     string   `json:"reason"`
				ResetAfter *float64 `json:"reset_after_ms"`
			}
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatal("reset presence diagnostic was not valid JSON", err, logs.String())
			}
			if entry.Field != "reset_after" || entry.Reason != "missing_or_null" || entry.ResetAfter != nil {
				t.Fatal("missing reset period was incorrectly reported as an explicit zero", logs.String())
			}
		})
	}
}

func TestGatewayDiscoveryZeroResetStillRejectsExhaustedSessionLimits(t *testing.T) {
	discovery := `{"url":"wss://gateway.discord.gg","shards":1,"session_start_limit":{"total":1000,"remaining":0,"reset_after":0,"max_concurrency":1}}`
	transport, options := gatewayFixture(t, func(*websocket.Conn) { t.Error("exhausted session limit dialed the gateway") }, discovery)
	err := transport.Connect(context.Background(), Identity{ApplicationID: "200", UserID: "100"}, func(*discord.InteractionEvent) {}, func() {}, options)
	if !errors.Is(err, ErrGatewaySessionLimit) {
		t.Fatal("zero reset period bypassed or obscured the exhausted session limit", err)
	}
}

func TestGatewayZeroResetDoesNotRefillTheAdvertisedIdentifyBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var connections, identifies, ready atomic.Int64
	discovery := `{"url":"wss://gateway.discord.gg","shards":1,"session_start_limit":{"total":1,"remaining":1,"reset_after":0,"max_concurrency":1}}`
	transport, options := gatewayFixture(t, func(conn *websocket.Conn) {
		if connections.Add(1) == 1 {
			readGatewayLogin(t, conn, 2)
			identifies.Add(1)
			sendGatewayReady(t, conn, "100")
			if err := conn.WriteJSON(map[string]any{"op": 9, "d": true}); err != nil {
				t.Error(err)
				return
			}
		} else if err := conn.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}}); err != nil {
			t.Error(err)
			return
		}
		for {
			var packet struct {
				Op int `json:"op"`
			}
			if err := conn.ReadJSON(&packet); err != nil {
				return
			}
			if packet.Op == 2 {
				identifies.Add(1)
				cancel()
				return
			}
		}
	}, discovery)
	var logs bytes.Buffer
	options.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	err := transport.Connect(ctx, Identity{ApplicationID: "200", UserID: "100"}, func(*discord.InteractionEvent) {}, func() { ready.Add(1) }, options)
	if !errors.Is(err, context.DeadlineExceeded) || identifies.Load() != 1 || ready.Load() != 1 {
		t.Fatal("zero reset rejected startup or allowed identifies beyond the advertised budget", err, identifies.Load(), ready.Load(), logs.String())
	}
}

func TestGatewayDiscoveryRejectsUnsafeURLsAndExhaustedSessionLimitsBeforeDial(t *testing.T) {
	for _, data := range []string{
		`{"url":"wss://untrusted.example","shards":1,"session_start_limit":{"total":1000,"remaining":999,"reset_after":86400000,"max_concurrency":1}}`,
		`{"url":"wss://gateway.discord.gg?token=secret","shards":1,"session_start_limit":{"total":1000,"remaining":999,"reset_after":86400000,"max_concurrency":1}}`,
		`{"url":"wss://gateway.discord.gg","shards":1,"session_start_limit":{"total":1000,"remaining":0,"reset_after":86400000,"max_concurrency":1}}`,
		`{"url":"wss://gateway.discord.gg","shards":2,"session_start_limit":{"total":1000,"remaining":999,"reset_after":86400000,"max_concurrency":1}}`,
		`{"url":"wss://gateway.discord.gg","shards":1,"session_start_limit":null}`,
	} {
		var dials atomic.Int64
		transport, options := gatewayFixture(t, func(*websocket.Conn) { dials.Add(1) }, data)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := transport.Connect(ctx, Identity{ApplicationID: "200", UserID: "100"}, func(*discord.InteractionEvent) {}, func() {}, options)
		cancel()
		if err == nil || errors.Is(err, context.DeadlineExceeded) || dials.Load() != 0 {
			t.Fatal("unsafe or unsupported gateway discovery was not rejected before dial", err, dials.Load())
		}
	}
}

func TestGatewayBoundsPacketsAndDoesNotLogRawCloseReasons(t *testing.T) {
	t.Run("oversized packet", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var logs bytes.Buffer
		transport, options := gatewayFixture(t, func(conn *websocket.Conn) {
			readGatewayLogin(t, conn, 2)
			sendGatewayReady(t, conn, "100")
			conn.WriteMessage(websocket.TextMessage, []byte(`{"op":0,"t":"GUILD_CREATE","s":2,"d":{"name":"`+strings.Repeat("DO-NOT-PRINT", 1000)+`"}}`))
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}, "")
		options.MaxMessageBytes = 512
		options.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
		err := transport.Connect(ctx, Identity{ApplicationID: "200", UserID: "100"}, func(*discord.InteractionEvent) {}, func() {}, options)
		if !errors.Is(err, ErrDiscordMalformed) || strings.Contains(logs.String(), "DO-NOT-PRINT") {
			t.Fatal("oversized gateway data was retained, retried indefinitely, or logged", err, logs.String())
		}
	})
	t.Run("fatal close", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var logs bytes.Buffer
		transport, options := gatewayFixture(t, func(conn *websocket.Conn) {
			readGatewayLogin(t, conn, 2)
			conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4004, "DO-NOT-PRINT-SECRET"), time.Now().Add(10*time.Second))
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}, "")
		options.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
		err := transport.Connect(ctx, Identity{ApplicationID: "200", UserID: "100"}, func(*discord.InteractionEvent) {}, func() {}, options)
		if !errors.Is(err, ErrDiscordAuth) || strings.Contains(logs.String()+err.Error(), "DO-NOT-PRINT") {
			t.Fatal("fatal gateway close exposed upstream text", err, logs.String())
		}
	})
}

func TestGatewayCanceledStartAndInvalidOptionsDoNotDial(t *testing.T) {
	transport, options := gatewayFixture(t, func(*websocket.Conn) { t.Error("invalid start dialed the gateway") }, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := transport.Connect(ctx, Identity{ApplicationID: "200", UserID: "100"}, func(*discord.InteractionEvent) {}, func() {}, options); !errors.Is(err, context.Canceled) {
		t.Fatal("gateway lost cancellation", err)
	}
	for _, invalid := range []GatewayOptions{{MaxMessageBytes: -1}, {DialTimeout: -time.Second}, {CloseTimeout: -time.Second}} {
		if err := transport.Connect(context.Background(), Identity{ApplicationID: "200", UserID: "100"}, func(*discord.InteractionEvent) {}, func() {}, invalid); err == nil {
			t.Fatal("invalid gateway limits accepted")
		}
	}
}

func TestGatewayDecoderAcceptsZeroDispatchSequence(t *testing.T) {
	op, err := decodeGatewayPacket([]byte(`{"op":0,"t":"READY","s":0,"d":{"v":9,"user":{"id":"100","bot":true},"session_id":"fixture-session","application":{"id":"200","flags":0}}}`))
	if err != nil || op.Sequence != 0 {
		t.Fatal("valid initial sequence zero was rejected", err)
	}
	if ready, ok := op.Data.(*gateway.ReadyEvent); !ok || ready.User.ID.String() != "100" || ready.SessionID != "fixture-session" {
		t.Fatal("initial READY identity/session data was lost", op.Data)
	}
}

func TestGatewayDecoderAcceptsDataFreeControlPackets(t *testing.T) {
	for _, raw := range []string{`{"op":11}`, `{"op":11,"s":null,"t":null}`, `{"op":7}`, `{"op":7,"d":null}`} {
		op, err := decodeGatewayPacket([]byte(raw))
		if err != nil {
			t.Fatal("valid data-free gateway control was rejected", raw, err)
		}
		switch op.Data.(type) {
		case *gateway.HeartbeatAckEvent, *gateway.ReconnectEvent:
		default:
			t.Fatal("gateway control was not handed to Arikawa", op.Data)
		}
	}
}

func TestGatewayDecoderRejectsMissingNullNegativeAndFractionalDispatchSequences(t *testing.T) {
	for _, raw := range []string{
		`{"op":0,"t":"GUILD_CREATE","d":{}}`,
		`{"op":0,"t":"GUILD_CREATE","s":null,"d":{}}`,
		`{"op":0,"t":"GUILD_CREATE","s":-1,"d":{}}`,
		`{"op":0,"t":"GUILD_CREATE","s":0.5,"d":{}}`,
		`{"op":10}`, `{"op":9}`, `{"op":11,"d":42}`,
	} {
		if _, err := decodeGatewayPacket([]byte(raw)); !errors.Is(err, ErrDiscordMalformed) {
			t.Fatal("required gateway data/sequence validation was bypassed", raw, err)
		}
	}
}

func TestGatewayConnectAcceptsInitialSequenceZeroAndDataFreeHeartbeatACK(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	transport, options := gatewayFixture(t, func(conn *websocket.Conn) {
		readGatewayLogin(t, conn, 2)
		conn.WriteMessage(websocket.TextMessage, []byte(`{"op":0,"t":"READY","s":0,"d":{"v":9,"user":{"id":"100","bot":true},"session_id":"fixture-session","application":{"id":"200","flags":0}}}`))
		conn.WriteMessage(websocket.TextMessage, []byte(`{"op":11}`))
		conn.WriteJSON(map[string]any{"op": 0, "t": "INTERACTION_CREATE", "s": 1, "d": commandEvent(t, 1, "status", nil)})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}, "")
	var ready, interactions atomic.Int64
	err := transport.Connect(ctx, Identity{ApplicationID: "200", UserID: "100"}, func(*discord.InteractionEvent) {
		interactions.Add(1)
		cancel()
	}, func() { ready.Add(1) }, options)
	if !errors.Is(err, context.Canceled) || ready.Load() != 1 || interactions.Load() != 1 {
		t.Fatal("valid initial session/control packets prevented runtime startup", err, ready.Load(), interactions.Load())
	}
}

func TestGatewayDiscoveryDiagnosticsIdentifyInvalidFieldsWithoutExposingData(t *testing.T) {
	for _, tc := range []struct{ field, response string }{
		{"session_start_limit", `{"url":"wss://gateway.discord.gg","shards":1,"session_start_limit":null}`},
		{"url", `{"url":"wss://DO-NOT-PRINT.example/path","shards":1,"session_start_limit":{"total":1000,"remaining":999,"reset_after":86400000,"max_concurrency":1}}`},
		{"total", `{"url":"wss://gateway.discord.gg","shards":1,"session_start_limit":{"total":0,"remaining":0,"reset_after":86400000,"max_concurrency":1}}`},
		{"remaining", `{"url":"wss://gateway.discord.gg","shards":1,"session_start_limit":{"total":1000,"remaining":1001,"reset_after":86400000,"max_concurrency":1}}`},
		{"max_concurrency", `{"url":"wss://gateway.discord.gg","shards":1,"session_start_limit":{"total":1000,"remaining":999,"reset_after":86400000,"max_concurrency":0}}`},
		{"reset_after", `{"url":"wss://gateway.discord.gg","shards":1,"session_start_limit":{"total":1000,"remaining":999,"reset_after":-1,"max_concurrency":1}}`},
	} {
		t.Run(tc.field, func(t *testing.T) {
			var logs bytes.Buffer
			transport, options := gatewayFixture(t, func(*websocket.Conn) { t.Error("invalid discovery dialed the gateway") }, tc.response)
			options.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			err := transport.Connect(context.Background(), Identity{ApplicationID: "200", UserID: "100"}, func(*discord.InteractionEvent) {}, func() {}, options)
			if !errors.Is(err, ErrDiscordMalformed) || !strings.Contains(logs.String(), `"field":"`+tc.field+`"`) || strings.Contains(logs.String(), "DO-NOT-PRINT") {
				t.Fatal("gateway rejection omitted its safe field diagnostic or exposed upstream data", err, logs.String())
			}
		})
	}
}
