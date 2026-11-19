package mal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"kotantei/internal/update"
)

type EnricherOptions struct {
	Endpoint         string
	HTTPClient       *http.Client
	RequestInterval  time.Duration
	RequestTimeout   time.Duration
	MaxResponseBytes int64
	MaxEntries       int
	MaxBytes         int
	MaxEntryBytes    int
	TTL              time.Duration
	FailureTTL       time.Duration
	Now              func() time.Time
	Wait             func(context.Context, time.Duration) error
}
type cacheItem struct {
	raw     []byte
	cause   error
	expires time.Time
	used    uint64
	size    int
}
type Enricher struct {
	options  EnricherOptions
	http     *http.Client
	route    *httpRoute
	pacer    *requestPacer
	gate     chan struct{}
	cache    map[string]cacheItem
	bytes    int
	sequence uint64
}

// Close releases idle connections after optional lookups have joined.
func (e *Enricher) Close() { e.http.CloseIdleConnections() }

func NewEnricher(options EnricherOptions) (*Enricher, error) {
	if options.Endpoint == "" {
		options.Endpoint = "https://api.tenrai.org/v1"
	}
	if options.RequestInterval == 0 {
		options.RequestInterval = time.Second
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = 5 * time.Second
	}
	if options.MaxResponseBytes == 0 {
		options.MaxResponseBytes = 1 << 20
	}
	if options.MaxEntries == 0 {
		options.MaxEntries = 64
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = 1 << 20
	}
	if options.MaxEntryBytes == 0 {
		options.MaxEntryBytes = 64 << 10
	}
	if options.TTL == 0 {
		options.TTL = 6 * time.Hour
	}
	if options.FailureTTL == 0 {
		options.FailureTTL = time.Minute
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Wait == nil {
		options.Wait = waitFor
	}
	base, err := parseEndpoint(options.Endpoint)
	if err != nil {
		return nil, err
	}
	if options.RequestInterval <= 0 || options.RequestTimeout <= 0 || options.MaxResponseBytes < 1 || options.MaxResponseBytes > 16<<20 || options.MaxEntries < 1 || options.MaxBytes < 1 || options.MaxEntryBytes < 1 || options.TTL <= 0 || options.FailureTTL <= 0 {
		return nil, errors.New("invalid enrichment limits")
	}
	return &Enricher{options: options, http: publicHTTPClient(options.HTTPClient, options.RequestTimeout), route: &httpRoute{base: base}, pacer: newPacer(), gate: make(chan struct{}, 1), cache: map[string]cacheItem{}}, nil
}

// Lookup is public-catalogue-only. The cache stores bounded encoded values and
// returns detached copies; failures have a short TTL and never trigger retries.
func (e *Enricher) Lookup(parent context.Context, subject update.Subject, flags Features) (Enrichment, error) {
	if err := subject.Validate(); err != nil {
		return Enrichment{}, err
	}
	switch subject.Kind {
	case update.AnimeSubject, update.MangaSubject, update.CharacterSubject, update.PersonSubject, update.CompanySubject:
	default:
		return Enrichment{}, ErrMalformed
	}
	ctx, cancel := context.WithTimeout(parent, e.options.RequestTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Enrichment{}, err
	}
	select {
	case e.gate <- struct{}{}:
	case <-ctx.Done():
		return Enrichment{}, ctx.Err()
	}
	defer func() { <-e.gate }()
	key := string(subject.Kind) + ":" + subject.ID + ":" + strconv.FormatUint(uint64(flags&(Synopsis|Seiyu)), 10)
	now := e.options.Now()
	for key, item := range e.cache {
		if !item.expires.After(now) {
			e.bytes -= item.size
			delete(e.cache, key)
		}
	}
	e.sequence++
	if item, ok := e.cache[key]; ok {
		item.used = e.sequence
		e.cache[key] = item
		if item.cause != nil {
			return Enrichment{}, item.cause
		}
		var value Enrichment
		if json.Unmarshal(item.raw, &value) != nil {
			return Enrichment{}, ErrMalformed
		}
		return value, nil
	}
	value, err := e.lookup(ctx, subject, flags)
	if ctx.Err() != nil {
		return Enrichment{}, ctx.Err()
	}
	// A caller's quota is not an upstream failure and must not be cached for
	// other accounts sharing this enricher.
	if errors.Is(err, ErrCollectionBudget) {
		return Enrichment{}, err
	}
	var raw []byte
	if err == nil {
		raw, err = json.Marshal(value)
		if err == nil && len(raw) > e.options.MaxEntryBytes {
			err = ErrResponseBudget
			raw = nil
		}
	}
	item := cacheItem{raw: raw, cause: err, expires: e.options.Now().Add(e.options.TTL), used: e.sequence, size: len(key) + len(raw) + 32}
	if err != nil {
		item.expires = e.options.Now().Add(e.options.FailureTTL)
		item.raw = nil
		item.size = len(key) + 32
	}
	if item.size <= e.options.MaxBytes {
		for len(e.cache) >= e.options.MaxEntries || e.bytes > e.options.MaxBytes-item.size {
			var oldest string
			var used uint64
			for key, item := range e.cache {
				if oldest == "" || item.used < used {
					oldest = key
					used = item.used
				}
			}
			if oldest == "" {
				break
			}
			e.bytes -= e.cache[oldest].size
			delete(e.cache, oldest)
		}
		e.cache[key] = item
		e.bytes += item.size
	}
	if err != nil {
		return Enrichment{}, err
	}
	return value, nil
}

func (e *Enricher) get(ctx context.Context, path string, query url.Values, result any) error {
	settings := requestSettings{interval: e.options.RequestInterval, timeout: e.options.RequestTimeout, maxBytes: e.options.MaxResponseBytes, now: e.options.Now, wait: e.options.Wait}
	response, err := getHTTP(ctx, e.http, e.pacer, e.route, settings, endpointURL(e.route.base, path, query), true)
	if err != nil {
		return err
	}
	if response.status >= 300 {
		return ErrMalformed
	}
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
	}
	if json.Unmarshal(response.body, &envelope) != nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" || envelope.Error != "" || json.Unmarshal(envelope.Data, result) != nil {
		return ErrMalformed
	}
	return nil
}

type tenraiName struct {
	ID   int64  `json:"mal_id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}
type tenraiReference struct {
	ID    int64  `json:"mal_id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}
type tenraiCredit struct {
	Role      string          `json:"role"`
	Anime     tenraiReference `json:"anime"`
	Manga     tenraiReference `json:"manga"`
	Character struct {
		Name string `json:"name"`
	} `json:"character"`
}
type tenraiEntity struct {
	ID     int64          `json:"mal_id"`
	About  string         `json:"about"`
	Anime  []tenraiCredit `json:"anime"`
	Manga  []tenraiCredit `json:"manga"`
	Voices []tenraiCredit `json:"voices"`
}
type tenraiMediaData struct {
	ID       int64   `json:"mal_id"`
	Title    string  `json:"title"`
	Format   string  `json:"type"`
	Status   string  `json:"status"`
	Episodes int     `json:"episodes"`
	Chapters int     `json:"chapters"`
	Volumes  int     `json:"volumes"`
	Score    float64 `json:"score"`
	Synopsis string  `json:"synopsis"`
	Images   struct {
		JPG struct {
			URL   string `json:"image_url"`
			Large string `json:"large_image_url"`
		} `json:"jpg"`
	} `json:"images"`
	Themes       []tenraiName `json:"themes"`
	Demographics []tenraiName `json:"demographics"`
	Genres       []tenraiName `json:"genres"`
	Studios      []tenraiName `json:"studios"`
	Authors      []tenraiName `json:"authors"`
}

func (e *Enricher) lookup(ctx context.Context, subject update.Subject, flags Features) (Enrichment, error) {
	value := Enrichment{}
	if subject.Kind == update.AnimeSubject || subject.Kind == update.MangaSubject {
		var data tenraiMediaData
		if err := e.get(ctx, string(subject.Kind)+"/"+subject.ID, nil, &data); err != nil {
			return value, err
		}
		if strconv.FormatInt(data.ID, 10) != subject.ID || data.Title == "" {
			return value, ErrMalformed
		}
		media := &Media{ID: data.ID, Title: data.Title, Format: strings.ToLower(strings.ReplaceAll(data.Format, " ", "_")), Status: strings.ToLower(strings.ReplaceAll(data.Status, " ", "_")), Episodes: data.Episodes, Chapters: data.Chapters, Volumes: data.Volumes, Mean: data.Score, Synopsis: data.Synopsis, Picture: Picture{Large: data.Images.JPG.Large, Medium: data.Images.JPG.URL}}
		for _, genre := range data.Genres {
			media.Genres = append(media.Genres, Catalog{ID: genre.ID, Name: genre.Name})
		}
		for _, studio := range data.Studios {
			media.Studios = append(media.Studios, Catalog{ID: studio.ID, Name: studio.Name})
		}
		for _, person := range data.Authors {
			if person.ID <= 0 || person.Name == "" {
				continue
			}
			author := Author{Role: "Author"}
			author.Node.ID = person.ID
			parts := strings.SplitN(person.Name, ",", 2)
			author.Node.LastName = strings.TrimSpace(parts[0])
			if len(parts) == 2 {
				author.Node.FirstName = strings.TrimSpace(parts[1])
			}
			media.Authors = append(media.Authors, author)
		}
		value.Media = media
		for _, theme := range data.Themes {
			if theme.Name != "" {
				value.Themes = append(value.Themes, theme.Name)
			}
		}
		for _, demographic := range data.Demographics {
			if demographic.Name != "" {
				value.Demographics = append(value.Demographics, demographic.Name)
			}
		}
		if subject.Kind == update.AnimeSubject && flags&Seiyu != 0 {
			var rows []struct {
				VoiceActors []struct {
					Language string     `json:"language"`
					Person   tenraiName `json:"person"`
				} `json:"voice_actors"`
			}
			if err := e.get(ctx, "anime/"+subject.ID+"/characters", nil, &rows); err != nil {
				return Enrichment{}, err
			}
			seen := map[int64]bool{}
			for _, row := range rows {
				for _, actor := range row.VoiceActors {
					if actor.Language != "Japanese" || actor.Person.ID <= 0 || actor.Person.Name == "" || seen[actor.Person.ID] || !catalogURL(actor.Person.URL) {
						continue
					}
					seen[actor.Person.ID] = true
					value.VoiceActors = append(value.VoiceActors, NamedLink{Name: actor.Person.Name, URL: actor.Person.URL})
					if len(value.VoiceActors) == 7 {
						break
					}
				}
				if len(value.VoiceActors) == 7 {
					break
				}
			}
		}
		return value, nil
	}
	if subject.Kind == update.CompanySubject {
		var data []tenraiReference
		if err := e.get(ctx, "anime", url.Values{"producers": {subject.ID}, "order_by": {"members"}, "sort": {"desc"}, "limit": {"1"}}, &data); err != nil {
			return value, err
		}
		if len(data) > 1 {
			return value, ErrMalformed
		}
		if len(data) > 0 {
			value.KnownFor = workOf(data[0], "")
		}
		return value, nil
	}
	path := "characters/"
	if subject.Kind == update.PersonSubject {
		path = "people/"
	}
	var data tenraiEntity
	if err := e.get(ctx, path+subject.ID+"/full", nil, &data); err != nil {
		return value, err
	}
	if strconv.FormatInt(data.ID, 10) != subject.ID {
		return value, ErrMalformed
	}
	if flags&Synopsis != 0 {
		value.Description = data.About
	}
	for _, group := range [][]tenraiCredit{data.Voices, data.Anime, data.Manga} {
		for _, credit := range group {
			reference := credit.Anime
			if reference.Title == "" {
				reference = credit.Manga
			}
			if work := workOf(reference, credit.Character.Name); work != nil {
				value.KnownFor = work
				break
			}
		}
		if value.KnownFor != nil {
			break
		}
	}
	return value, nil
}

func catalogURL(raw string) bool {
	u, err := siteURL(raw)
	return err == nil && u.IsAbs() && raw == u.String()
}
func workOf(reference tenraiReference, character string) *Work {
	if reference.Title == "" || !catalogURL(reference.URL) {
		return nil
	}
	return &Work{Title: reference.Title, URL: reference.URL, Character: character}
}
