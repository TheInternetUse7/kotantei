package anilist

import (
	"context"
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

// Search returns at most five public catalogue results. It shares the client's
// tracking/linking limiter and uses no account or authenticated list endpoints.
func (c *Client) Search(ctx context.Context, kind, search, format string) ([]Media, error) {
	search = strings.TrimSpace(search)
	format = strings.ToUpper(strings.TrimSpace(format))
	formats := []string{"TV", "TV_SHORT", "MOVIE", "SPECIAL", "OVA", "ONA", "MUSIC"}
	if kind == "manga" {
		formats = []string{"MANGA", "NOVEL", "ONE_SHOT"}
	}
	if (kind != "anime" && kind != "manga") || search == "" || !utf8.ValidString(search) || utf8.RuneCountInString(search) > 200 || (format != "" && !slices.Contains(formats, format)) {
		return nil, ErrMalformed
	}
	variables := map[string]any{"search": search, "type": strings.ToUpper(kind), "format": nil, "perPage": 5}
	if format != "" {
		variables["format"] = format
	}
	query := `query KotanteiSearch($search:String!,$type:MediaType!,$format:MediaFormat,$perPage:Int!) { Page(page:1,perPage:$perPage) { pageInfo { currentPage hasNextPage } items:media(search:$search,type:$type,format:$format,sort:SEARCH_MATCH) { ` + coreMediaFields + ` description(asHtml:false) genres } } }`
	var response struct {
		Page *page[Media] `json:"Page"`
	}
	if err := c.query(ctx, "KotanteiSearch", query, variables, &response); err != nil {
		return nil, err
	}
	data, err := checkedPage(response.Page, 1)
	if err != nil {
		return nil, err
	}
	if len(data.Items) > 5 {
		return nil, ErrMalformed
	}
	seen := map[int64]bool{}
	for _, media := range data.Items {
		if media.ID <= 0 || media.ID > math.MaxInt32 || media.Type != strings.ToUpper(kind) || seen[media.ID] || strings.TrimSpace(media.Title.Romaji+media.Title.English+media.Title.Native) == "" || (format != "" && media.Format != format) {
			return nil, ErrMalformed
		}
		seen[media.ID] = true
	}
	return data.Items, nil
}
