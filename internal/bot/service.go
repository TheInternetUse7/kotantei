package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/diamondburned/arikawa/v3/discord"

	"kotantei/internal/buildinfo"
	"kotantei/internal/notification"
	"kotantei/internal/scheduler"
	"kotantei/internal/store"
	"kotantei/internal/update"
)

var (
	ErrForbidden        = errors.New("this command requires server-management or bot-owner permission")
	ErrConfirmation     = errors.New("set confirm to true to perform this destructive action")
	ErrMissingUsername  = errors.New("provide a public provider username when linking a new account")
	ErrProviderDisabled = errors.New("this provider is disabled; ask the bot owner to enable it")
	ErrInvalidCommand   = errors.New("invalid or unsupported command options; run the slash command again")
)

type Linker interface {
	Link(context.Context, string, string, string) (store.LinkResult, error)
}
type SearchResult struct {
	Title, URL, ImageURL, Format, Status, Description string
	Score                                             float64
	Episodes, Chapters, Volumes                       int
	Genres                                            []string
}
type ProviderCommands struct {
	Linker Linker
	Search func(context.Context, string, string, string) ([]SearchResult, error)
}
type CommandNetwork interface {
	CheckChannel(context.Context, string, string, string) error
	Send(context.Context, store.Delivery) (string, error)
}
type ServiceOptions struct {
	SourceURL string
	Identity  Identity
	Providers map[string]ProviderCommands
	Scheduler *scheduler.Scheduler
	Shutdown  func()
	Logger    *slog.Logger
}
type Service struct {
	queue   *store.Outbox
	network CommandNetwork
	options ServiceOptions
	specs   map[string]leafSpec
}
type Request struct {
	GuildID, ChannelID, UserID string
	Permissions                discord.Permissions
	Path                       []string
	Values                     map[string]json.RawMessage
}
type Result struct {
	Payload json.RawMessage
	Stop    bool
}

func NewService(queue *store.Outbox, network CommandNetwork, options ServiceOptions) (*Service, error) {
	if queue == nil || network == nil || !numericID(options.Identity.ApplicationID) || !numericID(options.Identity.UserID) {
		return nil, errors.New("commands require storage, Discord transport, and a verified bot identity")
	}
	if options.SourceURL == "" {
		options.SourceURL = buildinfo.Current().SourceURL
	}
	if options.SourceURL != "" && notification.SafeURL(options.SourceURL) != options.SourceURL {
		return nil, errors.New("command source offer must be a bounded HTTPS URL without credentials")
	}
	options.Identity.OwnerIDs = slices.Clone(options.Identity.OwnerIDs)
	for _, id := range options.Identity.OwnerIDs {
		if !numericID(id) {
			return nil, errors.New("invalid bot owner ID")
		}
	}
	providers := make(map[string]ProviderCommands, len(options.Providers))
	for name, provider := range options.Providers {
		if name != "anilist" && name != "mal" {
			return nil, errors.New("invalid command provider")
		}
		providers[name] = provider
	}
	options.Providers = providers
	service := &Service{queue: queue, network: network, options: options, specs: map[string]leafSpec{}}
	for _, spec := range specifications() {
		service.specs[spec.path] = spec
	}
	return service, nil
}

func (s *Service) Execute(ctx context.Context, input Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if !numericID(input.GuildID) || !numericID(input.ChannelID) || !numericID(input.UserID) {
		return Result{}, ErrInvalidCommand
	}
	path := strings.Join(input.Path, " ")
	spec, ok := s.specs[path]
	if !ok {
		return Result{}, ErrInvalidCommand
	}
	values, err := commandValues(spec, input.Values)
	if err != nil {
		return Result{}, err
	}
	owner := slices.Contains(s.options.Identity.OwnerIDs, input.UserID)
	if spec.authority == ownerCommand && !owner {
		return Result{}, ErrForbidden
	}
	if spec.authority == managerCommand && !owner && !input.Permissions.Has(discord.PermissionManageGuild) && !input.Permissions.Has(discord.PermissionAdministrator) {
		return Result{}, ErrForbidden
	}
	if confirmed, ok := values["confirm"]; ok && confirmed != true {
		return Result{}, ErrConfirmation
	}
	if input.Path[0] == "anilist" || input.Path[0] == "mal" {
		return s.providerCommand(ctx, input, values)
	}
	switch path {
	case "sm set", "sm update":
		channel := input.ChannelID
		if selected, ok := values["channel"].(string); ok {
			channel = selected
		}
		if err := s.network.CheckChannel(ctx, input.GuildID, channel, s.options.Identity.UserID); err != nil {
			return Result{}, err
		}
		if err := s.queue.ConfigureGuild(ctx, input.GuildID, channel, path == "sm update"); err != nil {
			return Result{}, err
		}
		return success("Configured posting channel " + channel + ". Previously queued posts are not moved or replayed.")
	case "sm removeserver":
		if err := s.queue.RemoveGuild(ctx, input.GuildID); err != nil {
			return Result{}, err
		}
		return success("Stopped tracking this server. Provider accounts in other servers are unchanged.")
	case "sm forceremoveuser", "sm forceremoveuserbyid":
		key := "user"
		if path == "sm forceremoveuserbyid" {
			key = "userid"
		}
		user := values[key].(string)
		if !numericID(user) {
			return Result{}, ErrInvalidCommand
		}
		if err := s.queue.DisconnectUser(ctx, user, input.GuildID); err != nil {
			return Result{}, err
		}
		return success("Stopped both AniList and MAL updates for this user in this server only.")
	case "sm status", "status":
		return s.status(ctx, input.GuildID)
	case "sm resume":
		status, err := s.queue.GuildStatus(ctx, input.GuildID)
		if err != nil {
			return Result{}, err
		}
		if err := s.network.CheckChannel(ctx, input.GuildID, status.ChannelID, s.options.Identity.UserID); err != nil {
			return Result{}, err
		}
		count, err := s.queue.ResumeChannel(ctx, status.ChannelID, time.Now().UTC())
		if err != nil {
			return Result{}, err
		}
		return success(fmt.Sprintf("Requeued %d unconfirmed posts for this server's configured channel. Acknowledged posts are not replayed.", count))
	case "admin check":
		if s.options.Scheduler == nil {
			return Result{}, ErrProviderDisabled
		}
		if err := s.options.Scheduler.Trigger(values["name"].(string)); err != nil {
			return Result{}, ErrProviderDisabled
		}
		return success("Queued a provider cycle. Any current cycle finishes first; repeated requests are coalesced.")
	case "admin restart":
		if s.options.Shutdown == nil {
			return Result{}, ErrInvalidCommand
		}
		result, err := success("Shutting down gracefully. An external supervisor may restart the bot.")
		result.Stop = true
		return result, err
	case "admin cleanup":
		count, err := s.queue.Cleanup(ctx, 100)
		if err != nil {
			return Result{}, err
		}
		return success(fmt.Sprintf("Removed %d users with no server memberships, including their provider accounts and queued posts. Run again if more remain.", count))
	case "admin forcetoleave":
		guild := values["guildid"].(string)
		if !numericID(guild) {
			return Result{}, ErrInvalidCommand
		}
		if err := s.queue.RemoveGuild(ctx, guild); err != nil {
			return Result{}, err
		}
		return success("Removed that server's tracking configuration. The bot remains a Discord member.")
	case "say":
		channel := values["channel"].(string)
		if err := s.network.CheckChannel(ctx, input.GuildID, channel, s.options.Identity.UserID); err != nil {
			return Result{}, err
		}
		payload, err := notification.Payload(notification.Embed{Description: values["text"].(string), Color: 0x2e51a2}, "")
		if err != nil {
			return Result{}, err
		}
		if _, err := s.network.Send(ctx, store.Delivery{ChannelID: channel, UserID: input.UserID, Event: update.Event{Payload: payload}}); err != nil {
			return Result{}, err
		}
		return success("Posted the embed with all mentions disabled.")
	case "about":
		info := buildinfo.Current()
		const repository = "https://github.com/theinternetuse7/Kotantei"
		embed := notification.Embed{
			Title: "About Kotantei", URL: repository,
			Description: "A Go bot for public AniList and MyAnimeList updates in Discord. Licensed [AGPLv3](" + repository + ").",
			Color:       0x2e51a2,
			Fields:      []notification.Field{{Name: "Version", Value: info.Version}},
		}
		if s.options.SourceURL != "" {
			embed.URL = s.options.SourceURL
			embed.Fields = append(embed.Fields, notification.Field{Name: "Source", Value: "Download this build's source from the title link."})
		}
		return embedResult(embed)
	}
	return Result{}, ErrInvalidCommand
}

func (s *Service) providerCommand(ctx context.Context, input Request, values map[string]any) (Result, error) {
	provider, group, action := input.Path[0], input.Path[1], input.Path[2]
	if group == "user" {
		switch action {
		case "add":
			state, err := s.queue.Settings(ctx, input.UserID, provider)
			if err == nil {
				if _, err := s.queue.Connect(ctx, store.AccountLink{Provider: provider, UserID: input.UserID, GuildID: input.GuildID, ExternalID: state.ExternalID, Username: state.Username}); err != nil {
					return Result{}, err
				}
				return success("Reconnected your existing account here without resetting settings, checkpoints, favorites, or pending deliveries.")
			}
			if !errors.Is(err, store.ErrAccountNotFound) {
				return Result{}, err
			}
			username, _ := values["username"].(string)
			if strings.TrimSpace(username) == "" {
				return Result{}, ErrMissingUsername
			}
			adapter := s.options.Providers[provider]
			if adapter.Linker == nil {
				return Result{}, ErrProviderDisabled
			}
			if _, err := adapter.Linker.Link(ctx, input.UserID, input.GuildID, username); err != nil {
				return Result{}, err
			}
			return success("Linked your public account with quiet initial baselines; no historical notifications were queued.")
		case "remove":
			if err := s.queue.RemoveAccount(ctx, input.UserID, provider); err != nil {
				return Result{}, err
			}
			return success("Globally unlinked your " + provider + " account in every server. Other provider accounts and shared memberships are unchanged.")
		case "removehere":
			if err := s.queue.DisconnectUser(ctx, input.UserID, input.GuildID); err != nil {
				return Result{}, err
			}
			return success("Stopped BOTH AniList and MAL updates here. Account baselines and subscriptions in other servers are unchanged.")
		case "list":
			page := int64(1)
			if number, ok := values["page"].(int64); ok {
				page = number
			}
			accounts, more, err := s.queue.ListAccounts(ctx, input.GuildID, provider, int(page-1)*25, 25)
			if err != nil {
				return Result{}, err
			}
			var lines []string
			for _, account := range accounts {
				name := account.Username
				if name == "" {
					name = "Account " + account.ExternalID
				}
				lines = append(lines, fmt.Sprintf("%s — <@%s>", cleanText(name), account.UserID))
			}
			if len(lines) == 0 {
				lines = append(lines, "No tracked accounts on this page.")
			}
			if more {
				lines = append(lines, fmt.Sprintf("More accounts: use page %d.", page+1))
			}
			return embedResult(notification.Embed{Title: provider + " accounts — page " + strconv.FormatInt(page, 10), Description: strings.Join(lines, "\n"), Color: 0x2e51a2})
		}
	}
	if group == "features" {
		names := featureNames(provider)
		switch action {
		case "enable", "disable":
			flag, _ := strconv.ParseUint(values["feature"].(string), 10, 64)
			if err := s.queue.SetFeature(ctx, input.UserID, provider, flag, action == "enable"); err != nil {
				return Result{}, err
			}
			return success("Updated your " + provider + " feature setting. Checkpoints and queued payloads were not rewritten.")
		case "list", "enabled":
			mask := uint64(0)
			if action == "enabled" {
				state, err := s.queue.Settings(ctx, input.UserID, provider)
				if err != nil {
					return Result{}, err
				}
				mask = uint64(state.Features)
			}
			var lines []string
			for index, name := range names {
				if action == "list" || mask&(1<<index) != 0 {
					lines = append(lines, "- "+name)
				}
			}
			if len(lines) == 0 {
				lines = append(lines, "No supported features are enabled.")
			}
			if action == "enabled" && mask&^((1<<len(names))-1) != 0 {
				lines = append(lines, "Unrecognized imported bits are preserved.")
			}
			return embedResult(notification.Embed{Title: provider + " features: " + action, Description: strings.Join(lines, "\n"), Color: 0x2e51a2})
		}
	}
	if group == "colors" {
		switch action {
		case "set", "remove":
			kind, _ := strconv.ParseUint(values["updatetype"].(string), 10, 8)
			var color *uint32
			if action == "set" {
				raw := strings.TrimPrefix(values["color"].(string), "#")
				if len(raw) != 6 {
					return Result{}, ErrInvalidCommand
				}
				number, err := strconv.ParseUint(raw, 16, 32)
				if err != nil {
					return Result{}, ErrInvalidCommand
				}
				value := uint32(number)
				color = &value
			}
			if err := s.queue.SetColor(ctx, input.UserID, provider, uint8(kind), color); err != nil {
				return Result{}, err
			}
			return success("Updated your " + provider + " color override. Previously queued embeds keep their captured colors.")
		case "list":
			state, err := s.queue.Settings(ctx, input.UserID, provider)
			if err != nil {
				return Result{}, err
			}
			names := colorNames(provider)
			var lines []string
			for _, color := range state.Colors {
				name := fmt.Sprintf("imported type %d", color.UpdateType)
				if int(color.UpdateType) < len(names) {
					name = names[color.UpdateType]
				}
				lines = append(lines, fmt.Sprintf("%s: #%06X", name, color.ColorValue))
			}
			if len(lines) == 0 {
				lines = append(lines, "Using default update colors.")
			}
			return embedResult(notification.Embed{Title: provider + " color overrides", Description: strings.Join(lines, "\n"), Color: 0x2e51a2})
		}
	}
	if group == "search" {
		adapter := s.options.Providers[provider]
		if adapter.Search == nil {
			return Result{}, ErrProviderDisabled
		}
		query, format := values["title"], values["format"]
		if provider == "mal" {
			query, format = values["query"], values["type"]
		}
		filter, _ := format.(string)
		items, err := adapter.Search(ctx, action, strings.TrimSpace(query.(string)), filter)
		if err != nil {
			return Result{}, err
		}
		return searchReply(provider, action, items)
	}
	return Result{}, ErrInvalidCommand
}

func (s *Service) status(ctx context.Context, guildID string) (Result, error) {
	status, err := s.queue.GuildStatus(ctx, guildID)
	if err != nil && !errors.Is(err, store.ErrGuildNotConfigured) {
		return Result{}, err
	}
	text := "This server has no posting channel configured; a manager can use /sm set."
	if err == nil {
		text = fmt.Sprintf("Posting channel: %s\nPending posts: %d\nFailed posts: %d\nChannel state: %s", status.ChannelID, status.Pending, status.Failed, func() string {
			if status.BlockedReason == "" {
				return "not blocked"
			}
			return string(status.BlockedReason)
		}())
	}
	embed := notification.Embed{Title: "Kotantei status", Description: text, Color: 0x2e51a2}
	if s.options.Scheduler != nil {
		for _, provider := range s.options.Scheduler.Status() {
			value := fmt.Sprintf("Running: %t\nLast cycle: %d checked, %d failures, %d warnings\nQueued: %d events / %d deliveries", provider.Running, provider.Checked, provider.Failures, provider.Warnings, provider.Events, provider.Deliveries)
			if !provider.CompletedAt.IsZero() {
				value += "\nCompleted: " + provider.CompletedAt.Format(time.RFC3339)
			}
			if provider.LastError != "" {
				value += "\nLast error: " + provider.LastError
			}
			embed.Fields = append(embed.Fields, notification.Field{Name: provider.Name, Value: value})
		}
	}
	return embedResult(embed)
}
func (s *Service) Shutdown() {
	if s.options.Shutdown != nil {
		s.options.Shutdown()
	}
}
func success(description string) (Result, error) {
	return embedResult(notification.Embed{Title: "Success", Description: description, Color: 0x2db039})
}
func embedResult(embed notification.Embed) (Result, error) {
	payload, err := notification.Payload(embed, "")
	return Result{Payload: payload}, err
}

func commandValues(spec leafSpec, raw map[string]json.RawMessage) (map[string]any, error) {
	values := map[string]any{}
	defined := map[string]bool{}
	for _, field := range spec.options {
		name := field.Name()
		defined[name] = true
		encoded, present := raw[name]
		switch field := field.(type) {
		case *discord.StringOption:
			if !present {
				if field.Required {
					return nil, ErrInvalidCommand
				}
				continue
			}
			var value string
			if json.Unmarshal(encoded, &value) != nil || !utf8.ValidString(value) {
				return nil, ErrInvalidCommand
			}
			length := utf8.RuneCountInString(value)
			if (field.MinLength != nil && length < *field.MinLength) || (field.MaxLength != nil && length > *field.MaxLength) {
				return nil, ErrInvalidCommand
			}
			if len(field.Choices) > 0 {
				found := false
				for _, choice := range field.Choices {
					if choice.Value == value {
						found = true
					}
				}
				if !found {
					return nil, ErrInvalidCommand
				}
			}
			values[name] = value
		case *discord.BooleanOption:
			if !present {
				if field.Required {
					return nil, ErrConfirmation
				}
				continue
			}
			var value bool
			if json.Unmarshal(encoded, &value) != nil || string(encoded) == "null" {
				return nil, ErrInvalidCommand
			}
			values[name] = value
		case *discord.IntegerOption:
			if !present {
				if field.Required {
					return nil, ErrInvalidCommand
				}
				continue
			}
			var value int64
			if json.Unmarshal(encoded, &value) != nil || string(encoded) == "null" || (field.Min != nil && value < int64(*field.Min)) || (field.Max != nil && value > int64(*field.Max)) {
				return nil, ErrInvalidCommand
			}
			values[name] = value
		case *discord.ChannelOption:
			if !present {
				if field.Required {
					return nil, ErrInvalidCommand
				}
				continue
			}
			var value string
			if json.Unmarshal(encoded, &value) != nil || !numericID(value) {
				return nil, ErrInvalidCommand
			}
			values[name] = value
		case *discord.UserOption:
			if !present {
				return nil, ErrInvalidCommand
			}
			var value string
			if json.Unmarshal(encoded, &value) != nil || !numericID(value) {
				return nil, ErrInvalidCommand
			}
			values[name] = value
		default:
			return nil, ErrInvalidCommand
		}
	}
	for name := range raw {
		if !defined[name] {
			return nil, ErrInvalidCommand
		}
	}
	return values, nil
}

var botHTML = regexp.MustCompile(`<[^>]*>`)

func cleanText(value string) string {
	return strings.TrimSpace(html.UnescapeString(botHTML.ReplaceAllString(value, "")))
}
func trimText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return value
}
func markdownLabel(value string) string {
	return strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)").Replace(trimText(cleanText(value), 120))
}
func searchReply(provider, kind string, items []SearchResult) (Result, error) {
	embed := notification.Embed{Title: provider + " " + kind + " search", Color: 0x2e51a2}
	if len(items) == 0 {
		embed.Description = "No matches in this bounded search. Try another title or remove the format filter."
	}
	for _, item := range items[:min(5, len(items))] {
		var details []string
		if link := notification.SafeURL(item.URL); link != "" {
			details = append(details, "[Open "+markdownLabel(item.Title)+"]("+link+")")
		}
		if item.Format != "" {
			details = append(details, "Format: "+cleanText(item.Format))
		}
		if item.Status != "" {
			details = append(details, "Status: "+cleanText(item.Status))
		}
		if item.Score > 0 {
			details = append(details, fmt.Sprintf("Community score: %g", item.Score))
		}
		var totals []string
		if item.Episodes > 0 {
			totals = append(totals, fmt.Sprintf("%d ep.", item.Episodes))
		}
		if item.Chapters > 0 {
			totals = append(totals, fmt.Sprintf("%d ch.", item.Chapters))
		}
		if item.Volumes > 0 {
			totals = append(totals, fmt.Sprintf("%d v.", item.Volumes))
		}
		if len(totals) > 0 {
			details = append(details, strings.Join(totals, ", "))
		}
		if item.Description != "" {
			details = append(details, trimText(cleanText(item.Description), 250))
		}
		if len(details) == 0 {
			details = append(details, "Public catalogue result")
		}
		embed.Fields = append(embed.Fields, notification.Field{Name: cleanText(item.Title), Value: strings.Join(details, "\n")})
	}
	return embedResult(embed)
}
