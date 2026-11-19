// Package notification defines SDK-independent, bounded Discord message data.
package notification

import (
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var ErrInvalidMessage = errors.New("invalid notification message")

type Image struct {
	URL string `json:"url"`
}
type Author struct {
	Name    string `json:"name"`
	URL     string `json:"url,omitempty"`
	IconURL string `json:"icon_url,omitempty"`
}
type Footer struct {
	Text    string `json:"text"`
	IconURL string `json:"icon_url,omitempty"`
}
type Field struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}
type Embed struct {
	Title       string     `json:"title,omitempty"`
	URL         string     `json:"url,omitempty"`
	Description string     `json:"description,omitempty"`
	Color       uint32     `json:"color"`
	Timestamp   *time.Time `json:"timestamp,omitempty"`
	Author      *Author    `json:"author,omitempty"`
	Footer      *Footer    `json:"footer,omitempty"`
	Thumbnail   *Image     `json:"thumbnail,omitempty"`
	Fields      []Field    `json:"fields,omitempty"`
}
type AllowedMentions struct {
	Parse       []string `json:"parse"`
	Users       []string `json:"users"`
	RepliedUser bool     `json:"replied_user"`
}
type Message struct {
	Embeds          []Embed         `json:"embeds"`
	AllowedMentions AllowedMentions `json:"allowed_mentions"`
}

// Payload bounds text using Discord's UTF-16 limits and prevents all mentions
// except an explicitly supplied Discord user. The input is never modified.
func Payload(embed Embed, userID string) (json.RawMessage, error) {
	if embed.Color > 0xffffff {
		return nil, ErrInvalidMessage
	}
	mentions := AllowedMentions{Parse: []string{}, Users: []string{}}
	if userID != "" {
		id, err := strconv.ParseUint(userID, 10, 64)
		if err != nil || id == 0 || strconv.FormatUint(id, 10) != userID {
			return nil, ErrInvalidMessage
		}
		mentions.Users = append(mentions.Users, userID)
	}
	left := 6000
	text := func(value string, limit int) string {
		if left < limit {
			limit = left
		}
		value = clip(value, limit)
		left -= textUnits(value)
		return value
	}
	embed.Title = text(embed.Title, 256)
	embed.URL = SafeURL(embed.URL)
	if embed.Author != nil {
		author := *embed.Author
		author.Name = text(author.Name, 256)
		author.URL = SafeURL(author.URL)
		author.IconURL = SafeURL(author.IconURL)
		embed.Author = &author
		if author.Name == "" {
			embed.Author = nil
		}
	}
	embed.Description = text(embed.Description, 4096)
	if embed.Footer != nil {
		footer := *embed.Footer
		footer.Text = text(footer.Text, 2048)
		footer.IconURL = SafeURL(footer.IconURL)
		embed.Footer = &footer
		if footer.Text == "" {
			embed.Footer = nil
		}
	}
	if embed.Thumbnail != nil {
		image := Image{URL: SafeURL(embed.Thumbnail.URL)}
		embed.Thumbnail = &image
		if image.URL == "" {
			embed.Thumbnail = nil
		}
	}
	if embed.Timestamp != nil {
		at := embed.Timestamp.UTC()
		embed.Timestamp = &at
	}
	fields := make([]Field, 0, min(25, len(embed.Fields)))
	for _, field := range embed.Fields {
		if len(fields) == 25 || left < 2 {
			break
		}
		if strings.TrimSpace(field.Name) == "" || strings.TrimSpace(field.Value) == "" {
			continue
		}
		field.Name = text(field.Name, min(256, left-1))
		field.Value = text(field.Value, 1024)
		if field.Name != "" && field.Value != "" {
			fields = append(fields, field)
		}
	}
	embed.Fields = fields
	if embed.Title == "" && embed.Description == "" && len(fields) == 0 {
		return nil, ErrInvalidMessage
	}
	return json.Marshal(Message{Embeds: []Embed{embed}, AllowedMentions: mentions})
}

func SafeURL(value string) string {
	if len(value) > 2048 {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return ""
	}
	return value
}

func textUnits(value string) int {
	total := 0
	for _, r := range value {
		total++
		if r > 0xffff {
			total++
		}
	}
	return total
}

func clip(value string, limit int) string {
	if limit < 1 {
		return ""
	}
	value = strings.ToValidUTF8(value, "�")
	value = strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
	if textUnits(value) <= limit {
		return value
	}
	used := 0
	for index, r := range value {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if used+width > limit-1 {
			return value[:index] + "…"
		}
		used += width
	}
	return value
}
