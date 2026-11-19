package bot

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptrace"
	"sync"
	"syscall"
	"time"

	"github.com/diamondburned/arikawa/v3/utils/httputil"
)

// HTTP trace callbacks may run concurrently (including parallel connection
// attempts). Retain only fixed phase labels and numeric response metadata.
type requestDiagnostics struct {
	mu            sync.Mutex
	phase         string
	httpStatus    int
	responseBytes int64
}

func (d *requestDiagnostics) setPhase(phase string) {
	d.mu.Lock()
	d.phase = phase
	d.mu.Unlock()
}

func (d *requestDiagnostics) response(status int) {
	d.mu.Lock()
	d.phase = "response_body"
	d.httpStatus = status
	d.mu.Unlock()
}

func (d *requestDiagnostics) body(size int) {
	d.mu.Lock()
	d.responseBytes = int64(size)
	d.mu.Unlock()
}

func (d *requestDiagnostics) snapshot() (string, int, int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.phase, d.httpStatus, d.responseBytes
}

func (d *requestDiagnostics) httpTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart:          func(httptrace.DNSStartInfo) { d.setPhase("dns") },
		ConnectStart:      func(string, string) { d.setPhase("connect") },
		TLSHandshakeStart: func() { d.setPhase("tls_handshake") },
		GotConn:           func(httptrace.GotConnInfo) { d.setPhase("request") },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				d.setPhase("response_headers")
			}
		},
	}
}

// The original error is deliberately not retained: it can contain request or
// proxy URLs, credentials, response bodies, and target-controlled messages.
type discordRequestError struct {
	cause                        error
	operation, reason, phase     string
	httpStatus                   int
	discordCode                  httputil.ErrorCode
	elapsed, timeout, retryAfter time.Duration
	responseBytes, responseLimit int64
	errorTypes                   []string
	osError                      syscall.Errno
}

func (e *discordRequestError) Error() string { return e.cause.Error() }
func (e *discordRequestError) Unwrap() error { return e.cause }

func (a *Arikawa) requestError(ctx context.Context, operation string, trace *requestTrace, err error) error {
	if err == nil {
		return nil
	}
	phase, status, size := trace.diagnostics.snapshot()
	var code httputil.ErrorCode
	var httpErr *httputil.HTTPError
	if errors.As(err, &httpErr) {
		phase, status, code = "http_status", httpErr.Status, httpErr.Code
	}
	var osError syscall.Errno
	errors.As(err, &osError)
	return &discordRequestError{
		cause: discordError(ctx, err), operation: operation, reason: discordRequestReason(err), phase: phase,
		httpStatus: status, discordCode: code, elapsed: time.Since(trace.started), timeout: min(a.options.Timeout, a.http.Timeout),
		retryAfter: trace.retryAfter, responseBytes: size, responseLimit: a.options.MaxResponseBytes,
		errorTypes: errorTypeNames(err), osError: osError,
	}
}

func discordRequestReason(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrDiscordRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrDiscordMalformed):
		return "malformed_response"
	}
	var httpErr *httputil.HTTPError
	if errors.As(err, &httpErr) {
		switch {
		case httpErr.Status == 401:
			return "authentication"
		case httpErr.Status == 403:
			return "permissions"
		case httpErr.Status == 404:
			return "not_found"
		case httpErr.Status == 429:
			return "rate_limited"
		case httpErr.Status >= 500:
			return "server_error"
		default:
			return "http_error"
		}
	}
	var jsonErr httputil.JSONError
	if errors.As(err, &jsonErr) {
		return "invalid_json"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		switch {
		case dnsErr.IsTimeout:
			return "dns_timeout"
		case dnsErr.IsNotFound:
			return "dns_not_found"
		default:
			return "dns"
		}
	}
	var certificateErr *tls.CertificateVerificationError
	var authorityErr x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var invalidCertificateErr x509.CertificateInvalidError
	if errors.As(err, &certificateErr) || errors.As(err, &authorityErr) || errors.As(err, &hostnameErr) || errors.As(err, &invalidCertificateErr) {
		return "tls_certificate"
	}
	var tlsErr tls.RecordHeaderError
	if errors.As(err, &tlsErr) {
		return "tls_protocol"
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return "timeout"
	}
	var operationErr *net.OpError
	if errors.As(err, &operationErr) && operationErr.Op == "proxyconnect" {
		return "proxy_connect"
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset"
	case errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH):
		return "network_unreachable"
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return "connection_closed"
	default:
		return "transport"
	}
}

func errorTypeNames(err error) []string {
	var types []string
	for depth := 0; err != nil && depth < 8; depth++ {
		// %T does not invoke Error/String methods or include error contents.
		types = append(types, fmt.Sprintf("%T", err))
		err = errors.Unwrap(err)
	}
	return types
}

func discordFailureDetails(stage string, err error) []any {
	var failure *discordRequestError
	if !errors.As(err, &failure) {
		return []any{"stage", stage, "error", safeError(err), "error_types", errorTypeNames(err)}
	}
	details := []any{
		"stage", stage, "operation", failure.operation, "reason", failure.reason, "phase", failure.phase,
		"http_status", failure.httpStatus, "discord_code", failure.discordCode,
		"elapsed_ms", failure.elapsed.Milliseconds(), "timeout_ms", failure.timeout.Milliseconds(),
		"response_bytes", failure.responseBytes, "response_limit_bytes", failure.responseLimit,
		"retry_after_ms", failure.retryAfter.Milliseconds(), "error_types", failure.errorTypes,
	}
	if failure.osError != 0 {
		details = append(details, "os_error", uint64(failure.osError))
	}
	return details
}

func logRuntimeFailure(logger *slog.Logger, stage string, err error) {
	message := "runtime stage failed"
	var failure *discordRequestError
	if errors.As(err, &failure) {
		message = "discord request failed"
	}
	logger.Error(message, discordFailureDetails(stage, err)...)
}

func logMembershipFailure(logger *slog.Logger, guildID, userID string, err error) {
	details := discordFailureDetails("membership", err)
	if numericID(guildID) {
		details = append(details, "guild_id", guildID)
	}
	if numericID(userID) {
		details = append(details, "user_id", userID)
	}
	logger.Warn("tracked membership check failed", details...)
}
