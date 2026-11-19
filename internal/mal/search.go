package mal

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"
)

// Search fetches one bounded official catalogue page and returns at most five
// matching results. MAL has no native media-format filter on this endpoint.
func (c *Client) Search(ctx context.Context, kind, search, format string) ([]Media, error) {
	search = strings.TrimSpace(search)
	format = strings.ToLower(strings.TrimSpace(format))
	formats := []string{"tv", "movie", "ova", "ona", "special", "music", "tv_special"}
	if kind == "manga" {
		formats = []string{"manga", "novel", "one_shot", "doujinshi", "manhwa", "manhua", "oel", "light_novel"}
	}
	if (kind != "anime" && kind != "manga") || search == "" || !utf8.ValidString(search) || utf8.RuneCountInString(search) > 200 || (format != "" && !slices.Contains(formats, format)) {
		return nil, ErrMalformed
	}
	fields := "id,title,main_picture,media_type,status,mean,synopsis,genres"
	if kind == "anime" {
		fields += ",num_episodes,studios"
	} else {
		fields += ",num_chapters,num_volumes,authors{first_name,last_name}"
	}
	target := endpointURL(c.api.base, kind, url.Values{"q": {search}, "limit": {"25"}, "nsfw": {"true"}, "fields": {fields}})
	response, err := getHTTP(ctx, c.http, c.pacer, c.api, c.settings(), target, false)
	if err != nil {
		return nil, err
	}
	if response.status >= 300 {
		return nil, ErrMalformed
	}
	var envelope struct {
		Data []struct {
			Node *Media `json:"node"`
		} `json:"data"`
		Paging *struct{} `json:"paging"`
		Error  string    `json:"error"`
	}
	if json.Unmarshal(response.body, &envelope) != nil || envelope.Data == nil || len(envelope.Data) > 25 || envelope.Paging == nil || envelope.Error != "" {
		return nil, ErrMalformed
	}
	result := make([]Media, 0, 5)
	seen := map[int64]bool{}
	for _, item := range envelope.Data {
		if item.Node == nil || item.Node.ID <= 0 || strings.TrimSpace(item.Node.Title) == "" || seen[item.Node.ID] {
			return nil, ErrMalformed
		}
		seen[item.Node.ID] = true
		if len(result) < 5 && (format == "" || item.Node.Format == format) {
			result = append(result, *item.Node)
		}
	}
	return result, nil
}
