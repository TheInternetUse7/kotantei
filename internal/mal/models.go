// Package mal tracks public MyAnimeList lists and profiles.
package mal

import (
	"time"

	"kotantei/internal/store"
)

type Features uint64

const (
	AnimeList Features = 1 << iota
	MangaList
	Favorites
	Mention
	Website
	MediaFormat
	MediaStatus
	Genres
	Synopsis
	Studio
	Mangakas
	Tags
	Comments
	Dates
	Themes
	Demographic
	Seiyu
)

const DefaultFeatures Features = 127

type Picture struct {
	Medium string `json:"medium"`
	Large  string `json:"large"`
}
type Catalog struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}
type Author struct {
	Node struct {
		ID        int64  `json:"id"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	} `json:"node"`
	Role string `json:"role"`
}
type Media struct {
	ID       int64     `json:"id"`
	Title    string    `json:"title"`
	Picture  Picture   `json:"main_picture"`
	Format   string    `json:"media_type"`
	Status   string    `json:"status"`
	Episodes int       `json:"num_episodes"`
	Chapters int       `json:"num_chapters"`
	Volumes  int       `json:"num_volumes"`
	Mean     float64   `json:"mean"`
	Synopsis string    `json:"synopsis"`
	Genres   []Catalog `json:"genres"`
	Studios  []Catalog `json:"studios"`
	Authors  []Author  `json:"authors"`
}
type ListStatus struct {
	Status          string    `json:"status"`
	Score           *int      `json:"score"`
	UpdatedAt       time.Time `json:"updated_at"`
	EpisodesWatched *int      `json:"num_episodes_watched"`
	ChaptersRead    *int      `json:"num_chapters_read"`
	VolumesRead     *int      `json:"num_volumes_read"`
	Rewatching      bool      `json:"is_rewatching"`
	Rereading       bool      `json:"is_rereading"`
	TimesRewatched  int       `json:"num_times_rewatched"`
	TimesReread     int       `json:"num_times_reread"`
	Tags            []string  `json:"tags"`
	Comments        string    `json:"comments"`
	StartDate       string    `json:"start_date"`
	FinishDate      string    `json:"finish_date"`
}
type Entry struct {
	Node       *Media      `json:"node"`
	ListStatus *ListStatus `json:"list_status"`
}
type Profile struct {
	ID                int64
	Username          string
	AvatarURL         string
	Favorites         []store.Favorite
	FavoritesComplete bool
}
type FavoriteMetadata struct {
	Format string `json:"format,omitempty"`
	Year   int    `json:"year,omitempty"`
	From   string `json:"from,omitempty"`
}
type NamedLink struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}
type Work struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Character string `json:"character,omitempty"`
}
type Enrichment struct {
	Media        *Media      `json:"media,omitempty"`
	Themes       []string    `json:"themes,omitempty"`
	Demographics []string    `json:"demographics,omitempty"`
	VoiceActors  []NamedLink `json:"voice_actors,omitempty"`
	Description  string      `json:"description,omitempty"`
	KnownFor     *Work       `json:"known_for,omitempty"`
}
