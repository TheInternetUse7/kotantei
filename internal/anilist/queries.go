package anilist

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
)

const titleFields = `title { romaji english native romajiStyled:romaji(stylised:true) englishStyled:english(stylised:true) nativeStyled:native(stylised:true) }`
const coreMediaFields = `id type siteUrl format status countryOfOrigin episodes chapters volumes averageScore coverImage { large extraLarge } ` + titleFields
const enrichmentFields = `
description(asHtml:false) @include(if:$description)
genres @include(if:$genres)
tags @include(if:$tags) { name rank isGeneralSpoiler isMediaSpoiler }
studios(isMain:true) @include(if:$studios) { nodes { id name isAnimationStudio } }
staff(perPage:25,sort:RELEVANCE) @include(if:$staff) { edges { role node { id name { full native } siteUrl } } }
characters(perPage:7,role:MAIN) @include(if:$voices) { edges { voiceActors(language:JAPANESE,sort:RELEVANCE) { id name { full native } siteUrl } } }
`
const enrichmentVariables = `$description:Boolean!,$genres:Boolean!,$tags:Boolean!,$studios:Boolean!,$staff:Boolean!,$voices:Boolean!`

func (c *Client) profile(ctx context.Context, id int64, name string) (User, error) {
	query := `query KotanteiProfile($id:Int,$name:String) { User(id:$id,name:$name) { id name siteUrl avatar { large } options { titleLanguage } mediaListOptions { scoreFormat animeList { advancedScoringEnabled } mangaList { advancedScoringEnabled } } } }`
	// AniList treats an explicitly null selector as a filter. Omit the unused
	// variable so an ID lookup does not also require name:null (and vice versa).
	variables := map[string]any{}
	if id > 0 {
		variables["id"] = id
	} else {
		variables["name"] = name
	}
	var response struct {
		User *User `json:"User"`
	}
	if err := c.query(ctx, "KotanteiProfile", query, variables, &response); err != nil {
		return User{}, err
	}
	if response.User == nil {
		return User{}, ErrNotFound
	}
	user := *response.User
	if user.ID < 1 || user.ID > math.MaxInt32 || strings.TrimSpace(user.Name) == "" || len(user.Name) > 256 || (id > 0 && user.ID != id) {
		return User{}, ErrMalformed
	}
	if user.Options.TitleLanguage == "" {
		user.Options.TitleLanguage = "ROMAJI"
	}
	if !slices.Contains([]string{"POINT_100", "POINT_10_DECIMAL", "POINT_10", "POINT_5", "POINT_3"}, user.MediaListOptions.ScoreFormat) {
		user.MediaListOptions.ScoreFormat = "POINT_100"
	}
	return user, nil
}

func (c *Client) activityPage(ctx context.Context, id, since, until int64, number, size int, kind string) (page[Activity], error) {
	query := `query KotanteiActivities($id:Int!,$since:Int!,$until:Int!,$page:Int!,$perPage:Int!,$type:ActivityType!) {
Page(page:$page,perPage:$perPage) { pageInfo { currentPage hasNextPage } items:activities(userId:$id,type:$type,createdAt_greater:$since,createdAt_lesser:$until,sort:ID_DESC) { __typename ... on ListActivity { id userId createdAt status progress media { id type } } } } }`
	var response struct {
		Page *page[Activity] `json:"Page"`
	}
	err := c.query(ctx, "KotanteiActivities", query, map[string]any{"id": id, "since": since, "until": until, "page": number, "perPage": size, "type": kind}, &response)
	if err != nil {
		return page[Activity]{}, err
	}
	return checkedPage(response.Page, number)
}

func (c *Client) reviewPage(ctx context.Context, id int64, number, size int) (page[Review], error) {
	query := `query KotanteiReviews($id:Int!,$page:Int!,$perPage:Int!) { Page(page:$page,perPage:$perPage) { pageInfo { currentPage hasNextPage } items:reviews(userId:$id,sort:CREATED_AT_DESC) { id userId createdAt private summary siteUrl media { ` + coreMediaFields + ` } } } }`
	var response struct {
		Page *page[Review] `json:"Page"`
	}
	if err := c.query(ctx, "KotanteiReviews", query, map[string]any{"id": id, "page": number, "perPage": size}, &response); err != nil {
		return page[Review]{}, err
	}
	return checkedPage(response.Page, number)
}

func (c *Client) listEntries(ctx context.Context, id int64, ids []int64, size int, scoreFormat string) ([]Entry, error) {
	query := `query KotanteiEntries($id:Int!,$ids:[Int!]!,$perPage:Int!,$scoreFormat:ScoreFormat!) { Page(page:1,perPage:$perPage) { pageInfo { currentPage hasNextPage } items:mediaList(userId:$id,mediaId_in:$ids) { userId mediaId private status score(format:$scoreFormat) repeat notes advancedScores customLists(asArray:true) media { ` + coreMediaFields + ` } } } }`
	var response struct {
		Page *page[Entry] `json:"Page"`
	}
	if err := c.query(ctx, "KotanteiEntries", query, map[string]any{"id": id, "ids": ids, "perPage": size, "scoreFormat": scoreFormat}, &response); err != nil {
		return nil, err
	}
	data, err := checkedPage(response.Page, 1)
	if err != nil {
		return nil, err
	}
	if *data.PageInfo.HasNextPage {
		return nil, ErrMalformed
	}
	return data.Items, nil
}

type favoriteID struct {
	ID int64 `json:"id"`
}
type favoritesResponse struct {
	User *struct {
		ID         int64                        `json:"id"`
		Favourites map[string]*page[favoriteID] `json:"favourites"`
	} `json:"User"`
}

func (c *Client) favoritePages(ctx context.Context, id int64, pages map[string]int, size int) (map[string]*page[favoriteID], error) {
	query := `query KotanteiFavorites($id:Int!,$perPage:Int!,$animePage:Int!,$animeEnabled:Boolean!,$mangaPage:Int!,$mangaEnabled:Boolean!,$charactersPage:Int!,$charactersEnabled:Boolean!,$staffPage:Int!,$staffEnabled:Boolean!,$studiosPage:Int!,$studiosEnabled:Boolean!) { User(id:$id) { id favourites {`
	variables := map[string]any{"id": id, "perPage": size}
	for _, kind := range favoriteKinds {
		number, active := pages[kind]
		if !active {
			number = 1
		}
		variables[kind+"Page"] = number
		variables[kind+"Enabled"] = active
		query += fmt.Sprintf(` %s(page:$%sPage,perPage:$perPage) @include(if:$%sEnabled) { pageInfo { currentPage hasNextPage } items:nodes { id } }`, kind, kind, kind)
	}
	query += ` } } }`
	var response favoritesResponse
	if err := c.query(ctx, "KotanteiFavorites", query, variables, &response); err != nil {
		return nil, err
	}
	if response.User == nil || response.User.ID != id || response.User.Favourites == nil {
		return nil, ErrMalformed
	}
	return response.User.Favourites, nil
}

func enrichmentFlags(flags Features) map[string]any {
	return map[string]any{"description": flags&Description != 0, "genres": flags&Genres != 0, "tags": flags&Tags != 0, "studios": flags&Studio != 0, "staff": flags&(Mangaka|Director) != 0, "voices": flags&VoiceActors != 0}
}

func (c *Client) mediaEnrichment(ctx context.Context, ids []int64, size int, flags Features) ([]Media, error) {
	query := `query KotanteiEnrichment($ids:[Int!]!,$perPage:Int!,` + enrichmentVariables + `) { Page(page:1,perPage:$perPage) { pageInfo { currentPage hasNextPage } items:media(id_in:$ids) { id ` + enrichmentFields + ` } } }`
	variables := enrichmentFlags(flags)
	variables["ids"] = ids
	variables["perPage"] = size
	var response struct {
		Page *page[Media] `json:"Page"`
	}
	if err := c.query(ctx, "KotanteiEnrichment", query, variables, &response); err != nil {
		return nil, err
	}
	data, err := checkedPage(response.Page, 1)
	if err != nil {
		return nil, err
	}
	if *data.PageInfo.HasNextPage {
		return nil, ErrMalformed
	}
	return data.Items, nil
}

func (c *Client) favoriteDetails(ctx context.Context, kind string, ids []int64, size int, flags Features) ([]FavoriteInfo, error) {
	operation := "KotanteiFavoriteDetails" + sentence(kind)
	mediaFields := coreMediaFields + enrichmentFields
	var selection string
	switch kind {
	case "anime", "manga":
		selection = fmt.Sprintf(`media(id_in:$ids,type:%s) { %s }`, strings.ToUpper(kind), mediaFields)
	case "characters":
		selection = `characters(id_in:$ids) { id siteUrl name { full native } image { large } description(asHtml:false) @include(if:$description) knownFor:media(perPage:1,sort:POPULARITY_DESC) { nodes { ` + mediaFields + ` } } }`
	case "staff":
		selection = `staff(id_in:$ids) { id siteUrl name { full native } image { large } primaryOccupations description(asHtml:false) @include(if:$description) knownFor:staffMedia(perPage:1,sort:POPULARITY_DESC) { nodes { ` + mediaFields + ` } } }`
	case "studios":
		selection = `studios(id_in:$ids) { id siteUrl label:name knownFor:media(perPage:1,sort:POPULARITY_DESC) { nodes { ` + mediaFields + ` } } }`
	default:
		return nil, ErrMalformed
	}
	query := `query ` + operation + `($ids:[Int!]!,$perPage:Int!,` + enrichmentVariables + `) { Page(page:1,perPage:$perPage) { pageInfo { currentPage hasNextPage } items:` + selection + ` } }`
	variables := enrichmentFlags(flags)
	variables["ids"] = ids
	variables["perPage"] = size
	var response struct {
		Page *page[FavoriteInfo] `json:"Page"`
	}
	if err := c.query(ctx, operation, query, variables, &response); err != nil {
		return nil, err
	}
	data, err := checkedPage(response.Page, 1)
	if err != nil {
		return nil, err
	}
	if *data.PageInfo.HasNextPage {
		return nil, ErrMalformed
	}
	return data.Items, nil
}

func checkedPage[T any](value *page[T], number int) (page[T], error) {
	if value == nil || value.PageInfo == nil || value.PageInfo.HasNextPage == nil || value.PageInfo.CurrentPage == nil || *value.PageInfo.CurrentPage != number || value.Items == nil || (len(value.Items) == 0 && *value.PageInfo.HasNextPage) {
		return page[T]{}, ErrMalformed
	}
	return *value, nil
}
