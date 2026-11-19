package bot

import (
	"strconv"
	"strings"

	"github.com/diamondburned/arikawa/v3/api"
	"github.com/diamondburned/arikawa/v3/discord"
	"github.com/diamondburned/arikawa/v3/utils/json/option"
)

type authority uint8

const (
	selfCommand authority = iota
	managerCommand
	ownerCommand
)

type leafSpec struct {
	path, description string
	authority         authority
	options           []discord.CommandOptionValue
}

func featureNames(provider string) []string {
	if provider == "mal" {
		return []string{"animelist", "mangalist", "favorites", "mention", "website", "media format", "media status", "genres", "synopsis", "studios", "mangakas", "tags", "comments", "dates", "themes", "demographic", "seiyu"}
	}
	return []string{"animelist", "mangalist", "favourites", "mention", "website", "media format", "media status", "description", "genres", "tags", "studio", "mangaka", "reviews", "custom lists", "director", "seyu"}
}
func colorNames(provider string) []string {
	names := []string{"watching", "plan to watch", "completed anime", "dropped anime", "on-hold anime", "rewatching anime", "reading", "plan to read", "completed manga", "dropped manga", "on-hold manga", "rereading manga", "favorite added", "favorite removed"}
	if provider == "anilist" {
		names = append(names, "new review")
	}
	return names
}
func stringValue(name, description string, required bool, max int) *discord.StringOption {
	return &discord.StringOption{OptionName: name, Description: description, Required: required, MinLength: option.NewInt(1), MaxLength: option.NewInt(max)}
}
func confirmOption() *discord.BooleanOption {
	return &discord.BooleanOption{OptionName: "confirm", Description: "Confirm the stated destructive scope", Required: true}
}
func channelOption(required bool) *discord.ChannelOption {
	return &discord.ChannelOption{OptionName: "channel", Description: "Posting channel in this server", Required: required, ChannelTypes: []discord.ChannelType{discord.GuildText, discord.GuildNews}}
}
func pageOption() *discord.IntegerOption {
	return &discord.IntegerOption{OptionName: "page", Description: "Page of tracked accounts (25 per page)", Min: option.NewInt(1), Max: option.NewInt(1000)}
}
func choiceOption(name, description string, values []string, required bool) *discord.StringOption {
	result := stringValue(name, description, required, 64)
	for _, value := range values {
		result.Choices = append(result.Choices, discord.StringChoice{Name: value, Value: value})
	}
	return result
}

func specifications() []leafSpec {
	var result []leafSpec
	for _, provider := range []string{"anilist", "mal"} {
		feature := stringValue("feature", "Feature to change", true, 16)
		for index, name := range featureNames(provider) {
			feature.Choices = append(feature.Choices, discord.StringChoice{Name: name, Value: strconv.FormatUint(1<<index, 10)})
		}
		color := stringValue("updatetype", "Type of update", true, 2)
		for index, name := range colorNames(provider) {
			color.Choices = append(color.Choices, discord.StringChoice{Name: name, Value: strconv.Itoa(index)})
		}
		result = append(result,
			leafSpec{provider + " user add", "Quietly link your public account or reconnect an existing link", selfCommand, []discord.CommandOptionValue{stringValue("username", "Required for a new public account link", false, 64)}},
			leafSpec{provider + " user remove", "Globally unlink only this provider account, in every server", selfCommand, []discord.CommandOptionValue{confirmOption()}},
			leafSpec{provider + " user list", "List tracked accounts in this server", selfCommand, []discord.CommandOptionValue{pageOption()}},
			leafSpec{provider + " user removehere", "Stop BOTH AniList and MAL updates for you in this server", selfCommand, []discord.CommandOptionValue{confirmOption()}},
			leafSpec{provider + " features enable", "Enable one of your update features", selfCommand, []discord.CommandOptionValue{feature}},
			leafSpec{provider + " features disable", "Disable one of your update features", selfCommand, []discord.CommandOptionValue{feature}},
			leafSpec{provider + " features enabled", "Show your enabled update features", selfCommand, nil},
			leafSpec{provider + " features list", "List the supported update features", selfCommand, nil},
			leafSpec{provider + " colors set", "Set one of your update colors", selfCommand, []discord.CommandOptionValue{color, stringValue("color", "Six hexadecimal digits, optionally prefixed with #", true, 7)}},
			leafSpec{provider + " colors remove", "Restore the default color for one update type", selfCommand, []discord.CommandOptionValue{color}},
			leafSpec{provider + " colors list", "List your update color overrides", selfCommand, nil},
		)
		for _, kind := range []string{"anime", "manga"} {
			queryName, formatName := "title", "format"
			formats := []string{"TV", "TV_SHORT", "MOVIE", "SPECIAL", "OVA", "ONA", "MUSIC"}
			if kind == "manga" {
				formats = []string{"MANGA", "NOVEL", "ONE_SHOT"}
			}
			if provider == "mal" {
				queryName, formatName = "query", "type"
				formats = []string{"tv", "movie", "ova", "ona", "special", "music", "tv_special"}
				if kind == "manga" {
					formats = []string{"manga", "novel", "one_shot", "doujinshi", "manhwa", "manhua", "oel", "light_novel"}
				}
			}
			result = append(result, leafSpec{provider + " search " + kind, "Search the public catalogue; return up to five results", selfCommand, []discord.CommandOptionValue{stringValue(queryName, "Title to search for", true, 200), choiceOption(formatName, "Optional media format filter", formats, false)}})
		}
	}
	result = append(result,
		leafSpec{"sm set", "Configure this server's first posting channel", managerCommand, []discord.CommandOptionValue{channelOption(false)}},
		leafSpec{"sm update", "Change the posting channel; old pending posts are not moved", managerCommand, []discord.CommandOptionValue{channelOption(false)}},
		leafSpec{"sm status", "Show this server's delivery and channel status", managerCommand, nil},
		leafSpec{"sm resume", "After repairing permissions, requeue this channel's failed posts", managerCommand, nil},
		leafSpec{"sm removeserver", "Remove this server's tracking configuration, not global accounts", managerCommand, []discord.CommandOptionValue{confirmOption()}},
		leafSpec{"sm forceremoveuser", "Stop both providers for a user in this server", managerCommand, []discord.CommandOptionValue{&discord.UserOption{OptionName: "user", Description: "Discord user to stop tracking here", Required: true}, confirmOption()}},
		leafSpec{"sm forceremoveuserbyid", "Stop both providers for a Discord user ID in this server", managerCommand, []discord.CommandOptionValue{stringValue("userid", "Full Discord user ID", true, 20), confirmOption()}},
		leafSpec{"admin check", "Queue a provider cycle without overlapping its current work", ownerCommand, []discord.CommandOptionValue{choiceOption("name", "Enabled provider to check", []string{"anilist", "mal"}, true)}},
		leafSpec{"admin restart", "Exit gracefully; an external supervisor may restart the bot", ownerCommand, []discord.CommandOptionValue{confirmOption()}},
		leafSpec{"admin cleanup", "Delete up to 100 users with no server memberships and their accounts", ownerCommand, []discord.CommandOptionValue{confirmOption()}},
		leafSpec{"admin forcetoleave", "Remove a server's tracking configuration; do not leave Discord", ownerCommand, []discord.CommandOptionValue{stringValue("guildid", "Full Discord server ID", true, 20), confirmOption()}},
		leafSpec{"about", "About Kotantei and its independent implementation", selfCommand, nil},
		leafSpec{"status", "Show provider health and this server's delivery status", selfCommand, nil},
		leafSpec{"say", "Send a mention-disabled text embed to a channel in this server", managerCommand, []discord.CommandOptionValue{channelOption(true), stringValue("text", "Text to post, with all mentions disabled", true, 2000)}},
	)
	return result
}

// Commands returns independent registration data. Runtime authorization remains
// mandatory: Discord's command visibility/default permissions are not a guard.
func Commands() []api.CreateCommandData {
	descriptions := map[string]string{"anilist": "Public AniList tracking, settings, and catalogue search", "mal": "Public MyAnimeList tracking, settings, and catalogue search", "sm": "Server tracking and posting-channel management", "admin": "Bot-owner maintenance commands", "about": "About Kotantei", "status": "Provider and delivery health", "say": "Send a mention-disabled text embed"}
	order := []string{"anilist", "mal", "sm", "admin", "about", "status", "say"}
	roots := map[string]*api.CreateCommandData{}
	groups := map[string]*discord.SubcommandGroupOption{}
	for _, name := range order {
		command := &api.CreateCommandData{Name: name, Description: descriptions[name], Type: discord.ChatInputCommand, NoDMPermission: true}
		if name == "sm" || name == "say" {
			command.DefaultMemberPermissions = discord.NewPermissions(discord.PermissionManageGuild)
		} else if name == "admin" {
			command.DefaultMemberPermissions = discord.NewPermissions(discord.PermissionAdministrator)
		}
		roots[name] = command
	}
	for _, spec := range specifications() {
		parts := strings.Fields(spec.path)
		root := roots[parts[0]]
		if len(parts) == 1 {
			for _, value := range spec.options {
				root.Options = append(root.Options, value)
			}
			continue
		}
		sub := &discord.SubcommandOption{OptionName: parts[len(parts)-1], Description: spec.description, Options: spec.options}
		if len(parts) == 2 {
			root.Options = append(root.Options, sub)
		} else {
			key := parts[0] + " " + parts[1]
			group := groups[key]
			if group == nil {
				group = &discord.SubcommandGroupOption{OptionName: parts[1], Description: "Manage " + parts[1] + " for " + parts[0]}
				groups[key] = group
				root.Options = append(root.Options, group)
			}
			group.Subcommands = append(group.Subcommands, sub)
		}
	}
	result := make([]api.CreateCommandData, 0, len(order))
	for _, name := range order {
		result = append(result, *roots[name])
	}
	return result
}
