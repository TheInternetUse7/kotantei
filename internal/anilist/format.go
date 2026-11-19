package anilist

import (
	"encoding/json"
	"fmt"
	"html"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"kotantei/internal/notification"
	"kotantei/internal/store"
	"kotantei/internal/update"
)

var htmlTags = regexp.MustCompile(`<[^>]*>`)

func activityPayload(activity Activity, entry Entry, user User, state store.Snapshot) (json.RawMessage, error) {
	if activity.IsPrivate || entry.Private {
		return nil, ErrNotPublic
	}
	media := entry.Media
	if media == nil || media.ID <= 0 || (media.Type != "ANIME" && media.Type != "MANGA") {
		return nil, ErrMalformed
	}
	statusIndex := slices.Index([]string{"CURRENT", "PLANNING", "COMPLETED", "DROPPED", "PAUSED", "REPEATING"}, entry.Status)
	if statusIndex < 0 {
		return nil, ErrMalformed
	}
	updateType := statusIndex
	if media.Type == "MANGA" {
		updateType += 6
	}
	colors := []uint32{0x3db4f2, 0xf79a63, 0x7bd555, 0xe85d75, 0xfa7a7a, 0x3db4f2}
	embed := mediaEmbed(*media, user, Features(state.Features))
	embed.Author = author(user)
	embed.Color = overrideColor(state, uint8(updateType), colors[statusIndex])
	at := time.Unix(activity.CreatedAt, 0).UTC()
	embed.Timestamp = &at
	if activity.Progress != "" && (entry.Status == "PAUSED" || entry.Status == "DROPPED" || entry.Status == "COMPLETED") {
		verb := "Watched episode"
		if media.Type == "MANGA" {
			verb = "Read chapter"
		}
		embed.Description = fmt.Sprintf("%s %s and %s it", verb, plainText(activity.Progress), strings.ToLower(entry.Status))
	} else {
		embed.Description = strings.TrimSpace(sentence(plainText(activity.Status)) + " " + plainText(activity.Progress))
	}
	addField(&embed, "Score", scoreText(entry.Score, user.MediaListOptions.ScoreFormat), true)
	advanced := user.MediaListOptions.AnimeList.AdvancedScoringEnabled
	if media.Type == "MANGA" {
		advanced = user.MediaListOptions.MangaList.AdvancedScoringEnabled
	}
	if advanced {
		keys := make([]string, 0, len(entry.AdvancedScores))
		for key, value := range entry.AdvancedScores {
			if value != 0 {
				keys = append(keys, key)
			}
		}
		slices.Sort(keys)
		lines := make([]string, 0, min(10, len(keys)))
		for _, key := range keys[:min(10, len(keys))] {
			lines = append(lines, plainText(key)+": "+strconv.FormatFloat(entry.AdvancedScores[key], 'f', -1, 64))
		}
		addField(&embed, "Advanced scoring", strings.Join(lines, "\n"), true)
	}
	addTotals(&embed, *media)
	if entry.Repeat > 0 {
		label := "Rewatched times"
		if media.Type == "MANGA" {
			label = "Reread times"
		}
		addField(&embed, label, strconv.Itoa(entry.Repeat), true)
	}
	addField(&embed, "Notes", plainText(entry.Notes), false)
	if Features(state.Features)&CustomLists != 0 {
		var lists []string
		for _, list := range entry.CustomLists {
			if list.Enabled {
				lists = append(lists, plainText(list.Name))
			}
		}
		addField(&embed, "Custom lists", strings.Join(lists, ", "), false)
	}
	enrichEmbed(&embed, *media, user, Features(state.Features))
	return finishEmbed(embed, user, state)
}

func favoritePayload(favorite store.Favorite, user User, state store.Snapshot, added bool) (json.RawMessage, error) {
	var info FavoriteInfo
	if len(favorite.Extra) > 0 {
		_ = json.Unmarshal(favorite.Extra, &info)
	}
	flags := Features(state.Features)
	name := favorite.Name
	if name == "" {
		name = fmt.Sprintf("%s #%s", sentence(string(favorite.Subject.Kind)), favorite.Subject.ID)
	}
	embed := notification.Embed{Title: plainText(name), URL: entityURL(string(favorite.Subject.Kind), favorite.Subject.ID), Author: author(user), Thumbnail: &notification.Image{URL: favorite.ImageURL}}
	verb := "Removed"
	updateType := uint8(13)
	color := uint32(0xe85d75)
	if added {
		verb = "Added"
		updateType = 12
		color = 0x3db4f2
	}
	embed.Color = overrideColor(state, updateType, color)
	embed.Description = verb + " favorite " + string(favorite.Subject.Kind)
	if favorite.Subject.Kind == update.AnimeSubject || favorite.Subject.Kind == update.MangaSubject {
		media := info.Media
		if media.ID > 0 {
			embed.Title = mediaTitle(media, user.Options.TitleLanguage, flags)
			addTotals(&embed, media)
			addField(&embed, "Community score", scoreText(communityScore(media.AverageScore, user.MediaListOptions.ScoreFormat), user.MediaListOptions.ScoreFormat), true)
			enrichEmbed(&embed, media, user, flags)
		}
	} else {
		label := sentence(string(favorite.Subject.Kind))
		if favorite.Subject.Kind == update.StaffSubject && len(info.PrimaryOccupations) > 0 {
			label = info.PrimaryOccupations[0]
		}
		embed.Title += " [" + plainText(label) + "]"
		if flags&Description != 0 {
			addField(&embed, "Description", shortText(plainText(info.Description), 350), false)
		}
		if len(info.KnownFor.Nodes) > 0 {
			work := info.KnownFor.Nodes[0]
			kind := strings.ToLower(work.Type)
			addField(&embed, "Known for", namedLink(mediaTitle(work, user.Options.TitleLanguage, 0), kind, work.ID), true)
		}
	}
	return finishEmbed(embed, user, state)
}

func reviewPayload(review Review, user User, state store.Snapshot) (json.RawMessage, error) {
	if review.Private {
		return nil, ErrNotPublic
	}
	if review.Media == nil || review.Media.ID <= 0 {
		return nil, ErrMalformed
	}
	at := time.Unix(review.CreatedAt, 0).UTC()
	embed := notification.Embed{Title: "New review on " + mediaTitle(*review.Media, user.Options.TitleLanguage, Features(state.Features)&MediaFormat), Description: plainText(review.Summary), URL: entityURL("review", strconv.FormatInt(review.ID, 10)), Author: author(user), Timestamp: &at, Thumbnail: &notification.Image{URL: imageURL(review.Media.CoverImage)}, Color: overrideColor(state, 14, 0x3db4f2)}
	return finishEmbed(embed, user, state)
}

func finishEmbed(embed notification.Embed, _ User, state store.Snapshot) (json.RawMessage, error) {
	flags := Features(state.Features)
	mention := ""
	if flags&Mention != 0 {
		mention = state.UserID
		addField(&embed, "By", "<@"+mention+">", true)
	}
	if flags&Website != 0 {
		embed.Footer = &notification.Footer{Text: "AniList", IconURL: "https://anilist.co/img/icons/android-chrome-512x512.png"}
	}
	return notification.Payload(embed, mention)
}

func mediaEmbed(media Media, user User, flags Features) notification.Embed {
	return notification.Embed{Title: mediaTitle(media, user.Options.TitleLanguage, flags), URL: entityURL(strings.ToLower(media.Type), strconv.FormatInt(media.ID, 10)), Thumbnail: &notification.Image{URL: imageURL(media.CoverImage)}}
}

func author(user User) *notification.Author {
	url := notification.SafeURL(user.SiteURL)
	if url == "" {
		url = entityURL("user", strconv.FormatInt(user.ID, 10))
	}
	return &notification.Author{Name: user.Name, URL: url, IconURL: imageURL(user.Avatar)}
}

func mediaTitle(media Media, language string, flags Features) string {
	var title string
	switch language {
	case "ENGLISH":
		title = media.Title.English
	case "NATIVE":
		title = media.Title.Native
	case "ROMAJI_STYLISED":
		title = media.Title.RomajiStyled
	case "ENGLISH_STYLISED":
		title = media.Title.EnglishStyled
	case "NATIVE_STYLISED":
		title = media.Title.NativeStyled
	default:
		title = media.Title.Romaji
	}
	if title == "" {
		title = media.Title.Romaji
	}
	if title == "" {
		title = media.Title.English
	}
	if title == "" {
		title = media.Title.Native
	}
	if title == "" {
		title = fmt.Sprintf("%s #%d", sentence(strings.ToLower(media.Type)), media.ID)
	}
	title = plainText(title)
	if flags&MediaFormat != 0 && media.Format != "" {
		format := media.Format
		if media.CountryOfOrigin == "CN" {
			if media.Type == "ANIME" {
				format = "Donghua"
			} else {
				format = "Manhua"
			}
		} else if media.CountryOfOrigin == "KR" && media.Type == "MANGA" {
			format = "Manhwa"
		} else {
			labels := map[string]string{"TV": "TV", "TV_SHORT": "TV short", "MOVIE": "Movie", "SPECIAL": "Special", "OVA": "OVA", "ONA": "ONA", "MUSIC": "Music", "MANGA": "Manga", "NOVEL": "Novel", "ONE_SHOT": "One shot"}
			if label, ok := labels[format]; ok {
				format = label
			}
		}
		title += " (" + format + ")"
	}
	if flags&MediaStatus != 0 && media.Status != "" {
		title += " [" + sentence(strings.ToLower(strings.ReplaceAll(media.Status, "_", " "))) + "]"
	}
	return title
}

func addTotals(embed *notification.Embed, media Media) {
	var parts []string
	if media.Episodes > 0 {
		parts = append(parts, fmt.Sprintf("%d ep.", media.Episodes))
	}
	if media.Chapters > 0 {
		parts = append(parts, fmt.Sprintf("%d ch", media.Chapters))
	}
	if media.Volumes > 0 {
		parts = append(parts, fmt.Sprintf("%d v.", media.Volumes))
	}
	addField(embed, "Total", strings.Join(parts, ", "), true)
}

func enrichEmbed(embed *notification.Embed, media Media, user User, flags Features) {
	if flags&Genres != 0 {
		addField(embed, "Genres", strings.Join(media.Genres, ", "), false)
	}
	if flags&Tags != 0 {
		tags := slices.Clone(media.Tags)
		slices.SortStableFunc(tags, func(a, b Tag) int { return b.Rank - a.Rank })
		var names []string
		for _, tag := range tags[:min(7, len(tags))] {
			name := plainText(tag.Name)
			if tag.IsMediaSpoiler || tag.IsGeneralSpoiler {
				name = "||" + strings.ReplaceAll(name, "|", "") + "||"
			}
			names = append(names, name)
		}
		addField(embed, "Tags", strings.Join(names, ", "), false)
	}
	if flags&Description != 0 {
		addField(embed, "Description", shortText(plainText(media.Description), 350), false)
	}
	if media.Type == "ANIME" {
		if flags&Studio != 0 {
			var names []string
			for _, studio := range media.Studios.Nodes {
				if studio.IsAnimationStudio {
					names = append(names, namedLink(studio.Name, "studio", studio.ID))
				}
				if len(names) == 7 {
					break
				}
			}
			addField(embed, "Made by", strings.Join(names, ", "), true)
		}
		if flags&Director != 0 {
			for _, edge := range media.Staff.Edges {
				if strings.EqualFold(edge.Role, "Director") {
					addField(embed, "Director", namedLink(personName(edge.Node.Name, user.Options.TitleLanguage), "staff", edge.Node.ID), true)
					break
				}
			}
		}
		if flags&VoiceActors != 0 {
			var names []string
			seen := map[int64]bool{}
			for _, edge := range media.Characters.Edges {
				if len(edge.VoiceActors) == 0 {
					continue
				}
				actor := edge.VoiceActors[0]
				if seen[actor.ID] {
					continue
				}
				seen[actor.ID] = true
				names = append(names, namedLink(personName(actor.Name, user.Options.TitleLanguage), "staff", actor.ID))
				if len(names) == 7 {
					break
				}
			}
			addField(embed, "Seyu", strings.Join(names, ", "), false)
		}
	} else if flags&Mangaka != 0 {
		var names []string
		for _, edge := range media.Staff.Edges {
			role := strings.ToLower(edge.Role)
			if strings.Contains(role, "story") || strings.Contains(role, "art") || strings.Contains(role, "author") || strings.Contains(role, "original") {
				names = append(names, namedLink(personName(edge.Node.Name, user.Options.TitleLanguage), "staff", edge.Node.ID)+" — "+plainText(edge.Role))
			}
			if len(names) == 7 {
				break
			}
		}
		addField(embed, "Made by", strings.Join(names, ", "), false)
	}
}

func scoreText(value float64, format string) string {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return ""
	}
	switch format {
	case "POINT_10_DECIMAL":
		return strconv.FormatFloat(value, 'f', -1, 64) + "/10"
	case "POINT_10":
		return strconv.FormatFloat(math.Round(value), 'f', 0, 64) + "/10"
	case "POINT_5":
		return strings.Repeat("★", min(5, max(1, int(math.Round(value))))) + "/5"
	case "POINT_3":
		faces := []string{"😞", "😐", "😊"}
		return faces[min(2, max(0, int(math.Round(value))-1))]
	default:
		return strconv.FormatFloat(math.Round(value), 'f', 0, 64) + "/100"
	}
}

func communityScore(value int, format string) float64 {
	switch format {
	case "POINT_10", "POINT_10_DECIMAL":
		return float64(value) / 10
	case "POINT_5":
		return float64(value) / 20
	case "POINT_3":
		return float64(value) * 3 / 100
	default:
		return float64(value)
	}
}
func addField(embed *notification.Embed, name, value string, inline bool) {
	if strings.TrimSpace(value) != "" {
		embed.Fields = append(embed.Fields, notification.Field{Name: name, Value: value, Inline: inline})
	}
}
func overrideColor(state store.Snapshot, kind uint8, fallback uint32) uint32 {
	for _, color := range state.Colors {
		if color.UpdateType == kind {
			return color.ColorValue
		}
	}
	return fallback
}
func entityURL(kind, id string) string { return "https://anilist.co/" + kind + "/" + id }
func imageURL(image Image) string {
	if image.ExtraLarge != "" {
		return image.ExtraLarge
	}
	return image.Large
}
func personName(name Name, language string) string {
	if strings.HasPrefix(language, "NATIVE") && name.Native != "" {
		return name.Native
	}
	return name.Full
}
func namedLink(name, kind string, id int64) string {
	replacer := strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)")
	return "[" + replacer.Replace(shortText(plainText(name), 120)) + "](" + entityURL(kind, strconv.FormatInt(id, 10)) + ")"
}
func plainText(value string) string {
	return strings.TrimSpace(html.UnescapeString(htmlTags.ReplaceAllString(value, "")))
}
func shortText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return value
}
func sentence(value string) string {
	runes := []rune(value)
	if len(runes) > 0 {
		runes[0] = unicode.ToUpper(runes[0])
	}
	return string(runes)
}
