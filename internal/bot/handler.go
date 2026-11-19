package bot

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/diamondburned/arikawa/v3/api"
	"github.com/diamondburned/arikawa/v3/discord"
	"github.com/diamondburned/arikawa/v3/utils/json/option"

	"kotantei/internal/anilist"
	"kotantei/internal/mal"
	"kotantei/internal/notification"
	"kotantei/internal/store"
)

var (
	ErrStarting      = errors.New("the bot is starting; retry after readiness")
	ErrCommandBusy   = errors.New("the bot is busy; try again shortly")
	ErrCommandFailed = errors.New("the command failed; retry or ask the bot owner to check status")
)

type InteractionNetwork interface {
	Respond(context.Context, *discord.InteractionEvent, api.InteractionResponse) error
	Edit(context.Context, *discord.InteractionEvent, json.RawMessage) error
	GuildPermissions(context.Context, string, string) (discord.Permissions, error)
}
type HandlerOptions struct {
	MaxInFlight, BusyResponders, DedupeEntries          int
	AckTimeout, ReplyTimeout, CommandTimeout, DedupeTTL time.Duration
	Now                                                 func() time.Time
	Logger                                              *slog.Logger
}
type seenInteraction struct {
	at     time.Time
	active bool
}
type Handler struct {
	service       *Service
	network       InteractionNetwork
	options       HandlerOptions
	mu            sync.Mutex
	ready, closed bool
	slots, busy   chan struct{}
	seen          map[discord.InteractionID]seenInteraction
	group         sync.WaitGroup
}

func NewHandler(service *Service, network InteractionNetwork, options HandlerOptions) (*Handler, error) {
	if service == nil || network == nil {
		return nil, errors.New("interaction handler requires command service and Discord transport")
	}
	if options.MaxInFlight == 0 {
		options.MaxInFlight = 4
	}
	if options.BusyResponders == 0 {
		options.BusyResponders = 2
	}
	if options.DedupeEntries == 0 {
		options.DedupeEntries = 256
	}
	if options.AckTimeout == 0 {
		options.AckTimeout = 2 * time.Second
	}
	if options.ReplyTimeout == 0 {
		options.ReplyTimeout = 5 * time.Second
	}
	if options.CommandTimeout == 0 {
		options.CommandTimeout = 6 * time.Minute
	}
	if options.DedupeTTL == 0 {
		options.DedupeTTL = 15 * time.Minute
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.MaxInFlight < 1 || options.MaxInFlight > 16 || options.BusyResponders < 1 || options.BusyResponders > 16 || options.DedupeEntries < options.MaxInFlight || options.DedupeEntries > 4096 || options.AckTimeout <= 0 || options.AckTimeout >= 3*time.Second || options.ReplyTimeout <= 0 || options.CommandTimeout <= 0 || options.CommandTimeout > 10*time.Minute || options.DedupeTTL <= 0 {
		return nil, errors.New("unsafe interaction concurrency, cache, or deadline limits")
	}
	return &Handler{service: service, network: network, options: options, slots: make(chan struct{}, options.MaxInFlight), busy: make(chan struct{}, options.BusyResponders), seen: map[discord.InteractionID]seenInteraction{}}, nil
}
func (h *Handler) SetReady(ready bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.ready = ready
	}
}
func (h *Handler) Close() { h.mu.Lock(); defer h.mu.Unlock(); h.ready = false; h.closed = true }
func (h *Handler) Wait()  { h.group.Wait() }

// Handle is the synchronous entry point. Gateway dispatch uses the same logic
// under bounded admission, not Arikawa's unbounded asynchronous event handlers.
func (h *Handler) Handle(ctx context.Context, event *discord.InteractionEvent) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	input, err := h.request(event)
	if err != nil {
		if h.ours(event) {
			_ = h.reject(ctx, event, err)
		}
		return err
	}
	h.mu.Lock()
	duplicate, err := h.admit(event.ID)
	h.mu.Unlock()
	if duplicate {
		return nil
	}
	if err != nil {
		_ = h.reject(ctx, event, err)
		return err
	}
	return h.run(ctx, event, input)
}

// Dispatch returns immediately. Admission is bounded by command slots and a
// separate small overload-response allowance. Further floods are dropped rather
// than allocating unbounded goroutines or a queue that cannot meet the 3s ACK.
func (h *Handler) Dispatch(ctx context.Context, event *discord.InteractionEvent) bool {
	if ctx.Err() != nil || !h.ours(event) {
		return false
	}
	copy := *event
	event = &copy
	input, err := h.request(event)
	if err == nil {
		h.mu.Lock()
		duplicate, admitErr := h.admit(event.ID)
		h.mu.Unlock()
		if duplicate {
			return false
		}
		err = admitErr
	}
	if err != nil {
		h.dispatchReject(ctx, event, err)
		return false
	}
	go func() { _ = h.run(ctx, event, input) }()
	return true
}
func (h *Handler) ours(event *discord.InteractionEvent) bool {
	return validInteraction(event) && event.AppID.String() == h.service.options.Identity.ApplicationID
}

func (h *Handler) request(event *discord.InteractionEvent) (Request, error) {
	if !h.ours(event) || event.GuildID == 0 || event.Member == nil || event.Member.User.ID == 0 || event.User != nil {
		return Request{}, ErrInvalidCommand
	}
	command, ok := event.Data.(*discord.CommandInteraction)
	if !ok || command == nil || (command.GuildID != 0 && command.GuildID != event.GuildID) {
		return Request{}, ErrInvalidCommand
	}
	channel := event.ChannelID
	if channel == 0 && event.Channel != nil {
		channel = event.Channel.ID
	}
	if channel == 0 {
		return Request{}, ErrInvalidCommand
	}
	path := []string{command.Name}
	options := command.Options
	if command.Name == "anilist" || command.Name == "mal" {
		if len(options) != 1 || options[0].Type != discord.SubcommandGroupOptionType {
			return Request{}, ErrInvalidCommand
		}
		path = append(path, options[0].Name)
		options = options[0].Options
		if len(options) != 1 || options[0].Type != discord.SubcommandOptionType {
			return Request{}, ErrInvalidCommand
		}
		path = append(path, options[0].Name)
		options = options[0].Options
	} else if command.Name == "sm" || command.Name == "admin" {
		if len(options) != 1 || options[0].Type != discord.SubcommandOptionType {
			return Request{}, ErrInvalidCommand
		}
		path = append(path, options[0].Name)
		options = options[0].Options
	}
	spec, ok := h.service.specs[strings.Join(path, " ")]
	if !ok || len(options) > len(spec.options) {
		return Request{}, ErrInvalidCommand
	}
	types := map[string]discord.CommandOptionType{}
	for _, field := range spec.options {
		types[field.Name()] = field.Type()
	}
	values := map[string]json.RawMessage{}
	bytes := 0
	for _, field := range options {
		if types[field.Name] != field.Type || len(field.Options) != 0 || values[field.Name] != nil {
			return Request{}, ErrInvalidCommand
		}
		bytes += len(field.Value)
		if bytes > 64<<10 {
			return Request{}, ErrInvalidCommand
		}
		values[field.Name] = append(json.RawMessage{}, field.Value...)
	}
	if _, err := commandValues(spec, values); err != nil {
		return Request{}, err
	}
	return Request{GuildID: event.GuildID.String(), ChannelID: channel.String(), UserID: event.Member.User.ID.String(), Path: path, Values: values}, nil
}

// admit is called with mu held; WaitGroup.Add and the shutdown gate share that
// lock so shutdown cannot race a new worker being added after Wait starts.
func (h *Handler) admit(id discord.InteractionID) (bool, error) {
	if h.closed || !h.ready {
		return false, ErrStarting
	}
	now := h.options.Now()
	for id, item := range h.seen {
		if !item.active && now.Sub(item.at) >= h.options.DedupeTTL {
			delete(h.seen, id)
		}
	}
	if _, ok := h.seen[id]; ok {
		return true, nil
	}
	select {
	case h.slots <- struct{}{}:
	default:
		return false, ErrCommandBusy
	}
	if len(h.seen) >= h.options.DedupeEntries {
		var oldest discord.InteractionID
		var at time.Time
		for id, item := range h.seen {
			if !item.active && (oldest == 0 || item.at.Before(at)) {
				oldest, at = id, item.at
			}
		}
		if oldest != 0 {
			delete(h.seen, oldest)
		}
	}
	h.seen[id] = seenInteraction{at: now, active: true}
	h.group.Add(1)
	return false, nil
}

func (h *Handler) run(parent context.Context, event *discord.InteractionEvent, input Request) (returned error) {
	retain := true
	defer func() {
		defer h.group.Done()
		h.mu.Lock()
		if retain {
			h.seen[event.ID] = seenInteraction{at: h.options.Now()}
		} else {
			delete(h.seen, event.ID)
		}
		<-h.slots
		h.mu.Unlock()
		if returned != nil {
			h.options.Logger.Warn("interaction failed", "interaction_id", event.ID.String(), "reason", errorCode(returned))
		}
	}()
	ack, cancelAck := context.WithTimeout(parent, h.options.AckTimeout)
	err := h.network.Respond(ack, event, api.InteractionResponse{Type: api.DeferredMessageInteractionWithSource, Data: &api.InteractionResponseData{Flags: discord.EphemeralMessage}})
	cancelAck()
	if err != nil {
		retain = false
		return safeError(err)
	}
	// ACK and command work have separate budgets. Neither detaches from parent
	// shutdown, and the operation budget starts only after a successful ACK.
	ctx, cancel := context.WithTimeout(parent, h.options.CommandTimeout)
	defer cancel()
	spec := h.service.specs[strings.Join(input.Path, " ")]
	if spec.authority == managerCommand && !slices.Contains(h.service.options.Identity.OwnerIDs, input.UserID) {
		input.Permissions, err = h.network.GuildPermissions(ctx, input.GuildID, input.UserID)
	}
	var result Result
	if err == nil {
		result, err = h.service.Execute(ctx, input)
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	if err != nil {
		err = safeError(err)
		result, _ = embedResult(notification.Embed{Title: "Unable to complete command", Description: errorText(err), Color: 0xee5a5a})
	}
	// A command's own timeout must still get a short private timeout reply. Parent
	// shutdown cancellation is not detached and stops outgoing response work.
	reply, cancelReply := context.WithTimeout(parent, h.options.ReplyTimeout)
	defer cancelReply()
	if editErr := h.network.Edit(reply, event, result.Payload); editErr != nil {
		return safeError(editErr)
	}
	if err == nil && result.Stop {
		h.service.Shutdown()
	}
	return err
}

func (h *Handler) reject(ctx context.Context, event *discord.InteractionEvent, reason error) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrStarting
	}
	select {
	case h.busy <- struct{}{}:
		h.group.Add(1)
	default:
		h.mu.Unlock()
		return ErrCommandBusy
	}
	h.mu.Unlock()
	defer func() { <-h.busy; h.group.Done() }()
	return h.sendRejection(ctx, event, reason)
}
func (h *Handler) dispatchReject(ctx context.Context, event *discord.InteractionEvent, reason error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	select {
	case h.busy <- struct{}{}:
		h.group.Add(1)
	default:
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	go func() {
		defer func() { <-h.busy; h.group.Done() }()
		if err := h.sendRejection(ctx, event, reason); err != nil {
			h.options.Logger.Warn("interaction rejection failed", "reason", errorCode(err))
		}
	}()
}
func (h *Handler) sendRejection(parent context.Context, event *discord.InteractionEvent, reason error) error {
	ctx, cancel := context.WithTimeout(parent, h.options.AckTimeout)
	defer cancel()
	return safeError(h.network.Respond(ctx, event, api.InteractionResponse{Type: api.MessageInteractionWithSource, Data: &api.InteractionResponseData{Flags: discord.EphemeralMessage, Content: option.NewNullableString(errorText(reason))}}))
}

func safeError(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, ErrStarting, ErrCommandBusy, ErrForbidden, ErrConfirmation, ErrInvalidCommand, ErrMissingUsername, ErrProviderDisabled, ErrDiscordAuth, ErrDiscordNotFound, ErrDiscordMalformed, ErrDiscordUnavailable, ErrDiscordRateLimited, ErrGatewaySessionLimit, ErrGatewayConfiguration, ErrChannelType, ErrChannelPermissions, store.ErrGuildNotConfigured, store.ErrGuildAlreadyConfigured, store.ErrUnchangedChannel, store.ErrMembershipNotFound, store.ErrAccountConflict, store.ErrAccountNotFound, store.ErrStalePoll, store.ErrBackpressure, anilist.ErrRateLimited, mal.ErrRateLimited, mal.ErrUnauthorized, mal.ErrRenameRecovery, mal.ErrIncompleteFavorites} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrCommandFailed
}
func errorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrForbidden):
		return "forbidden"
	case errors.Is(err, ErrCommandBusy):
		return "busy"
	case errors.Is(err, ErrStarting):
		return "starting"
	case errors.Is(err, store.ErrStalePoll):
		return "stale_poll"
	case errors.Is(err, store.ErrBackpressure):
		return "backpressure"
	case errors.Is(err, anilist.ErrRateLimited) || errors.Is(err, mal.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, mal.ErrRenameRecovery):
		return "rename_recovery"
	case errors.Is(err, ErrDiscordAuth) || errors.Is(err, mal.ErrUnauthorized):
		return "authentication"
	default:
		return "command_failed"
	}
}
func errorText(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "The operation timed out. Check status before retrying; an already committed change may still have succeeded."
	case errors.Is(err, store.ErrGuildNotConfigured):
		return "Ask a server manager to configure a posting channel with /sm set first."
	case errors.Is(err, anilist.ErrRateLimited) || errors.Is(err, mal.ErrRateLimited):
		return "The provider is rate limited. Retry after its cooldown."
	case errors.Is(err, store.ErrBackpressure):
		return "The pending delivery queue is full. Ask the bot owner to check channel failures and outbox status."
	case errors.Is(err, mal.ErrUnauthorized):
		return "MAL rejected the application's client ID. Ask the bot owner to check its configuration."
	case errors.Is(err, ErrInvalidCommand):
		return "Use these slash commands in a server, with the current supported options. Old controls are not active."
	default:
		return safeError(err).Error()
	}
}
