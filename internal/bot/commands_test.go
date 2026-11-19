package bot

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/diamondburned/arikawa/v3/api"
	"github.com/diamondburned/arikawa/v3/discord"
)

func TestCommandRegistryPreservesSupportedGroupsAndDisablesDMs(t *testing.T) {
	commands := Commands()
	roots := map[string]bool{}
	validName := regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	for _, command := range commands {
		if !command.NoDMPermission || !validName.MatchString(command.Name) || roots[command.Name] || len(command.Description) > 100 || len(command.Options) > 25 {
			t.Fatal("unsafe command registration", command)
		}
		roots[command.Name] = true
		if _, err := json.Marshal(command); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"anilist", "mal", "sm", "admin", "about", "status", "say"} {
		if !roots[name] {
			t.Fatal("missing supported command", name)
		}
	}
	for _, name := range []string{"anilist", "mal"} {
		command := commandNamed(t, commands, name)
		groups := map[string]bool{}
		for _, option := range command.Options {
			group, ok := option.(*discord.SubcommandGroupOption)
			if !ok {
				t.Fatal("provider commands were not grouped")
			}
			groups[group.OptionName] = true
			for _, sub := range group.Subcommands {
				if !validName.MatchString(sub.OptionName) || len(sub.Options) > 25 {
					t.Fatal("invalid subcommand")
				}
			}
		}
		for _, group := range []string{"user", "features", "colors", "search"} {
			if !groups[group] {
				t.Fatal("missing provider group", name, group)
			}
		}
	}
}

func commandNamed(t *testing.T, commands []api.CreateCommandData, name string) api.CreateCommandData {
	t.Helper()
	for _, command := range commands {
		if command.Name == name {
			return command
		}
	}
	t.Fatal("command missing", name)
	return api.CreateCommandData{}
}
func subNamed(t *testing.T, root, group, name string) *discord.SubcommandOption {
	t.Helper()
	command := commandNamed(t, Commands(), root)
	for _, option := range command.Options {
		if group == "" {
			if sub, ok := option.(*discord.SubcommandOption); ok && sub.OptionName == name {
				return sub
			}
		} else if g, ok := option.(*discord.SubcommandGroupOption); ok && g.OptionName == group {
			for _, sub := range g.Subcommands {
				if sub.OptionName == name {
					return sub
				}
			}
		}
	}
	t.Fatal("subcommand missing", root, group, name)
	return nil
}

func TestFeatureAndColorChoicesKeepProviderSpecificImportedValues(t *testing.T) {
	for _, provider := range []string{"anilist", "mal"} {
		sub := subNamed(t, provider, "features", "enable")
		feature, ok := sub.Options[0].(*discord.StringOption)
		if !ok {
			t.Fatal("feature option not a bounded string choice")
		}
		expected := 16
		if provider == "mal" {
			expected = 17
		}
		if len(feature.Choices) != expected {
			t.Fatal("missing supported feature choices", provider)
		}
		found := false
		for _, choice := range feature.Choices {
			if choice.Value == "4096" {
				found = true
				if (provider == "anilist" && choice.Name != "reviews") || (provider == "mal" && choice.Name != "comments") {
					t.Fatal("review and comment bits were confused", choice)
				}
			}
		}
		if !found {
			t.Fatal("legacy feature value missing")
		}
		sub = subNamed(t, provider, "colors", "set")
		color := sub.Options[0].(*discord.StringOption)
		expected = 15
		if provider == "mal" {
			expected = 14
		}
		if len(color.Choices) != expected {
			t.Fatal("imported color types missing", provider)
		}
	}
}

func TestDestructiveCommandsRequireExplicitConfirmationAndManagerDefaults(t *testing.T) {
	for _, path := range [][3]string{{"anilist", "user", "remove"}, {"mal", "user", "removehere"}, {"sm", "", "removeserver"}, {"sm", "", "forceremoveuser"}, {"admin", "", "restart"}, {"admin", "", "cleanup"}} {
		sub := subNamed(t, path[0], path[1], path[2])
		found := false
		for _, option := range sub.Options {
			if boolean, ok := option.(*discord.BooleanOption); ok && boolean.OptionName == "confirm" && boolean.Required {
				found = true
			}
		}
		if !found {
			t.Fatal("destructive command lacks explicit confirmation", path)
		}
	}
	manager := commandNamed(t, Commands(), "sm")
	if manager.DefaultMemberPermissions == nil || !manager.DefaultMemberPermissions.Has(discord.PermissionManageGuild) {
		t.Fatal("manager commands are unrestricted by default")
	}
}
