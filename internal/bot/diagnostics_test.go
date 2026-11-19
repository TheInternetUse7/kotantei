package bot

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/diamondburned/arikawa/v3/discord"

	"kotantei/internal/scheduler"
)

func discordFailureLog(t *testing.T, logs string) map[string]any {
	t.Helper()
	for _, secret := range []string{"DO-NOT-PRINT", "test-token", "test-client", "gateway.discord.gg", "https://discord.com"} {
		if strings.Contains(logs, secret) {
			t.Fatal("runtime diagnostics exposed credentials, URLs, or upstream text", logs)
		}
	}
	decoder := json.NewDecoder(strings.NewReader(logs))
	for {
		var entry map[string]any
		if err := decoder.Decode(&entry); err == io.EOF {
			t.Fatal("runtime omitted Discord request failure diagnostics", logs)
		} else if err != nil {
			t.Fatal("runtime diagnostic was not valid JSON", err, logs)
		}
		if entry["msg"] == "discord request failed" {
			for _, key := range []string{"elapsed_ms", "timeout_ms", "response_bytes", "response_limit_bytes"} {
				if value, ok := entry[key].(float64); !ok || value < 0 {
					t.Fatal("request diagnostic omitted a safe numeric bound or duration", key, entry)
				}
			}
			if kinds, ok := entry["error_types"].([]any); !ok || len(kinds) == 0 {
				t.Fatal("request diagnostic omitted safe Go error types", entry)
			}
			return entry
		}
	}
}

func TestRuntimeLogsDiscordRequestFailureStages(t *testing.T) {
	for _, tc := range []struct {
		name, stage, operation, reason, phase, body string
		status, code                                int
		want                                        error
	}{
		{"user lookup", "identity", "identity_user", "server_error", "http_status", `{"code":50000,"message":"DO-NOT-PRINT-BODY","errors":{"token":"DO-NOT-PRINT-TOKEN"}}`, 503, 50000, ErrDiscordUnavailable},
		{"application lookup", "identity", "identity_application", "server_error", "http_status", `{"code":50000,"message":"DO-NOT-PRINT-BODY"}`, 503, 50000, ErrDiscordUnavailable},
		{"command registration", "registration", "register_commands", "server_error", "http_status", `{"code":50000,"message":"DO-NOT-PRINT-BODY"}`, 503, 50000, ErrDiscordUnavailable},
		{"gateway discovery", "gateway", "gateway_discovery", "server_error", "http_status", `{"code":50000,"message":"DO-NOT-PRINT-BODY"}`, 503, 50000, ErrDiscordUnavailable},
		{"authentication", "identity", "identity_user", "authentication", "http_status", `{"code":0,"message":"DO-NOT-PRINT-BODY"}`, 401, 0, ErrDiscordAuth},
		{"invalid JSON", "identity", "identity_user", "invalid_json", "decode", `{"id":"DO-NOT-PRINT-ID"}`, 200, 0, ErrDiscordUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, queue := botStore(t)
			var logs bytes.Buffer
			network := &scriptedDiscord{Arikawa: transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
				operation := ""
				switch {
				case strings.HasSuffix(r.URL.Path, "/users/@me"):
					operation = "identity_user"
				case strings.HasSuffix(r.URL.Path, "/oauth2/applications/@me"):
					operation = "identity_application"
				case strings.HasSuffix(r.URL.Path, "/commands"):
					operation = "register_commands"
				case strings.HasSuffix(r.URL.Path, "/gateway/bot"):
					operation = "gateway_discovery"
				}
				if operation == tc.operation {
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
					return
				}
				if !runtimeREST(t, w, r) {
					t.Error("unexpected Discord request")
				}
			}, HTTPOptions{})}
			network.connect = func(ctx context.Context, identity Identity, dispatch func(*discord.InteractionEvent), ready func(), options GatewayOptions) error {
				if tc.stage == "identity" {
					t.Error("gateway started after identity failure")
				}
				if tc.stage == "gateway" {
					return network.Arikawa.Connect(ctx, identity, dispatch, ready, options)
				}
				ready()
				<-ctx.Done()
				return ctx.Err()
			}
			err := Run(context.Background(), queue, runtimeConfig(t), RuntimeOptions{Discord: network, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
			if !errors.Is(err, tc.want) || strings.Contains(err.Error(), "DO-NOT-PRINT") {
				t.Fatal("runtime changed error classification or exposed upstream text", err)
			}
			entry := discordFailureLog(t, logs.String())
			for key, want := range map[string]any{"stage": tc.stage, "operation": tc.operation, "reason": tc.reason, "phase": tc.phase, "http_status": float64(tc.status), "discord_code": float64(tc.code), "timeout_ms": float64(15000)} {
				if entry[key] != want {
					t.Fatal("runtime diagnostic did not identify the failed request", key, want, entry)
				}
			}
		})
	}
}

func TestRuntimeLogsDiscordIdentityNetworkFailures(t *testing.T) {
	for _, tc := range []struct {
		name, reason, phase string
		err                 error
	}{
		{"DNS lookup", "dns_not_found", "dns", &net.DNSError{Err: "DO-NOT-PRINT-DNS", Name: "DO-NOT-PRINT-HOST", IsNotFound: true}},
		{"connection timeout", "timeout", "connect", &net.OpError{Op: "dial", Net: "DO-NOT-PRINT-NETWORK", Err: os.ErrDeadlineExceeded}},
		{"TLS certificate", "tls_certificate", "tls_handshake", &tls.CertificateVerificationError{Err: x509.HostnameError{Host: "DO-NOT-PRINT-HOST", Certificate: &x509.Certificate{DNSNames: []string{"DO-NOT-PRINT-CERTIFICATE"}}}}},
		{"connection refused", "connection_refused", "connect", &net.OpError{Op: "dial", Net: "DO-NOT-PRINT-NETWORK", Err: syscall.ECONNREFUSED}},
		{"proxy connection", "proxy_connect", "connect", &net.OpError{Op: "proxyconnect", Net: "DO-NOT-PRINT-PROXY", Err: errors.New("DO-NOT-PRINT-PROXY-CREDENTIALS")}},
		{"unknown transport error", "transport", "request", errors.New("DO-NOT-PRINT-ERROR")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, queue := botStore(t)
			var logs bytes.Buffer
			var calls atomic.Int64
			transport, err := NewArikawa("DO-NOT-PRINT-BOT-TOKEN", HTTPOptions{Timeout: time.Second, HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if trace := httptrace.ContextClientTrace(r.Context()); trace != nil {
					switch tc.phase {
					case "dns":
						if trace.DNSStart != nil {
							trace.DNSStart(httptrace.DNSStartInfo{Host: "DO-NOT-PRINT-HOST"})
						}
					case "connect":
						if trace.ConnectStart != nil {
							trace.ConnectStart("tcp", "DO-NOT-PRINT-ADDRESS")
						}
					case "tls_handshake":
						if trace.TLSHandshakeStart != nil {
							trace.TLSHandshakeStart()
						}
					}
				}
				return nil, tc.err
			})}})
			if err != nil {
				t.Fatal(err)
			}
			err = Run(context.Background(), queue, runtimeConfig(t), RuntimeOptions{Discord: transport, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
			if !errors.Is(err, ErrDiscordUnavailable) || calls.Load() != 1 || strings.Contains(err.Error(), "DO-NOT-PRINT") {
				t.Fatal("identity failure changed retries or leaked an upstream error", err, calls.Load())
			}
			entry := discordFailureLog(t, logs.String())
			for key, want := range map[string]any{"stage": "identity", "operation": "identity_user", "reason": tc.reason, "phase": tc.phase, "http_status": float64(0), "timeout_ms": float64(1000)} {
				if entry[key] != want {
					t.Fatal("network diagnostic lost its safe error category or HTTP phase", key, want, entry)
				}
			}
			if errors.Is(tc.err, syscall.ECONNREFUSED) && entry["os_error"] != float64(syscall.ECONNREFUSED) {
				t.Fatal("network diagnostic omitted the numeric OS error code", entry)
			}
		})
	}
}

func TestRuntimeLogsDiscordIdentityResponseHeaderTimeout(t *testing.T) {
	_, queue := botStore(t)
	var logs bytes.Buffer
	release := make(chan struct{})
	defer close(release)
	transport := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}, HTTPOptions{Timeout: 100 * time.Millisecond})
	err := Run(context.Background(), queue, runtimeConfig(t), RuntimeOptions{Discord: transport, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("identity HTTP deadline was lost", err)
	}
	entry := discordFailureLog(t, logs.String())
	for key, want := range map[string]any{"stage": "identity", "operation": "identity_user", "reason": "timeout", "phase": "response_headers", "http_status": float64(0), "timeout_ms": float64(100)} {
		if entry[key] != want {
			t.Fatal("timeout diagnostic did not identify waiting for response headers", key, want, entry)
		}
	}
}

func TestRuntimeLogsDiscordIdentityEffectiveHTTPTimeout(t *testing.T) {
	_, queue := botStore(t)
	var logs bytes.Buffer
	transport, err := NewArikawa("test-token", HTTPOptions{
		Timeout: time.Second,
		HTTPClient: &http.Client{Timeout: 20 * time.Millisecond, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = Run(context.Background(), queue, runtimeConfig(t), RuntimeOptions{Discord: transport, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("shorter HTTP client timeout was lost", err)
	}
	entry := discordFailureLog(t, logs.String())
	if entry["timeout_ms"] != float64(20) {
		t.Fatal("diagnostic reported the SDK timeout rather than the tighter HTTP client timeout", entry)
	}
}

type cancelOnLogHandler struct {
	slog.Handler
	message string
	cancel  context.CancelFunc
}

func (h *cancelOnLogHandler) Handle(ctx context.Context, record slog.Record) error {
	err := h.Handler.Handle(ctx, record)
	if record.Message == h.message {
		h.cancel()
	}
	return err
}

func TestRuntimeLogsMembershipVerificationFailuresWithoutRemovingSubscriptions(t *testing.T) {
	_, queue := botStore(t)
	before, err := queue.Memberships(context.Background(), "", "", 100)
	if err != nil || len(before) == 0 {
		t.Fatal("fixture has no tracked memberships", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var logs bytes.Buffer
	logger := slog.New(&cancelOnLogHandler{Handler: slog.NewJSONHandler(&logs, nil), message: "initial tracked membership reconciliation completed", cancel: cancel})
	network := &scriptedDiscord{Arikawa: transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/members/") {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"code":10004,"message":"DO-NOT-PRINT-UNKNOWN-GUILD"}`)
			return
		}
		if !runtimeREST(t, w, r) {
			t.Error("unexpected Discord request")
		}
	}, HTTPOptions{})}
	network.connect = func(ctx context.Context, _ Identity, _ func(*discord.InteractionEvent), ready func(), _ GatewayOptions) error {
		ready()
		<-ctx.Done()
		return ctx.Err()
	}
	err = Run(ctx, queue, runtimeConfig(t), RuntimeOptions{Discord: network, Logger: logger, Reconcile: scheduler.ReconcileOptions{RequestInterval: time.Nanosecond}})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("failed membership verification became a fatal startup error", err)
	}
	after, err := queue.Memberships(context.Background(), "", "", 100)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("failed membership verification changed subscriptions", err)
	}
	for _, secret := range []string{"DO-NOT-PRINT", "test-token", "https://discord.com"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("membership diagnostics exposed credentials or upstream content", logs.String())
		}
	}
	decoder := json.NewDecoder(strings.NewReader(logs.String()))
	warnings := 0
	for {
		var entry map[string]any
		if err := decoder.Decode(&entry); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if entry["msg"] != "tracked membership check failed" {
			continue
		}
		warnings++
		for key, want := range map[string]any{"level": "WARN", "stage": "membership", "operation": "membership_user", "reason": "not_found", "http_status": float64(404), "discord_code": float64(10004)} {
			if entry[key] != want {
				t.Fatal("membership warning omitted the safe Discord failure diagnostic", key, want, entry)
			}
		}
		guild, _ := entry["guild_id"].(string)
		user, _ := entry["user_id"].(string)
		if !numericID(guild) || !numericID(user) {
			t.Fatal("membership warning omitted its numeric guild or user identity", entry)
		}
	}
	if warnings != len(before) {
		t.Fatal("membership failures lacked individual diagnostics", warnings, len(before), logs.String())
	}
}
