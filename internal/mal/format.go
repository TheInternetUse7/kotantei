package mal

import (
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"kotantei/internal/notification"
	"kotantei/internal/store"
	"kotantei/internal/update"
)

var htmlTags = regexp.MustCompile(`<[^>]*>`)

func listPayload(kind string, entry Entry, profile Profile, state store.Snapshot, extra Enrichment) (json.RawMessage, error) {
	if _, err := entryRevision(kind, entry); err != nil {
		return nil, err
	}
	if strings.TrimSpace(entry.Node.Title) == "" {
		return nil, ErrMalformed
	}
	flags := Features(state.Features)
	media, status := entry.Node, entry.ListStatus
	embed := notification.Embed{Title: mediaTitle(*media, flags), URL: favoriteURL(update.Subject{Kind: update.SubjectKind(kind), ID: strconv.FormatInt(media.ID, 10)}), Author: profileAuthor(profile), Thumbnail: &notification.Image{URL: pictureURL(media.Picture)}}
	at := status.UpdatedAt.UTC()
	embed.Timestamp = &at
	index := 0
	label := "Watching"
	planned := false
	switch status.Status {
	case "completed":
		index = 2
		label = "Completed"
	case "dropped":
		index = 3
		label = "Dropped"
	case "on_hold":
		index = 4
		label = "On hold"
	case "plan_to_watch", "plan_to_read":
		index = 1
		planned = true
		label = "Plan to watch"
	case "reading":
		label = "Reading"
	}
	progress := ""
	times := status.TimesRewatched
	timesLabel := "Rewatch times"
	if kind == "anime" {
		progress = progressText(*status.EpisodesWatched, media.Episodes, planned, "ep.")
		if status.Rewatching {
			index = 5
			label = "Rewatching"
		}
	} else {
		if planned {
			label = "Plan to read"
		}
		times = status.TimesReread
		timesLabel = "Reread times"
		if status.Rereading {
			index = 5
			label = "Rereading"
		}
		var parts []string
		for _, text := range []string{progressText(*status.ChaptersRead, media.Chapters, planned, "ch."), progressText(*status.VolumesRead, media.Volumes, planned, "v.")} {
			if text != "" {
				parts = append(parts, text)
			}
		}
		progress = strings.Join(parts, ", ")
	}
	colors := []uint32{0x2db039, 0xa6a6a6, 0x2e51a2, 0xee5a5a, 0xf0c83e, 0x2db039}
	colorType := index
	if kind == "manga" {
		colorType += 6
	}
	embed.Color = colorOverride(state, uint8(colorType), colors[index])
	if *status.Score > 0 {
		addField(&embed, "Score", strconv.Itoa(*status.Score), true)
	}
	if progress != "" {
		label += " - " + progress
	}
	addField(&embed, "Progress", label, true)
	if times > 0 {
		addField(&embed, timesLabel, strconv.Itoa(times), true)
	}
	if flags&Tags != 0 {
		addField(&embed, "Tags", strings.Join(status.Tags, ", "), false)
	}
	if flags&Comments != 0 {
		addField(&embed, "Comments", plainText(status.Comments), false)
	}
	if flags&Dates != 0 {
		start, finish := dateText(status.StartDate), dateText(status.FinishDate)
		switch {
		case start != "" && finish != "":
			addField(&embed, "Start Date - Finish Date", start+" - "+finish, false)
		case start != "":
			addField(&embed, "Start Date", start, false)
		case finish != "":
			addField(&embed, "Finish Date", finish, false)
		}
	}
	addMediaMetadata(&embed, *media, flags)
	addEnrichment(&embed, extra, flags)
	return finishEmbed(embed, state)
}

func favoritePayload(favorite store.Favorite, profile Profile, state store.Snapshot, added bool, extra Enrichment) (json.RawMessage, error) {
	if err := favorite.Subject.Validate(); err != nil {
		return nil, err
	}
	flags := Features(state.Features)
	metadata := favoriteMetadata(favorite.Extra)
	name := favorite.Name
	if name == "" {
		name = sentence(string(favorite.Subject.Kind)) + " #" + favorite.Subject.ID
	}
	embed := notification.Embed{Title: plainText(name), URL: favoriteURL(favorite.Subject), Author: profileAuthor(profile), Thumbnail: &notification.Image{URL: favorite.ImageURL}}
	index := uint8(13)
	color := uint32(0xee5a5a)
	verb := "Removed"
	if added {
		index = 12
		color = 0x2db039
		verb = "Added"
	}
	embed.Color = colorOverride(state, index, color)
	embed.Description = verb + " favorite"
	if favorite.Subject.Kind == update.AnimeSubject || favorite.Subject.Kind == update.MangaSubject {
		if extra.Media != nil && strconv.FormatInt(extra.Media.ID, 10) == favorite.Subject.ID {
			embed.Title = mediaTitle(*extra.Media, flags)
			if image := pictureURL(extra.Media.Picture); image != "" {
				embed.Thumbnail = &notification.Image{URL: image}
			}
			if extra.Media.Mean > 0 {
				addField(&embed, "Community score", strconv.FormatFloat(extra.Media.Mean, 'f', -1, 64), true)
			}
			var totals []string
			if extra.Media.Episodes > 0 {
				totals = append(totals, fmt.Sprintf("%d ep.", extra.Media.Episodes))
			}
			if extra.Media.Chapters > 0 {
				totals = append(totals, fmt.Sprintf("%d ch.", extra.Media.Chapters))
			}
			if extra.Media.Volumes > 0 {
				totals = append(totals, fmt.Sprintf("%d v.", extra.Media.Volumes))
			}
			addField(&embed, "Total", strings.Join(totals, ", "), true)
			addMediaMetadata(&embed, *extra.Media, flags)
		} else if flags&MediaFormat != 0 && metadata.Format != "" {
			embed.Title += " (" + mediaFormat(metadata.Format) + ")"
		}
		if metadata.Year > 0 {
			addField(&embed, "Year", strconv.Itoa(metadata.Year), true)
		}
	} else {
		label := sentence(string(favorite.Subject.Kind))
		if favorite.Subject.Kind == update.CompanySubject {
			label = "Studio"
		}
		embed.Title += " [" + label + "]"
		if favorite.Subject.Kind == update.CharacterSubject {
			addField(&embed, "From", plainText(metadata.From), true)
		}
		if extra.KnownFor != nil {
			label := "Known for"
			if favorite.Subject.Kind == update.CharacterSubject {
				label = "From"
				removeField(&embed, label)
			}
			text := namedLink(extra.KnownFor.Title, extra.KnownFor.URL)
			if extra.KnownFor.Character != "" {
				text = plainText(extra.KnownFor.Character) + " from " + text
			}
			addField(&embed, label, text, true)
		}
		if flags&Synopsis != 0 {
			addField(&embed, "Description", shortText(plainText(extra.Description), 500), false)
		}
	}
	addEnrichment(&embed, extra, flags)
	return finishEmbed(embed, state)
}

func finishEmbed(embed notification.Embed, state store.Snapshot) (json.RawMessage, error) {
	mention := ""
	flags := Features(state.Features)
	if flags&Mention != 0 {
		mention = state.UserID
		addField(&embed, "By", "<@"+mention+">", true)
	}
	if flags&Website != 0 {
		embed.Footer = &notification.Footer{Text: "MyAnimeList", IconURL: "https://cdn.myanimelist.net/images/favicon.ico"}
	}
	return notification.Payload(embed, mention)
}
func profileAuthor(profile Profile) *notification.Author {
	return &notification.Author{Name: profile.Username, URL: "https://myanimelist.net/profile/" + profile.Username, IconURL: profile.AvatarURL}
}
func mediaTitle(media Media, flags Features) string {
	title := plainText(media.Title)
	if flags&MediaFormat != 0 && media.Format != "" {
		format := mediaFormat(media.Format)
		if !strings.HasSuffix(strings.ToLower(title), strings.ToLower("("+format+")")) {
			title += " (" + format + ")"
		}
	}
	if flags&MediaStatus != 0 && media.Status != "" {
		title += " [" + sentence(strings.ReplaceAll(media.Status, "_", " ")) + "]"
	}
	return title
}
func mediaFormat(value string) string {
	switch strings.ToLower(value) {
	case "tv":
		return "TV"
	case "ova":
		return "OVA"
	case "ona":
		return "ONA"
	default:
		return sentence(strings.ReplaceAll(value, "_", " "))
	}
}
func progressText(current, total int, planned bool, unit string) string {
	if current == 0 && total == 0 {
		return ""
	}
	if current == total || (planned && current == 0) {
		return fmt.Sprintf("%d %s", total, unit)
	}
	if total == 0 {
		return fmt.Sprintf("%d/? %s", current, unit)
	}
	return fmt.Sprintf("%d/%d %s", current, total, unit)
}
func addMediaMetadata(embed *notification.Embed, media Media, flags Features) {
	if flags&Genres != 0 {
		var names []string
		for _, genre := range media.Genres[:min(7, len(media.Genres))] {
			names = append(names, plainText(genre.Name))
		}
		addField(embed, "Genres", strings.Join(names, ", "), false)
	}
	if flags&Synopsis != 0 {
		addField(embed, "Synopsis", shortText(plainText(media.Synopsis), 500), false)
	}
	if flags&Studio != 0 {
		var names []string
		for _, studio := range media.Studios[:min(3, len(media.Studios))] {
			names = append(names, namedLink(studio.Name, "https://myanimelist.net/anime/producer/"+strconv.FormatInt(studio.ID, 10)))
		}
		addField(embed, "Studios", strings.Join(names, ", "), false)
	}
	if flags&Mangakas != 0 {
		var names []string
		for _, author := range media.Authors[:min(7, len(media.Authors))] {
			name := strings.TrimSpace(author.Node.LastName)
			if name == "" {
				name = strings.TrimSpace(author.Node.FirstName)
			} else if author.Node.FirstName != "" {
				name += ", " + strings.TrimSpace(author.Node.FirstName)
			}
			if author.Role != "" {
				name += " (" + author.Role + ")"
			}
			names = append(names, namedLink(name, "https://myanimelist.net/people/"+strconv.FormatInt(author.Node.ID, 10)))
		}
		addField(embed, "Authors", strings.Join(names, ", "), false)
	}
}
func addEnrichment(embed *notification.Embed, extra Enrichment, flags Features) {
	if flags&Themes != 0 {
		addField(embed, "Themes", strings.Join(extra.Themes[:min(7, len(extra.Themes))], ", "), false)
	}
	if flags&Demographic != 0 {
		addField(embed, "Demographic", strings.Join(extra.Demographics[:min(3, len(extra.Demographics))], ", "), false)
	}
	if flags&Seiyu != 0 {
		var names []string
		for _, actor := range extra.VoiceActors[:min(7, len(extra.VoiceActors))] {
			names = append(names, namedLink(actor.Name, actor.URL))
		}
		addField(embed, "Seiyu", strings.Join(names, ", "), false)
	}
}
func favoriteMetadata(raw json.RawMessage) FavoriteMetadata {
	var extra FavoriteMetadata
	_ = json.Unmarshal(raw, &extra)
	var imported struct {
		Type string `json:"type"`
		Year int    `json:"start_year"`
		From string `json:"from_title_name"`
	}
	_ = json.Unmarshal(raw, &imported)
	if extra.Format == "" {
		extra.Format = imported.Type
	}
	if extra.Year == 0 {
		extra.Year = imported.Year
	}
	if extra.From == "" {
		extra.From = imported.From
	}
	return extra
}
func pictureURL(picture Picture) string {
	for _, raw := range []string{picture.Large, picture.Medium} {
		if safe := notification.SafeURL(raw); safe != "" {
			return safe
		}
	}
	return ""
}
func colorOverride(state store.Snapshot, index uint8, fallback uint32) uint32 {
	for _, color := range state.Colors {
		if color.UpdateType == index {
			return color.ColorValue
		}
	}
	return fallback
}
func addField(embed *notification.Embed, name, value string, inline bool) {
	if strings.TrimSpace(value) != "" {
		embed.Fields = append(embed.Fields, notification.Field{Name: name, Value: value, Inline: inline})
	}
}
func removeField(embed *notification.Embed, name string) {
	fields := embed.Fields[:0]
	for _, field := range embed.Fields {
		if field.Name != name {
			fields = append(fields, field)
		}
	}
	embed.Fields = fields
}
func namedLink(name, raw string) string {
	name = plainText(name)
	safe := notification.SafeURL(raw)
	if safe == "" {
		return name
	}
	replacer := strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)")
	return "[" + replacer.Replace(shortText(name, 120)) + "](" + safe + ")"
}
func plainText(raw string) string {
	return strings.TrimSpace(html.UnescapeString(htmlTags.ReplaceAllString(raw, "")))
}
func shortText(raw string, max int) string {
	runes := []rune(raw)
	if len(runes) > max {
		return string(runes[:max-1]) + "…"
	}
	return raw
}
func sentence(raw string) string {
	runes := []rune(raw)
	if len(runes) > 0 {
		runes[0] = unicode.ToUpper(runes[0])
	}
	return string(runes)
}
func dateText(raw string) string {
	if raw == "" || strings.HasPrefix(raw, "0000") {
		return ""
	}
	if at, err := time.Parse(time.DateOnly, raw); err == nil {
		return at.Format("02/01/2006")
	}
	if validDate(raw) {
		return raw
	}
	return ""
}
