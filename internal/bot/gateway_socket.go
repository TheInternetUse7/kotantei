package bot

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/diamondburned/arikawa/v3/utils/ws"
	"github.com/gorilla/websocket"
)

// A small ws.Connection driver adds packet limits and sanitized errors without
// changing Arikawa's global logging functions or using its unbounded decoder.
// Compression is not negotiated/requested: the read limit applies to JSON size.
type gatewaySocket struct {
	dialer                    websocket.Dialer
	maxBytes                  int64
	dialTimeout, closeTimeout time.Duration
	mu                        sync.Mutex
	current                   *openSocket
}
type openSocket struct {
	conn   *websocket.Conn
	writes chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *gatewaySocket) Dial(ctx context.Context, address string) (<-chan ws.Op, error) {
	_ = s.Close(false)
	dialContext, cancelDial := context.WithTimeout(ctx, s.dialTimeout)
	conn, response, err := s.dialer.DialContext(dialContext, address, nil)
	cancelDial()
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrDiscordUnavailable
	}
	conn.SetReadLimit(s.maxBytes)
	readContext, cancelRead := context.WithCancel(context.Background())
	state := &openSocket{conn: conn, writes: make(chan struct{}, 1), cancel: cancelRead, done: make(chan struct{})}
	s.mu.Lock()
	s.current = state
	s.mu.Unlock()
	events := make(chan ws.Op, 1)
	go func() {
		defer close(state.done)
		defer close(events)
		defer conn.Close()
		for {
			kind, reader, err := conn.NextReader()
			if err != nil {
				socketClose(readContext, events, err)
				return
			}
			if kind != websocket.TextMessage {
				socketClose(readContext, events, ErrDiscordMalformed)
				return
			}
			raw, err := io.ReadAll(io.LimitReader(reader, s.maxBytes+1))
			if err != nil {
				if errors.Is(err, websocket.ErrReadLimit) {
					err = ErrDiscordMalformed
				}
				socketClose(readContext, events, err)
				return
			}
			if int64(len(raw)) > s.maxBytes || !utf8.Valid(raw) {
				socketClose(readContext, events, ErrDiscordMalformed)
				return
			}
			op, err := decodeGatewayPacket(raw)
			if err != nil {
				socketClose(readContext, events, err)
				return
			}
			select {
			case events <- op:
			case <-readContext.Done():
				return
			}
		}
	}()
	return events, nil
}

func socketClose(ctx context.Context, events chan<- ws.Op, err error) {
	event := &ws.CloseEvent{Code: -1, Err: ErrDiscordUnavailable}
	var upstream *websocket.CloseError
	if errors.As(err, &upstream) {
		event.Code = upstream.Code
	}
	if errors.Is(err, ErrDiscordMalformed) || errors.Is(err, websocket.ErrReadLimit) {
		event.Err = ErrDiscordMalformed
	}
	select {
	case events <- ws.Op{Code: event.Op(), Type: event.EventType(), Data: event}:
	case <-ctx.Done():
	}
}

func (s *gatewaySocket) Send(ctx context.Context, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	state := s.current
	s.mu.Unlock()
	if state == nil {
		return ws.ErrWebsocketClosed
	}
	select {
	case state.writes <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-state.writes }()
	deadline := time.Now().Add(s.dialTimeout)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	state.conn.SetWriteDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { state.conn.Close() })
	defer stop()
	if err := state.conn.WriteMessage(websocket.TextMessage, raw); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrDiscordUnavailable
	}
	return nil
}

func (s *gatewaySocket) Close(graceful bool) error {
	s.mu.Lock()
	state := s.current
	s.current = nil
	s.mu.Unlock()
	if state == nil {
		return ws.ErrWebsocketClosed
	}
	state.cancel()
	if graceful {
		_ = state.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(s.closeTimeout))
	}
	_ = state.conn.Close()
	<-state.done
	return nil
}
