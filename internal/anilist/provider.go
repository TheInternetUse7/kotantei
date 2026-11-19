package anilist

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"kotantei/internal/store"
	"kotantei/internal/tracking"
	"kotantei/internal/update"
)

var ErrCollectionBudget = tracking.ErrCollectionBudget

type ProviderOptions struct {
	PageSize     int
	MaxPages     int
	MaxRecords   int
	MaxFavorites int
	MaxBytes     int
	CheckTimeout time.Duration
	Now          func() time.Time
}
type Provider struct {
	queue   *store.Outbox
	client  *Client
	options ProviderOptions
}
type CheckResult struct {
	store.PollResult
	MetadataFailures int
}

var favoriteKinds = []string{"anime", "manga", "characters", "staff", "studios"}

func NewProvider(queue *store.Outbox, client *Client, options ProviderOptions) (*Provider, error) {
	if queue == nil || client == nil {
		return nil, errors.New("AniList provider requires an outbox and client")
	}
	if options.PageSize == 0 {
		options.PageSize = 25
	}
	if options.MaxPages == 0 {
		options.MaxPages = 64
	}
	if options.MaxRecords == 0 {
		options.MaxRecords = 512
	}
	if options.MaxFavorites == 0 {
		options.MaxFavorites = 5000
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = 4 << 20
	}
	if options.CheckTimeout == 0 {
		options.CheckTimeout = 3 * time.Minute
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.PageSize < 1 || options.PageSize > 25 || options.MaxPages < 1 || options.MaxRecords < 1 || options.MaxFavorites < 1 || options.MaxBytes < 1 || options.CheckTimeout <= 0 {
		return nil, errors.New("invalid AniList collection limits")
	}
	limits := queue.Limits()
	options.MaxRecords = min(options.MaxRecords, limits.MaxRecords)
	options.MaxFavorites = min(options.MaxFavorites, limits.MaxFavorites)
	options.MaxBytes = min(options.MaxBytes, limits.MaxBatchBytes)
	return &Provider{queue: queue, client: client, options: options}, nil
}

// Link silently baselines a new public account. Existing accounts only gain
// the shared guild membership; their identity/settings/cursors are preserved.
func (p *Provider) Link(parent context.Context, userID, guildID, username string) (store.LinkResult, error) {
	ctx, cancel := context.WithTimeout(parent, p.options.CheckTimeout)
	defer cancel()
	if !discordID(userID) || !discordID(guildID) {
		return store.LinkResult{}, store.ErrInvalidPoll
	}
	state, err := p.queue.ForUser(ctx, userID, "anilist")
	if err == nil {
		return p.queue.Connect(ctx, store.AccountLink{Provider: "anilist", ExternalID: state.ExternalID, UserID: userID, GuildID: guildID, Username: state.Username})
	}
	if !errors.Is(err, store.ErrAccountNotFound) {
		return store.LinkResult{}, err
	}
	username = strings.TrimSpace(username)
	if username == "" || len(username) > 256 {
		return store.LinkResult{}, ErrMalformed
	}
	fence, err := p.fence()
	if err != nil {
		return store.LinkResult{}, err
	}
	budget := collectionBudget{options: p.options}
	if err := budget.request(); err != nil {
		return store.LinkResult{}, err
	}
	user, err := p.client.profile(ctx, 0, username)
	if err != nil {
		return store.LinkResult{}, err
	}
	if err := budget.retain(user); err != nil {
		return store.LinkResult{}, err
	}
	activities, err := p.activities(ctx, user.ID, update.Cursor{Value: fence}, fence, AnimeList|MangaList, &budget)
	if err != nil {
		return store.LinkResult{}, err
	}
	reviews, err := p.reviews(ctx, user.ID, update.Cursor{Value: fence}, fence, &budget)
	if err != nil {
		return store.LinkResult{}, err
	}
	favorites, err := p.favorites(ctx, user.ID, nil, &budget)
	if err != nil {
		return store.LinkResult{}, err
	}
	activityIDs := []string{}
	for _, activity := range activities {
		if activity.CreatedAt == fence {
			activityIDs = append(activityIDs, strconv.FormatInt(activity.ID, 10))
		}
	}
	reviewIDs := []string{}
	for _, review := range reviews {
		if review.CreatedAt == fence {
			reviewIDs = append(reviewIDs, strconv.FormatInt(review.ID, 10))
		}
	}
	return p.queue.Connect(ctx, store.AccountLink{Provider: "anilist", ExternalID: strconv.FormatInt(user.ID, 10), UserID: userID, GuildID: guildID, Username: user.Name, Favorites: favorites, Cursors: map[update.Stream]update.Cursor{update.Activity: {Value: fence, SourceIDs: activityIDs}, update.ReviewStream: {Value: fence, SourceIDs: reviewIDs}}})
}

// Check stages every required stream before recording anything. Display-only
// enrichment is best effort; required GraphQL failures never advance cursors.
func (p *Provider) Check(parent context.Context, accountID int64) (CheckResult, error) {
	ctx, cancel := context.WithTimeout(parent, p.options.CheckTimeout)
	defer cancel()
	state, err := p.queue.Snapshot(ctx, accountID)
	if err != nil {
		return CheckResult{}, err
	}
	if state.Provider != "anilist" {
		return CheckResult{}, store.ErrInvalidPoll
	}
	id, err := strconv.ParseInt(state.ExternalID, 10, 32)
	if err != nil || id < 1 {
		return CheckResult{}, ErrMalformed
	}
	fence, err := p.fence()
	if err != nil {
		return CheckResult{}, err
	}
	flags := Features(state.Features)
	budget := collectionBudget{options: p.options}
	if err := budget.request(); err != nil {
		return CheckResult{}, err
	}
	user, err := p.client.profile(ctx, id, "")
	if err != nil {
		return CheckResult{}, err
	}
	if err := budget.retain(user); err != nil {
		return CheckResult{}, err
	}
	var activities []Activity
	var reviews []Review
	var favorites []store.Favorite
	if flags&(AnimeList|MangaList) != 0 {
		activities, err = p.activities(ctx, id, state.Cursors[update.Activity], fence, flags, &budget)
		if err != nil {
			return CheckResult{}, err
		}
	}
	if flags&Reviews != 0 {
		reviews, err = p.reviews(ctx, id, state.Cursors[update.ReviewStream], fence, &budget)
		if err != nil {
			return CheckResult{}, err
		}
	}
	if flags&Favorites != 0 {
		favorites, err = p.favorites(ctx, id, state.Favorites, &budget)
		if err != nil {
			return CheckResult{}, err
		}
	}
	input := store.Poll{AccountID: accountID, ExpectedRevision: state.Revision, Now: time.Unix(fence, 0).UTC(), Username: &user.Name}
	result := CheckResult{}
	latest := map[int64]Activity{}
	for _, activity := range activities {
		observation := activityObservation(activity)
		if state.Cursors[update.Activity].Contains(observation) {
			continue
		}
		if activity.Media == nil || activity.IsPrivate {
			input.Observations = append(input.Observations, observation)
			continue
		}
		if previous, ok := latest[activity.Media.ID]; ok {
			if newerActivity(activity, previous) {
				input.Observations = append(input.Observations, activityObservation(previous))
				latest[activity.Media.ID] = activity
			} else {
				input.Observations = append(input.Observations, observation)
			}
		} else {
			latest[activity.Media.ID] = activity
		}
	}
	ids := make([]int64, 0, len(latest))
	for id := range latest {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	entries := map[int64]Entry{}
	for start := 0; start < len(ids); start += p.options.PageSize {
		chunk := ids[start:min(len(ids), start+p.options.PageSize)]
		if err := budget.request(); err != nil {
			return CheckResult{}, err
		}
		rows, err := p.client.listEntries(ctx, id, chunk, p.options.PageSize, user.MediaListOptions.ScoreFormat)
		if err != nil {
			return CheckResult{}, err
		}
		for _, entry := range rows {
			if entry.UserID != id || !slices.Contains(chunk, entry.MediaID) || entry.MediaID < 1 {
				return CheckResult{}, ErrMalformed
			}
			if _, duplicate := entries[entry.MediaID]; duplicate {
				return CheckResult{}, ErrMalformed
			}
			if entry.Media != nil && (entry.Media.ID != entry.MediaID || !validMediaType(entry.Media.Type)) {
				return CheckResult{}, ErrMalformed
			}
			if err := budget.retain(entry); err != nil {
				return CheckResult{}, err
			}
			entries[entry.MediaID] = entry
		}
	}
	base, err := p.basePayloads(state, user, latest, ids, entries, reviews, favorites, &budget)
	if err != nil {
		return CheckResult{}, err
	}
	// Optional metadata gets its own bounded retained-data allowance. Core
	// messages are reserved first, so enrichment cannot consume their capacity.
	metadataBudget := collectionBudget{options: ProviderOptions{MaxBytes: min(p.options.MaxBytes, 1<<20)}}
	metadataStopped := false
	if flags&(Description|Genres|Tags|Studio|Mangaka|Director|VoiceActors) != 0 {
		for start := 0; start < len(ids); start += p.options.PageSize {
			chunk := ids[start:min(len(ids), start+p.options.PageSize)]
			if err := budget.request(); err != nil {
				result.MetadataFailures++
				metadataStopped = true
				break
			}
			rows, err := p.client.mediaEnrichment(ctx, chunk, p.options.PageSize, flags)
			if ctx.Err() != nil {
				return CheckResult{}, ctx.Err()
			}
			if err != nil {
				result.MetadataFailures++
				// Preserve the client's cooldown for future polls without spending
				// this collection's remaining deadline on display-only requests.
				metadataStopped = errors.Is(err, ErrRateLimited) || errors.Is(err, ErrUnavailable) || errors.Is(err, context.DeadlineExceeded)
				if metadataStopped {
					break
				}
				continue
			}
			for _, extra := range rows {
				if !slices.Contains(chunk, extra.ID) {
					result.MetadataFailures++
					continue
				}
				entry, ok := entries[extra.ID]
				if !ok || entry.Private || entry.Media == nil {
					continue
				}
				if err := metadataBudget.retain(extra); err != nil {
					result.MetadataFailures++
					continue
				}
				media := *entry.Media
				media.Description = extra.Description
				media.Genres = extra.Genres
				media.Tags = extra.Tags
				media.Studios = extra.Studios
				media.Staff = extra.Staff
				media.Characters = extra.Characters
				entry.Media = &media
				entries[extra.ID] = entry
			}
		}
	}
	for _, mediaID := range ids {
		activity := latest[mediaID]
		entry, ok := entries[mediaID]
		if !ok || entry.Private || entry.Media == nil {
			input.Observations = append(input.Observations, activityObservation(activity))
			continue
		}
		if activity.Media.Type != entry.Media.Type {
			return CheckResult{}, ErrMalformed
		}
		payload, err := activityPayload(activity, entry, user, state)
		if err != nil {
			return CheckResult{}, err
		}
		payload, fallback := p.preferPayload(payload, base.activities[mediaID], &budget)
		if fallback {
			result.MetadataFailures++
		}
		input.Events = append(input.Events, update.Event{Stream: update.Activity, Kind: update.List, SourceID: strconv.FormatInt(activity.ID, 10), Subject: mediaSubject(*entry.Media), OccurredAt: time.Unix(activity.CreatedAt, 0).UTC(), Payload: payload})
	}
	for _, review := range reviews {
		observation := update.Observation{Stream: update.ReviewStream, Value: review.CreatedAt, SourceID: strconv.FormatInt(review.ID, 10)}
		if state.Cursors[update.ReviewStream].Contains(observation) {
			continue
		}
		if review.Media == nil || review.Private {
			input.Observations = append(input.Observations, observation)
			continue
		}
		payload := base.reviews[review.ID]
		input.Events = append(input.Events, update.Event{Stream: update.ReviewStream, Kind: update.Review, SourceID: strconv.FormatInt(review.ID, 10), Subject: mediaSubject(*review.Media), OccurredAt: time.Unix(review.CreatedAt, 0).UTC(), Payload: payload})
	}
	if flags&Favorites != 0 {
		current, changed := base.current, base.changed
		for _, kind := range favoriteKinds {
			if metadataStopped {
				break
			}
			var changeIDs []int64
			for subject := range changed {
				if graphKind(subject.Kind) == kind {
					number, _ := strconv.ParseInt(subject.ID, 10, 64)
					changeIDs = append(changeIDs, number)
				}
			}
			slices.Sort(changeIDs)
			for start := 0; start < len(changeIDs); start += p.options.PageSize {
				chunk := changeIDs[start:min(len(changeIDs), start+p.options.PageSize)]
				if err := budget.request(); err != nil {
					result.MetadataFailures++
					metadataStopped = true
					break
				}
				rows, err := p.client.favoriteDetails(ctx, kind, chunk, p.options.PageSize, flags)
				if ctx.Err() != nil {
					return CheckResult{}, ctx.Err()
				}
				if err != nil {
					result.MetadataFailures++
					metadataStopped = errors.Is(err, ErrRateLimited) || errors.Is(err, ErrUnavailable) || errors.Is(err, context.DeadlineExceeded)
					if metadataStopped {
						break
					}
					continue
				}
				for _, info := range rows {
					if !slices.Contains(chunk, info.ID) {
						result.MetadataFailures++
						continue
					}
					if err := metadataBudget.retain(info); err != nil {
						result.MetadataFailures++
						continue
					}
					subject := update.Subject{Kind: subjectKind(kind), ID: strconv.FormatInt(info.ID, 10)}
					favorite := changed[subject]
					favorite.Name = favoriteName(info, kind, user)
					favorite.NameURL = entityURL(string(subject.Kind), subject.ID)
					favorite.ImageURL = imageURL(info.CoverImage)
					if kind == "characters" || kind == "staff" {
						favorite.ImageURL = imageURL(info.Image)
					}
					favorite.Extra, _ = json.Marshal(info)
					if old, exists := current[subject]; exists {
						if favoriteBytes(favorite) > p.queue.Limits().MaxPayloadBytes || budget.reserve(max(0, favoriteBytes(favorite)-favoriteBytes(old))) != nil {
							result.MetadataFailures++
							continue
						}
						current[subject] = favorite
					}
					changed[subject] = favorite
				}
			}
		}
		subjects := make([]update.Subject, 0, len(changed))
		for subject := range changed {
			subjects = append(subjects, subject)
		}
		sortSubjects(subjects)
		for _, subject := range subjects {
			favorite := changed[subject]
			_, added := current[subject]
			payload, err := favoritePayload(favorite, user, state, added)
			if err != nil {
				return CheckResult{}, err
			}
			payload, fallback := p.preferPayload(payload, base.favorites[subject], &budget)
			if fallback {
				result.MetadataFailures++
			}
			kind := update.FavoriteRemoved
			if added {
				kind = update.FavoriteAdded
			}
			input.Events = append(input.Events, update.Event{Stream: update.Favorites, Kind: kind, SourceID: string(subject.Kind) + ":" + subject.ID, Subject: subject, OccurredAt: input.Now, Payload: payload})
		}
		favorites = make([]store.Favorite, 0, len(current))
		for _, favorite := range current {
			favorites = append(favorites, favorite)
		}
		input.Favorites = &favorites
	}
	recorded, err := p.queue.Record(ctx, input)
	if err != nil {
		return CheckResult{}, err
	}
	result.PollResult = recorded
	return result, nil
}

func (p *Provider) activities(ctx context.Context, id int64, cursor update.Cursor, fence int64, flags Features, budget *collectionBudget) ([]Activity, error) {
	if cursor.Value < 0 || cursor.Value >= math.MaxInt32 {
		return nil, ErrMalformed
	}
	typeName := "MEDIA_LIST"
	if flags&AnimeList == 0 {
		typeName = "MANGA_LIST"
	} else if flags&MangaList == 0 {
		typeName = "ANIME_LIST"
	}
	result := []Activity{}
	seen := map[int64]Activity{}
	for number := 1; ; number++ {
		if err := budget.request(); err != nil {
			return nil, err
		}
		data, err := p.client.activityPage(ctx, id, cursor.Value-1, fence+1, number, p.options.PageSize, typeName)
		if err != nil {
			return nil, err
		}
		unseen := 0
		for _, activity := range data.Items {
			if activity.ID < 1 || activity.ID > math.MaxInt32 || activity.UserID != id || activity.Typename != "ListActivity" || activity.CreatedAt < cursor.Value || activity.CreatedAt > fence {
				return nil, ErrMalformed
			}
			if activity.Media != nil && (activity.Media.ID < 1 || !validMediaType(activity.Media.Type)) {
				return nil, ErrMalformed
			}
			if err := budget.record(activity); err != nil {
				return nil, err
			}
			if previous, duplicate := seen[activity.ID]; duplicate {
				if previous.CreatedAt != activity.CreatedAt || !sameActivitySubject(previous, activity) {
					return nil, ErrMalformed
				}
				continue
			}
			seen[activity.ID] = activity
			unseen++
			result = append(result, activity)
		}
		if !*data.PageInfo.HasNextPage {
			return result, nil
		}
		if unseen == 0 {
			return nil, tracking.ErrPaginationCycle
		}
	}
}

func (p *Provider) reviews(ctx context.Context, id int64, cursor update.Cursor, fence int64, budget *collectionBudget) ([]Review, error) {
	if cursor.Value < 0 || cursor.Value >= math.MaxInt32 {
		return nil, ErrMalformed
	}
	result := []Review{}
	seen := map[int64]bool{}
	previousTime := int64(math.MaxInt64)
	for number := 1; ; number++ {
		if err := budget.request(); err != nil {
			return nil, err
		}
		data, err := p.client.reviewPage(ctx, id, number, p.options.PageSize)
		if err != nil {
			return nil, err
		}
		older := false
		unseen := 0
		for _, review := range data.Items {
			if review.ID < 1 || review.UserID != id || review.CreatedAt < 0 || review.CreatedAt > previousTime {
				return nil, ErrMalformed
			}
			previousTime = review.CreatedAt
			if review.CreatedAt < cursor.Value {
				older = true
				continue
			}
			if review.CreatedAt > fence {
				continue
			}
			if review.Media != nil && (review.Media.ID < 1 || !validMediaType(review.Media.Type)) {
				return nil, ErrMalformed
			}
			if err := budget.record(review); err != nil {
				return nil, err
			}
			if seen[review.ID] {
				continue
			}
			seen[review.ID] = true
			unseen++
			result = append(result, review)
		}
		if older || !*data.PageInfo.HasNextPage {
			return result, nil
		}
		if unseen == 0 && len(data.Items) > 0 && data.Items[len(data.Items)-1].CreatedAt <= fence {
			return nil, tracking.ErrPaginationCycle
		}
	}
}

func (p *Provider) favorites(ctx context.Context, id int64, baseline []store.Favorite, budget *collectionBudget) ([]store.Favorite, error) {
	pages := map[string]int{}
	for _, kind := range favoriteKinds {
		pages[kind] = 1
	}
	old := favoriteMap(baseline)
	result := []store.Favorite{}
	seen := map[update.Subject]bool{}
	for len(pages) > 0 {
		if err := budget.request(); err != nil {
			return nil, err
		}
		response, err := p.client.favoritePages(ctx, id, pages, p.options.PageSize)
		if err != nil {
			return nil, err
		}
		for _, kind := range favoriteKinds {
			number, active := pages[kind]
			if !active {
				continue
			}
			data, err := checkedPage(response[kind], number)
			if err != nil {
				return nil, err
			}
			for _, item := range data.Items {
				if item.ID < 1 || item.ID > math.MaxInt32 {
					return nil, ErrMalformed
				}
				subject := update.Subject{Kind: subjectKind(kind), ID: strconv.FormatInt(item.ID, 10)}
				if seen[subject] {
					return nil, ErrMalformed
				}
				seen[subject] = true
				if len(result) >= p.options.MaxFavorites {
					return nil, ErrCollectionBudget
				}
				favorite, ok := old[subject]
				if !ok {
					favorite = store.Favorite{Subject: subject, Extra: json.RawMessage(`{}`)}
				}
				if err := budget.retain(favorite); err != nil {
					return nil, err
				}
				result = append(result, favorite)
			}
			if *data.PageInfo.HasNextPage {
				pages[kind]++
			} else {
				delete(pages, kind)
			}
		}
	}
	return result, nil
}

type collectionBudget struct {
	options                  ProviderOptions
	requests, records, bytes int
}

func (b *collectionBudget) request() error {
	if b.requests >= b.options.MaxPages {
		return ErrCollectionBudget
	}
	b.requests++
	return nil
}
func (b *collectionBudget) retain(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ErrMalformed
	}
	return b.retainPayload(encoded)
}
func (b *collectionBudget) retainPayload(value []byte) error { return b.reserve(len(value)) }
func (b *collectionBudget) reserve(size int) error {
	if size > b.options.MaxBytes-b.bytes {
		return ErrCollectionBudget
	}
	b.bytes += size
	return nil
}
func (b *collectionBudget) record(value any) error {
	if b.records >= b.options.MaxRecords {
		return ErrCollectionBudget
	}
	if err := b.retain(value); err != nil {
		return err
	}
	b.records++
	return nil
}
func (p *Provider) fence() (int64, error) {
	at := p.options.Now()
	seconds := at.Unix()
	if at.IsZero() || seconds < 0 || seconds >= math.MaxInt32 {
		return 0, ErrMalformed
	}
	return seconds, nil
}
func discordID(id string) bool {
	number, err := strconv.ParseUint(id, 10, 64)
	return err == nil && number > 0 && strconv.FormatUint(number, 10) == id
}
func validMediaType(kind string) bool { return kind == "ANIME" || kind == "MANGA" }
func mediaSubject(media Media) update.Subject {
	return update.Subject{Kind: update.SubjectKind(strings.ToLower(media.Type)), ID: strconv.FormatInt(media.ID, 10)}
}
func activityObservation(activity Activity) update.Observation {
	return update.Observation{Stream: update.Activity, Value: activity.CreatedAt, SourceID: strconv.FormatInt(activity.ID, 10)}
}
func newerActivity(a, b Activity) bool {
	return a.CreatedAt > b.CreatedAt || (a.CreatedAt == b.CreatedAt && a.ID > b.ID)
}
func sameActivitySubject(a, b Activity) bool {
	if a.Media == nil || b.Media == nil {
		return a.Media == nil && b.Media == nil
	}
	return a.Media.ID == b.Media.ID && a.Media.Type == b.Media.Type
}
func favoriteMap(favorites []store.Favorite) map[update.Subject]store.Favorite {
	result := make(map[update.Subject]store.Favorite, len(favorites))
	for _, favorite := range favorites {
		result[favorite.Subject] = favorite
	}
	return result
}
func subjectKind(kind string) update.SubjectKind {
	switch kind {
	case "characters":
		return update.CharacterSubject
	case "studios":
		return update.StudioSubject
	default:
		return update.SubjectKind(kind)
	}
}
func graphKind(kind update.SubjectKind) string {
	switch kind {
	case update.CharacterSubject:
		return "characters"
	case update.StudioSubject:
		return "studios"
	default:
		return string(kind)
	}
}
func sortSubjects(subjects []update.Subject) {
	slices.SortFunc(subjects, func(a, b update.Subject) int {
		if order := strings.Compare(string(a.Kind), string(b.Kind)); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
}
func favoriteName(info FavoriteInfo, kind string, user User) string {
	switch kind {
	case "anime", "manga":
		return mediaTitle(info.Media, user.Options.TitleLanguage, 0)
	case "studios":
		return info.Label
	default:
		return personName(info.Name, user.Options.TitleLanguage)
	}
}

type payloadBaseline struct {
	activities map[int64]json.RawMessage
	reviews    map[int64]json.RawMessage
	favorites  map[update.Subject]json.RawMessage
	current    map[update.Subject]store.Favorite
	changed    map[update.Subject]store.Favorite
}

func (p *Provider) basePayloads(state store.Snapshot, user User, activities map[int64]Activity, ids []int64, entries map[int64]Entry, reviews []Review, favorites []store.Favorite, budget *collectionBudget) (payloadBaseline, error) {
	base := payloadBaseline{activities: map[int64]json.RawMessage{}, reviews: map[int64]json.RawMessage{}, favorites: map[update.Subject]json.RawMessage{}, current: favoriteMap(favorites), changed: map[update.Subject]store.Favorite{}}
	for _, id := range ids {
		entry, ok := entries[id]
		if !ok || entry.Private || entry.Media == nil {
			continue
		}
		payload, err := activityPayload(activities[id], entry, user, state)
		if err != nil {
			return payloadBaseline{}, err
		}
		if err := budget.retainPayload(payload); err != nil {
			return payloadBaseline{}, err
		}
		base.activities[id] = payload
	}
	for _, review := range reviews {
		observation := update.Observation{Stream: update.ReviewStream, Value: review.CreatedAt, SourceID: strconv.FormatInt(review.ID, 10)}
		if review.Media == nil || review.Private || state.Cursors[update.ReviewStream].Contains(observation) {
			continue
		}
		payload, err := reviewPayload(review, user, state)
		if err != nil {
			return payloadBaseline{}, err
		}
		if err := budget.retainPayload(payload); err != nil {
			return payloadBaseline{}, err
		}
		base.reviews[review.ID] = payload
	}
	if Features(state.Features)&Favorites != 0 {
		previous := favoriteMap(state.Favorites)
		for subject, favorite := range base.current {
			if _, ok := previous[subject]; !ok {
				base.changed[subject] = favorite
			}
		}
		for subject, favorite := range previous {
			if _, ok := base.current[subject]; !ok {
				base.changed[subject] = favorite
			}
		}
		if budget.records+len(base.changed) > p.options.MaxRecords {
			return payloadBaseline{}, ErrCollectionBudget
		}
		budget.records += len(base.changed)
		for subject, favorite := range base.changed {
			_, added := base.current[subject]
			payload, err := favoritePayload(favorite, user, state, added)
			if err != nil {
				return payloadBaseline{}, err
			}
			if err := budget.retainPayload(payload); err != nil {
				return payloadBaseline{}, err
			}
			base.favorites[subject] = payload
		}
	}
	return base, nil
}

func (p *Provider) preferPayload(enriched, base json.RawMessage, budget *collectionBudget) (json.RawMessage, bool) {
	if len(enriched) > p.queue.Limits().MaxPayloadBytes || budget.reserve(max(0, len(enriched)-len(base))) != nil {
		return base, true
	}
	return enriched, false
}
func favoriteBytes(favorite store.Favorite) int {
	return len(favorite.Subject.ID) + len(favorite.Name) + len(favorite.NameURL) + len(favorite.ImageURL) + len(favorite.Extra)
}
