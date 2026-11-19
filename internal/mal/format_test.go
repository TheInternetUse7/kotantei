package mal

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kotantei/internal/notification"
	"kotantei/internal/store"
	"kotantei/internal/update"
)

func sampleProfile() Profile { return Profile{ID: 101, Username: "Reader", AvatarURL: avatarURL(101)} }
func sampleState() store.Snapshot {
	return store.Snapshot{AccountID: 1, Provider: "mal", UserID: "1", ExternalID: "101", Username: "Reader", Features: int64(DefaultFeatures)}
}
func messageEmbed(t *testing.T, payload json.RawMessage) notification.Embed {
	t.Helper()
	var message notification.Message
	if err := json.Unmarshal(payload, &message); err != nil {
		t.Fatal(err)
	}
	if len(message.Embeds) != 1 {
		t.Fatal("missing embed")
	}
	return message.Embeds[0]
}
func field(embed notification.Embed, name string) string {
	for _, field := range embed.Fields {
		if field.Name == name {
			return field.Value
		}
	}
	return ""
}

func TestMALListEmbedKeepsScoresProgressRepeatsAndImportedColors(t *testing.T) {
	entry := sampleEntry(42, 101, "anime")
	entry.ListStatus.Rewatching = true
	entry.ListStatus.TimesRewatched = 2
	state := sampleState()
	state.Colors = []store.ColorOverride{{UpdateType: 5, ColorValue: 123}}
	payload, err := listPayload("anime", entry, sampleProfile(), state, Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	embed := messageEmbed(t, payload)
	if embed.Title != "Title (TV) [Finished airing]" || embed.URL != "https://myanimelist.net/anime/42" || embed.Color != 123 || embed.Timestamp == nil || !embed.Timestamp.Equal(time.Unix(101, 0)) {
		t.Fatalf("wrong list embed: %+v", embed)
	}
	for name, want := range map[string]string{"Score": "8", "Progress": "Rewatching - 3/12 ep.", "Rewatch times": "2", "By": "<@1>"} {
		if field(embed, name) != want {
			t.Fatalf("%s = %q, want %q", name, field(embed, name), want)
		}
	}
	if embed.Author == nil || embed.Author.Name != "Reader" || embed.Footer == nil || embed.Footer.Text != "MyAnimeList" {
		t.Fatal("missing account or provider presentation")
	}
}

func TestMALMangaVolumesAndUnknownTotalsAreDisplayed(t *testing.T) {
	entry := sampleEntry(43, 101, "manga")
	entry.Node.Volumes = 0
	entry.Node.Chapters = 0
	entry.ListStatus.TimesReread = 2
	payload, err := listPayload("manga", entry, sampleProfile(), sampleState(), Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	embed := messageEmbed(t, payload)
	if field(embed, "Progress") != "Reading - 4/? ch., 2/? v." || field(embed, "Reread times") != "2" {
		t.Fatal("unknown manga totals or rereads lost", embed)
	}
	entry.ListStatus.Score = integer(0)
	payload, err = listPayload("manga", entry, sampleProfile(), sampleState(), Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if field(messageEmbed(t, payload), "Score") != "" {
		t.Fatal("zero score was displayed")
	}
}

func TestMALListColorsMatchAllTanteiUpdateTypes(t *testing.T) {
	for _, kind := range []string{"anime", "manga"} {
		current, planned := "watching", "plan_to_watch"
		offset := 0
		if kind == "manga" {
			current, planned = "reading", "plan_to_read"
			offset = 6
		}
		for index, status := range []string{current, planned, "completed", "dropped", "on_hold", current} {
			entry := sampleEntry(42, 101, kind)
			entry.ListStatus.Status = status
			if index == 5 {
				if kind == "anime" {
					entry.ListStatus.Rewatching = true
				} else {
					entry.ListStatus.Rereading = true
				}
			}
			state := sampleState()
			state.Colors = []store.ColorOverride{{UpdateType: uint8(index + offset), ColorValue: uint32(100 + index + offset)}}
			payload, err := listPayload(kind, entry, sampleProfile(), state, Enrichment{})
			if err != nil {
				t.Fatal(err)
			}
			if messageEmbed(t, payload).Color != uint32(100+index+offset) {
				t.Fatal("wrong imported color type", kind, status)
			}
		}
	}
}

func TestMALListFeatureSwitchesControlMetadataCommentsTagsAndDates(t *testing.T) {
	entry := sampleEntry(42, 101, "anime")
	entry.ListStatus.Tags = []string{"A", "B"}
	entry.ListStatus.Comments = "<b>Comment</b>"
	entry.ListStatus.StartDate = "2020-01-02"
	entry.ListStatus.FinishDate = "2020-03"
	entry.Node.Genres = []Catalog{{Name: "Drama"}}
	entry.Node.Synopsis = "<p>Synopsis</p>"
	entry.Node.Studios = []Catalog{{ID: 7, Name: "Studio"}}
	extra := Enrichment{Themes: []string{"Theme"}, Demographics: []string{"Seinen"}, VoiceActors: []NamedLink{{Name: "Actor", URL: "https://myanimelist.net/people/8"}}}
	state := sampleState()
	state.Features = int64(AnimeList)
	payload, err := listPayload("anime", entry, sampleProfile(), state, extra)
	if err != nil {
		t.Fatal(err)
	}
	embed := messageEmbed(t, payload)
	if embed.Title != "Title" || embed.Footer != nil {
		t.Fatal("disabled title/footer flags ignored")
	}
	for _, name := range []string{"Genres", "Synopsis", "Studios", "Tags", "Comments", "Start Date - Finish Date", "Themes", "Demographic", "Seiyu", "By"} {
		if field(embed, name) != "" {
			t.Fatal("disabled feature displayed", name)
		}
	}
	state.Features = int64(DefaultFeatures | Genres | Synopsis | Studio | Tags | Comments | Dates | Themes | Demographic | Seiyu)
	payload, err = listPayload("anime", entry, sampleProfile(), state, extra)
	if err != nil {
		t.Fatal(err)
	}
	embed = messageEmbed(t, payload)
	for _, name := range []string{"Genres", "Synopsis", "Studios", "Tags", "Comments", "Start Date - Finish Date", "Themes", "Demographic", "Seiyu"} {
		if field(embed, name) == "" {
			t.Fatal("enabled feature missing", name)
		}
	}
	if field(embed, "Comments") != "Comment" || field(embed, "Start Date - Finish Date") != "02/01/2020 - 2020-03" {
		t.Fatal("comments or partial dates malformed")
	}
}

func TestMALFavoriteEmbedsCoverFiveKindsAndImportedMetadata(t *testing.T) {
	for _, kind := range []update.SubjectKind{update.AnimeSubject, update.MangaSubject, update.CharacterSubject, update.PersonSubject, update.CompanySubject} {
		favorite := store.Favorite{Subject: update.Subject{Kind: kind, ID: "42"}, Name: "Favorite", ImageURL: "https://cdn.myanimelist.net/cover.jpg", Extra: json.RawMessage(`{"type":"TV","start_year":2020,"from_title_name":"Original work"}`)}
		for _, added := range []bool{true, false} {
			state := sampleState()
			typeID := uint8(13)
			if added {
				typeID = 12
			}
			state.Colors = []store.ColorOverride{{UpdateType: typeID, ColorValue: 123}}
			payload, err := favoritePayload(favorite, sampleProfile(), state, added, Enrichment{})
			if err != nil {
				t.Fatal(err)
			}
			embed := messageEmbed(t, payload)
			if embed.Color != 123 || !strings.Contains(embed.Title, "Favorite") || embed.URL != favoriteURL(favorite.Subject) {
				t.Fatalf("bad %s favorite: %+v", kind, embed)
			}
			if (kind == update.AnimeSubject || kind == update.MangaSubject) && field(embed, "Year") != "2020" {
				t.Fatal("imported favorite year lost")
			}
			if kind == update.CharacterSubject && field(embed, "From") != "Original work" {
				t.Fatal("imported character source lost")
			}
		}
	}
}

func TestMALFavoriteEnrichmentNeverChangesCoreIdentityOrMentionRules(t *testing.T) {
	favorite := store.Favorite{Subject: update.Subject{Kind: update.AnimeSubject, ID: "42"}, Name: "Fallback"}
	entry := sampleEntry(42, 101, "anime")
	extra := Enrichment{Media: entry.Node, Themes: []string{"Theme"}, VoiceActors: []NamedLink{{Name: "Actor", URL: "javascript:alert(1)"}}}
	state := sampleState()
	state.Features |= int64(Themes | Seiyu)
	payload, err := favoritePayload(favorite, sampleProfile(), state, true, extra)
	if err != nil {
		t.Fatal(err)
	}
	var message notification.Message
	if err := json.Unmarshal(payload, &message); err != nil {
		t.Fatal(err)
	}
	if message.Embeds[0].URL != "https://myanimelist.net/anime/42" || field(message.Embeds[0], "Themes") != "Theme" || strings.Contains(string(payload), "javascript:") || len(message.AllowedMentions.Parse) != 0 || len(message.AllowedMentions.Users) != 1 || message.AllowedMentions.Users[0] != "1" {
		t.Fatal("enrichment changed identity or mention safety")
	}
}

func TestMALAuthorNamesDoNotInventCommaPrefixesForMissingNameParts(t *testing.T) {
	entry := sampleEntry(43, 101, "manga")
	entry.Node.Authors = []Author{{Role: "Story"}}
	entry.Node.Authors[0].Node.ID = 7
	entry.Node.Authors[0].Node.FirstName = "Name"
	state := sampleState()
	state.Features |= int64(Mangakas)
	payload, err := listPayload("manga", entry, sampleProfile(), state, Enrichment{})
	if err != nil {
		t.Fatal(err)
	}
	if got := field(messageEmbed(t, payload), "Authors"); !strings.Contains(got, `[Name \(Story\)]`) {
		t.Fatalf("missing surname created an invalid author name: %q", got)
	}
}
