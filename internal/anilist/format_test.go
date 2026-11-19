package anilist

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"kotantei/internal/notification"
	"kotantei/internal/store"
	"kotantei/internal/update"
)

func exampleUser() User {
	return User{ID: 101, Name: "Reader", SiteURL: "https://anilist.co/user/Reader", Avatar: Image{Large: "https://s4.anilist.co/avatar.jpg"}, Options: UserOptions{TitleLanguage: "ENGLISH"}, MediaListOptions: ListOptions{ScoreFormat: "POINT_10_DECIMAL", AnimeList: TypeOptions{AdvancedScoringEnabled: true}}}
}
func exampleMedia() Media {
	return Media{ID: 42, Type: "ANIME", Title: Title{Romaji: "Romaji title", English: "English title", Native: "作品"}, Format: "TV", Status: "FINISHED", CoverImage: Image{Large: "https://s4.anilist.co/cover.jpg"}, Episodes: 12}
}
func exampleState() store.Snapshot {
	return store.Snapshot{AccountID: 1, UserID: "1", Provider: "anilist", ExternalID: "101", Features: int64(DefaultFeatures)}
}
func decodeMessage(t *testing.T, raw json.RawMessage) notification.Message {
	t.Helper()
	var message notification.Message
	if err := json.Unmarshal(raw, &message); err != nil {
		t.Fatal(err)
	}
	if len(message.Embeds) != 1 {
		t.Fatal("missing embed")
	}
	return message
}
func fieldValue(embed notification.Embed, name string) string {
	for _, field := range embed.Fields {
		if field.Name == name {
			return field.Value
		}
	}
	return ""
}

func TestActivityEmbedPreservesProgressScoresRepeatsNotesAndImportedColors(t *testing.T) {
	media := exampleMedia()
	user := exampleUser()
	state := exampleState()
	state.Colors = []store.ColorOverride{{UpdateType: 2, ColorValue: 123}}
	activity := Activity{ID: 10, CreatedAt: 200, Status: "watched episode", Progress: "3", Media: &media}
	entry := Entry{Media: &media, Status: "COMPLETED", Score: 8.5, Repeat: 2, Notes: "A note", AdvancedScores: map[string]float64{"Story": 9, "Art": 0}}
	raw, err := activityPayload(activity, entry, user, state)
	if err != nil {
		t.Fatal(err)
	}
	message := decodeMessage(t, raw)
	embed := message.Embeds[0]
	if embed.Title != "English title (TV) [Finished]" || embed.URL != "https://anilist.co/anime/42" || embed.Color != 123 || embed.Timestamp == nil || !embed.Timestamp.Equal(time.Unix(200, 0)) || embed.Author == nil || embed.Author.Name != "Reader" {
		t.Fatalf("incorrect activity presentation: %+v", embed)
	}
	for name, want := range map[string]string{"Score": "8.5/10", "Total": "12 ep.", "Rewatched times": "2", "Notes": "A note", "Advanced scoring": "Story: 9", "By": "<@1>"} {
		if got := fieldValue(embed, name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if !strings.Contains(embed.Description, "Watched episode 3") || !strings.Contains(embed.Description, "completed it") || embed.Footer == nil || embed.Footer.Text != "AniList" {
		t.Fatal("missing progress/status/footer")
	}
}

func TestActivityColorsMatchAllImportedUpdateTypes(t *testing.T) {
	statuses := []string{"CURRENT", "PLANNING", "COMPLETED", "DROPPED", "PAUSED", "REPEATING"}
	for _, kind := range []string{"ANIME", "MANGA"} {
		for index, status := range statuses {
			media := exampleMedia()
			media.Type = kind
			typeID := index
			if kind == "MANGA" {
				typeID += 6
			}
			state := exampleState()
			state.Colors = []store.ColorOverride{{UpdateType: uint8(typeID), ColorValue: uint32(100 + typeID)}}
			raw, err := activityPayload(Activity{CreatedAt: 200, Status: "updated", Media: &media}, Entry{Status: status, Media: &media}, exampleUser(), state)
			if err != nil {
				t.Fatal(err)
			}
			if got := decodeMessage(t, raw).Embeds[0].Color; got != uint32(100+typeID) {
				t.Fatalf("%s %s used the wrong color type", kind, status)
			}
		}
	}
}

func TestActivityEmbedHonorsOptionalMetadataAndFeatureSwitches(t *testing.T) {
	media := exampleMedia()
	media.Description = "<b>Description</b> &amp; text"
	media.Genres = []string{"Drama", "Fantasy"}
	media.Tags = []Tag{{Name: "Spoiler", Rank: 100, IsMediaSpoiler: true}}
	media.Studios.Nodes = []StudioNode{{ID: 7, Name: "Studio", IsAnimationStudio: true}}
	media.Staff.Edges = []StaffEdge{{Role: "Director", Node: Person{ID: 8, Name: Name{Full: "Director"}}}}
	media.Characters.Edges = []CharacterEdge{{VoiceActors: []Person{{ID: 9, Name: Name{Full: "Actor"}}}}}
	entry := Entry{Status: "CURRENT", Media: &media, CustomLists: []CustomList{{Name: "Seasonal", Enabled: true}}}
	state := exampleState()
	state.Features = int64(AnimeList)
	raw, err := activityPayload(Activity{CreatedAt: 200, Status: "watched episode", Media: &media}, entry, exampleUser(), state)
	if err != nil {
		t.Fatal(err)
	}
	embed := decodeMessage(t, raw).Embeds[0]
	for _, name := range []string{"Description", "Genres", "Tags", "Made by", "Director", "Seyu", "Custom lists", "By"} {
		if fieldValue(embed, name) != "" {
			t.Fatalf("disabled metadata %s displayed", name)
		}
	}
	if embed.Title != "English title" || embed.Footer != nil {
		t.Fatal("disabled title/footer switches ignored")
	}
	state.Features = int64(DefaultFeatures | Description | Genres | Tags | Studio | Director | VoiceActors | CustomLists)
	raw, err = activityPayload(Activity{CreatedAt: 200, Status: "watched episode", Media: &media}, entry, exampleUser(), state)
	if err != nil {
		t.Fatal(err)
	}
	embed = decodeMessage(t, raw).Embeds[0]
	for _, name := range []string{"Description", "Genres", "Tags", "Made by", "Director", "Seyu", "Custom lists"} {
		if fieldValue(embed, name) == "" {
			t.Fatalf("enabled metadata %s missing", name)
		}
	}
	if fieldValue(embed, "Description") != "Description & text" || !strings.Contains(fieldValue(embed, "Tags"), "||Spoiler||") {
		t.Fatal("description/spoiler formatting failed")
	}
}

func TestActivityEmbedUsesNativeAndStylisedTitlesAndMangaCounts(t *testing.T) {
	media := exampleMedia()
	media.Type = "MANGA"
	media.Format = "MANGA"
	media.CountryOfOrigin = "KR"
	media.Title.NativeStyled = "作品！"
	media.Episodes = 0
	media.Chapters = 100
	media.Volumes = 12
	user := exampleUser()
	user.Options.TitleLanguage = "NATIVE_STYLISED"
	raw, err := activityPayload(Activity{CreatedAt: 200, Status: "read chapter", Progress: "4", Media: &media}, Entry{Status: "CURRENT", Media: &media}, user, exampleState())
	if err != nil {
		t.Fatal(err)
	}
	embed := decodeMessage(t, raw).Embeds[0]
	if !strings.HasPrefix(embed.Title, "作品！ (Manhwa)") || fieldValue(embed, "Total") != "100 ch, 12 v." {
		t.Fatalf("manga title/counts incorrect: %+v", embed)
	}
}

func TestScoreFormatsAndZeroScores(t *testing.T) {
	for _, tc := range []struct {
		value        float64
		format, want string
	}{{0, "POINT_100", ""}, {85, "POINT_100", "85/100"}, {8.5, "POINT_10_DECIMAL", "8.5/10"}, {8, "POINT_10", "8/10"}, {4, "POINT_5", "★★★★/5"}, {1, "POINT_3", "😞"}, {2, "POINT_3", "😐"}, {3, "POINT_3", "😊"}} {
		if got := scoreText(tc.value, tc.format); got != tc.want {
			t.Fatalf("%s: %q, want %q", tc.format, got, tc.want)
		}
	}
}

func TestFavoriteEmbedsSupportAllCategoriesAndColorOverrides(t *testing.T) {
	for _, kind := range []update.SubjectKind{update.AnimeSubject, update.MangaSubject, update.CharacterSubject, update.StaffSubject, update.StudioSubject} {
		favorite := store.Favorite{Subject: update.Subject{Kind: kind, ID: "42"}, Name: "Favorite", ImageURL: "https://s4.anilist.co/image.jpg"}
		for _, added := range []bool{true, false} {
			state := exampleState()
			typeID := uint8(13)
			if added {
				typeID = 12
			}
			state.Colors = []store.ColorOverride{{UpdateType: typeID, ColorValue: 123}}
			raw, err := favoritePayload(favorite, exampleUser(), state, added)
			if err != nil {
				t.Fatal(err)
			}
			embed := decodeMessage(t, raw).Embeds[0]
			verb := "Removed"
			if added {
				verb = "Added"
			}
			if embed.Color != 123 || !strings.Contains(embed.Title, "Favorite") || !strings.Contains(embed.Description, verb) || embed.URL != fmt.Sprintf("https://anilist.co/%s/42", kind) {
				t.Fatalf("%s favorite incorrect: %+v", kind, embed)
			}
		}
	}
}

func TestReviewEmbedsUseSummaryTimestampAndImportedColor(t *testing.T) {
	media := exampleMedia()
	state := exampleState()
	state.Features |= int64(Reviews)
	state.Colors = []store.ColorOverride{{UpdateType: 14, ColorValue: 123}}
	raw, err := reviewPayload(Review{ID: 10, CreatedAt: 200, Summary: "<b>Summary</b>", Media: &media}, exampleUser(), state)
	if err != nil {
		t.Fatal(err)
	}
	embed := decodeMessage(t, raw).Embeds[0]
	if !strings.Contains(embed.Title, "New review on English title") || embed.Description != "Summary" || embed.URL != "https://anilist.co/review/10" || embed.Color != 123 || embed.Timestamp == nil {
		t.Fatalf("incorrect review: %+v", embed)
	}
}

func TestReviewFormatterRefusesPrivateDraftContents(t *testing.T) {
	media := exampleMedia()
	if _, err := reviewPayload(Review{ID: 10, Private: true, CreatedAt: 200, Media: &media}, exampleUser(), exampleState()); err == nil {
		t.Fatal("private review was formatted")
	}
}

func TestActivityFormatterRefusesPrivateOrMissingListEntries(t *testing.T) {
	media := exampleMedia()
	if _, err := activityPayload(Activity{CreatedAt: 200, Media: &media}, Entry{Status: "CURRENT", Media: &media, Private: true}, exampleUser(), exampleState()); err == nil {
		t.Fatal("private list data was formatted")
	}
	if _, err := activityPayload(Activity{CreatedAt: 200, Media: &media}, Entry{Status: "CURRENT"}, exampleUser(), exampleState()); err == nil {
		t.Fatal("missing list data was formatted")
	}
}
