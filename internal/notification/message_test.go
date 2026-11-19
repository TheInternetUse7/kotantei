package notification

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf16"
)

func units(s string) int { return len(utf16.Encode([]rune(s))) }
func TestPayloadEnforcesDiscordUTF16AndAggregateEmbedLimits(t *testing.T) {
	embed := Embed{Title: strings.Repeat("😀", 200), Description: strings.Repeat("Text", 1500), Author: &Author{Name: strings.Repeat("A", 300)}, Footer: &Footer{Text: strings.Repeat("F", 2100)}}
	for range 40 {
		embed.Fields = append(embed.Fields, Field{Name: strings.Repeat("N", 300), Value: strings.Repeat("😀", 600)})
	}
	raw, err := Payload(embed, "")
	if err != nil {
		t.Fatal(err)
	}
	var result Message
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Embeds) != 1 {
		t.Fatal("missing embed")
	}
	got := result.Embeds[0]
	if units(got.Title) > 256 || units(got.Description) > 4096 || len(got.Fields) > 25 || units(got.Author.Name) > 256 || (got.Footer != nil && units(got.Footer.Text) > 2048) {
		t.Fatal("individual Discord limit exceeded")
	}
	total := units(got.Title) + units(got.Description) + units(got.Author.Name)
	if got.Footer != nil {
		total += units(got.Footer.Text)
	}
	for _, field := range got.Fields {
		if units(field.Name) > 256 || units(field.Value) > 1024 {
			t.Fatal("field limit exceeded")
		}
		total += units(field.Name) + units(field.Value)
	}
	if total > 6000 {
		t.Fatal("aggregate Discord limit exceeded")
	}
	if embed.Title != strings.Repeat("😀", 200) {
		t.Fatal("encoding changed caller-owned data")
	}
}

func TestPayloadAllowsOnlyTheRequestedDiscordUserMention(t *testing.T) {
	raw, err := Payload(Embed{Title: "@everyone <@2> <@&3>"}, "1")
	if err != nil {
		t.Fatal(err)
	}
	var result Message
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.AllowedMentions.Parse) != 0 || len(result.AllowedMentions.Users) != 1 || result.AllowedMentions.Users[0] != "1" || result.AllowedMentions.RepliedUser {
		t.Fatal("notification allowed unintended mentions")
	}
	if _, err := Payload(Embed{Title: "Title"}, "invalid"); err == nil {
		t.Fatal("invalid Discord mention accepted")
	}
}

func TestPayloadDropsUnsafeURLsAndRejectsInvalidColors(t *testing.T) {
	raw, err := Payload(Embed{Title: "Title", URL: "javascript:alert(1)", Thumbnail: &Image{URL: "file:///secret"}, Author: &Author{Name: "Reader", URL: "https://name:password@example.com"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	var result Message
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	got := result.Embeds[0]
	if got.URL != "" || got.Thumbnail != nil || got.Author.URL != "" {
		t.Fatal("unsafe link survived notification encoding")
	}
	if _, err := Payload(Embed{Title: "Title", Color: 0x1000000}, ""); err == nil {
		t.Fatal("invalid Discord color accepted")
	}
	if SafeURL("https://s4.anilist.co/file.jpg") == "" {
		t.Fatal("valid HTTPS image rejected")
	}
}
