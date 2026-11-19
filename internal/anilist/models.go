package anilist

type Features uint64

const (
	AnimeList Features = 1 << iota
	MangaList
	Favorites
	Mention
	Website
	MediaFormat
	MediaStatus
	Description
	Genres
	Tags
	Studio
	Mangaka
	Reviews
	CustomLists
	Director
	VoiceActors
)
const DefaultFeatures Features = 127

type Name struct {
	Full   string `json:"full"`
	Native string `json:"native"`
}
type Title struct {
	Romaji        string `json:"romaji"`
	English       string `json:"english"`
	Native        string `json:"native"`
	RomajiStyled  string `json:"romajiStyled"`
	EnglishStyled string `json:"englishStyled"`
	NativeStyled  string `json:"nativeStyled"`
}
type Image struct {
	Large      string `json:"large"`
	ExtraLarge string `json:"extraLarge"`
}
type UserOptions struct {
	TitleLanguage string `json:"titleLanguage"`
}
type TypeOptions struct {
	AdvancedScoringEnabled bool `json:"advancedScoringEnabled"`
}
type ListOptions struct {
	ScoreFormat string      `json:"scoreFormat"`
	AnimeList   TypeOptions `json:"animeList"`
	MangaList   TypeOptions `json:"mangaList"`
}
type User struct {
	ID               int64       `json:"id"`
	Name             string      `json:"name"`
	SiteURL          string      `json:"siteUrl"`
	Avatar           Image       `json:"avatar"`
	Options          UserOptions `json:"options"`
	MediaListOptions ListOptions `json:"mediaListOptions"`
}
type Tag struct {
	Name             string `json:"name"`
	Rank             int    `json:"rank"`
	IsMediaSpoiler   bool   `json:"isMediaSpoiler"`
	IsGeneralSpoiler bool   `json:"isGeneralSpoiler"`
}
type Person struct {
	ID      int64  `json:"id"`
	Name    Name   `json:"name"`
	SiteURL string `json:"siteUrl"`
}
type StaffEdge struct {
	Role string `json:"role"`
	Node Person `json:"node"`
}
type CharacterEdge struct {
	VoiceActors []Person `json:"voiceActors"`
}
type StudioNode struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	IsAnimationStudio bool   `json:"isAnimationStudio"`
}
type Media struct {
	ID              int64    `json:"id"`
	Type            string   `json:"type"`
	Title           Title    `json:"title"`
	SiteURL         string   `json:"siteUrl"`
	Format          string   `json:"format"`
	Status          string   `json:"status"`
	CountryOfOrigin string   `json:"countryOfOrigin"`
	CoverImage      Image    `json:"coverImage"`
	Episodes        int      `json:"episodes"`
	Chapters        int      `json:"chapters"`
	Volumes         int      `json:"volumes"`
	AverageScore    int      `json:"averageScore"`
	Description     string   `json:"description"`
	Genres          []string `json:"genres"`
	Tags            []Tag    `json:"tags"`
	Studios         struct {
		Nodes []StudioNode `json:"nodes"`
	} `json:"studios"`
	Staff struct {
		Edges []StaffEdge `json:"edges"`
	} `json:"staff"`
	Characters struct {
		Edges []CharacterEdge `json:"edges"`
	} `json:"characters"`
}
type Activity struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"userId"`
	Typename  string `json:"__typename"`
	CreatedAt int64  `json:"createdAt"`
	Status    string `json:"status"`
	Progress  string `json:"progress"`
	IsPrivate bool   `json:"isPrivate"`
	Media     *Media `json:"media"`
}
type CustomList struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}
type Entry struct {
	UserID         int64              `json:"userId"`
	MediaID        int64              `json:"mediaId"`
	Private        bool               `json:"private"`
	Status         string             `json:"status"`
	Score          float64            `json:"score"`
	Repeat         int                `json:"repeat"`
	Notes          string             `json:"notes"`
	AdvancedScores map[string]float64 `json:"advancedScores"`
	CustomLists    []CustomList       `json:"customLists"`
	Media          *Media             `json:"media"`
}
type Review struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"userId"`
	CreatedAt int64  `json:"createdAt"`
	Private   bool   `json:"private"`
	SiteURL   string `json:"siteUrl"`
	Summary   string `json:"summary"`
	Media     *Media `json:"media"`
}
type FavoriteInfo struct {
	Media
	Name               Name     `json:"name"`
	Label              string   `json:"label"`
	Image              Image    `json:"image"`
	PrimaryOccupations []string `json:"primaryOccupations"`
	KnownFor           struct {
		Nodes []Media `json:"nodes"`
	} `json:"knownFor"`
}
type PageInfo struct {
	CurrentPage *int  `json:"currentPage"`
	HasNextPage *bool `json:"hasNextPage"`
}
type page[T any] struct {
	PageInfo *PageInfo `json:"pageInfo"`
	Items    []T       `json:"items"`
}
