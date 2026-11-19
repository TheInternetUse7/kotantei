package mal

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"kotantei/internal/store"
	"kotantei/internal/tracking"
	"kotantei/internal/update"
)

var ErrCollectionBudget = tracking.ErrCollectionBudget
var ErrRenameRecovery = errors.New("MAL username could not be verified from its saved ID; retry or check profile access before relinking")

type ProviderOptions struct {
	PageSize        int
	MaxRequests     int
	MaxRecords      int
	MaxFavorites    int
	MaxBytes        int
	CheckTimeout    time.Duration
	ProfileTimeout  time.Duration
	MetadataTimeout time.Duration
	Enricher        *Enricher
	Now             func() time.Time
}
type Provider struct {
	queue   *store.Outbox
	client  *Client
	options ProviderOptions
}
type CheckResult struct {
	store.PollResult
	ProfileFailures  int
	FavoriteFailures int
	MetadataFailures int
	PrivateLists     int
	Renamed          bool
}

func NewProvider(queue *store.Outbox, client *Client, options ProviderOptions) (*Provider, error) {
	if queue == nil || client == nil {
		return nil, errors.New("MAL provider requires an outbox and client")
	}
	if options.PageSize == 0 {
		options.PageSize = 100
	}
	if options.MaxRequests == 0 {
		options.MaxRequests = 64
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
		options.CheckTimeout = 5 * time.Minute
	}
	if options.ProfileTimeout == 0 {
		options.ProfileTimeout = 5 * time.Second
	}
	if options.MetadataTimeout == 0 {
		options.MetadataTimeout = 10 * time.Second
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.PageSize < 1 || options.PageSize > 1000 || options.MaxRequests < 1 || options.MaxRecords < 1 || options.MaxFavorites < 1 || options.MaxBytes < 1 || options.CheckTimeout <= 0 || options.ProfileTimeout <= 0 || options.MetadataTimeout <= 0 {
		return nil, errors.New("invalid MAL collection limits")
	}
	limits := queue.Limits()
	options.MaxRecords = min(options.MaxRecords, limits.MaxRecords)
	options.MaxFavorites = min(options.MaxFavorites, limits.MaxFavorites)
	options.MaxBytes = min(options.MaxBytes, limits.MaxBatchBytes)
	return &Provider{queue: queue, client: client, options: options}, nil
}

func (p *Provider) Link(parent context.Context, userID, guildID, username string) (store.LinkResult, error) {
	ctx, cancel := context.WithTimeout(parent, p.options.CheckTimeout)
	defer cancel()
	if !discordID(userID) || !discordID(guildID) {
		return store.LinkResult{}, store.ErrInvalidPoll
	}
	state, err := p.queue.ForUser(ctx, userID, "mal")
	if err == nil {
		return p.queue.Connect(ctx, store.AccountLink{Provider: "mal", ExternalID: state.ExternalID, UserID: userID, GuildID: guildID, Username: state.Username})
	}
	if !errors.Is(err, store.ErrAccountNotFound) {
		return store.LinkResult{}, err
	}
	username = strings.TrimSpace(username)
	if !validUsername(username) {
		return store.LinkResult{}, ErrMalformed
	}
	fence, err := p.fence()
	if err != nil {
		return store.LinkResult{}, err
	}
	budget := &collectionBudget{options: p.options}
	ctx = context.WithValue(ctx, budgetKey{}, budget)
	profile, err := p.profile(ctx, username, true)
	if err != nil {
		return store.LinkResult{}, err
	}
	if !profile.FavoritesComplete {
		return store.LinkResult{}, ErrIncompleteFavorites
	}
	if len(profile.Favorites) > p.options.MaxFavorites {
		return store.LinkResult{}, ErrCollectionBudget
	}
	if err := budget.retain(profile); err != nil {
		return store.LinkResult{}, err
	}
	cursors := map[update.Stream]update.Cursor{}
	for _, kind := range []string{"anime", "manga"} {
		stream := update.Stream(kind)
		cursor := update.Cursor{Value: fence.Truncate(time.Second).UnixMicro(), SourceIDs: []string{}}
		rows, err := p.collect(ctx, profile.Username, kind, cursor, fence, DefaultFeatures, budget)
		if err != nil && !errors.Is(err, ErrNotPublic) {
			return store.LinkResult{}, err
		}
		observations := make([]update.Observation, 0, len(rows))
		for _, row := range rows {
			observation, err := entryRevision(kind, row)
			if err != nil {
				return store.LinkResult{}, err
			}
			observations = append(observations, observation)
		}
		// MAL usually emits whole seconds, but fractional timestamps are valid.
		// Quietly consume all existing revisions in the linking second instead
		// of treating fractional positions as newly discovered on the first poll.
		cursor, err = cursor.Advance(observations, p.queue.Limits().MaxBoundaryIDs)
		if err != nil {
			return store.LinkResult{}, err
		}
		cursors[stream] = cursor
	}
	return p.queue.Connect(ctx, store.AccountLink{Provider: "mal", ExternalID: strconv.FormatInt(profile.ID, 10), UserID: userID, GuildID: guildID, Username: profile.Username, Cursors: cursors, Favorites: profile.Favorites})
}

// Check keeps scraper/catalogue failures separate from required list pages.
// A positively changed identity is verified before reading lists under its name.
func (p *Provider) Check(parent context.Context, accountID int64) (CheckResult, error) {
	ctx, cancel := context.WithTimeout(parent, p.options.CheckTimeout)
	defer cancel()
	state, err := p.queue.Snapshot(ctx, accountID)
	if err != nil {
		return CheckResult{}, err
	}
	if state.Provider != "mal" {
		return CheckResult{}, store.ErrInvalidPoll
	}
	id, err := strconv.ParseInt(state.ExternalID, 10, 64)
	if err != nil || id <= 0 {
		return CheckResult{}, ErrMalformed
	}
	fence, err := p.fence()
	if err != nil {
		return CheckResult{}, err
	}
	flags := Features(state.Features)
	budget := &collectionBudget{options: p.options}
	ctx = context.WithValue(ctx, budgetKey{}, budget)
	result := CheckResult{}
	profile, err := p.checkProfile(ctx, state, id, flags, &result)
	if err != nil {
		return CheckResult{}, err
	}
	rows := map[string][]Entry{}
	for attempt := 0; attempt < 2; attempt++ {
		rows = map[string][]Entry{}
		result.PrivateLists = 0
		recoverName := false
		for _, kind := range []string{"anime", "manga"} {
			feature := AnimeList
			if kind == "manga" {
				feature = MangaList
			}
			if flags&feature == 0 {
				continue
			}
			collected, err := p.collect(ctx, profile.Username, kind, state.Cursors[update.Stream(kind)], fence, flags, budget)
			if errors.Is(err, ErrNotFound) {
				recoverName = true
				break
			}
			if errors.Is(err, ErrNotPublic) {
				result.PrivateLists++
				continue
			}
			if err != nil {
				return CheckResult{}, err
			}
			rows[kind] = collected
		}
		if !recoverName {
			break
		}
		if attempt == 1 {
			return CheckResult{}, ErrRenameRecovery
		}
		profile, err = p.recover(ctx, id, flags&Favorites != 0)
		if err != nil {
			return CheckResult{}, err
		}
		result.Renamed = profile.Username != state.Username
	}
	if ctx.Err() != nil {
		return CheckResult{}, ctx.Err()
	}
	if err := budget.retain(profile); err != nil {
		return CheckResult{}, err
	}
	input := store.Poll{AccountID: accountID, ExpectedRevision: state.Revision, Now: fence, Username: &profile.Username}
	type listItem struct {
		kind        string
		entry       Entry
		observation update.Observation
		payload     json.RawMessage
	}
	items := []listItem{}
	for _, kind := range []string{"anime", "manga"} {
		latest := map[int64]listItem{}
		for _, entry := range rows[kind] {
			observation, err := entryRevision(kind, entry)
			if err != nil {
				return CheckResult{}, err
			}
			if state.Cursors[observation.Stream].Contains(observation) {
				continue
			}
			if strings.TrimSpace(entry.Node.Title) == "" {
				input.Observations = append(input.Observations, observation)
				continue
			}
			item := listItem{kind: kind, entry: entry, observation: observation}
			if previous, ok := latest[entry.Node.ID]; ok {
				if observation.Value >= previous.observation.Value {
					input.Observations = append(input.Observations, previous.observation)
					latest[entry.Node.ID] = item
				} else {
					input.Observations = append(input.Observations, observation)
				}
			} else {
				latest[entry.Node.ID] = item
			}
		}
		ids := make([]int64, 0, len(latest))
		for id := range latest {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			item := latest[id]
			payload, err := listPayload(kind, item.entry, profile, state, Enrichment{})
			if err != nil {
				return CheckResult{}, err
			}
			if len(payload) > p.queue.Limits().MaxPayloadBytes {
				return CheckResult{}, ErrCollectionBudget
			}
			if err := budget.reserve(len(payload)); err != nil {
				return CheckResult{}, err
			}
			item.payload = payload
			items = append(items, item)
		}
	}
	current := map[update.Subject]store.Favorite{}
	changed := map[update.Subject]store.Favorite{}
	favoritePayloads := map[update.Subject]json.RawMessage{}
	if flags&Favorites != 0 {
		if !profile.FavoritesComplete {
			result.FavoriteFailures++
		} else {
			if len(profile.Favorites) > p.options.MaxFavorites {
				return CheckResult{}, ErrCollectionBudget
			}
			previous := favoriteMap(state.Favorites)
			current = mergeFavorites(profile.Favorites, previous)
			favorites := favoriteValues(current)
			if err := budget.retain(favorites); err != nil {
				return CheckResult{}, err
			}
			input.Favorites = &favorites
			for subject, favorite := range current {
				if _, ok := previous[subject]; !ok {
					changed[subject] = favorite
				}
			}
			for subject, favorite := range previous {
				if _, ok := current[subject]; !ok {
					changed[subject] = favorite
				}
			}
			if budget.records+len(changed) > p.options.MaxRecords {
				return CheckResult{}, ErrCollectionBudget
			}
			budget.records += len(changed)
			for subject, favorite := range changed {
				_, added := current[subject]
				payload, err := favoritePayload(favorite, profile, state, added, Enrichment{})
				if err != nil {
					return CheckResult{}, err
				}
				if len(payload) > p.queue.Limits().MaxPayloadBytes {
					return CheckResult{}, ErrCollectionBudget
				}
				if err := budget.reserve(len(payload)); err != nil {
					return CheckResult{}, err
				}
				favoritePayloads[subject] = payload
			}
		}
	}
	// Metadata has its own short context/allowance; core payload space is already
	// reserved. A timeout or cooldown here falls back without dropping core work.
	metadataContext, stopMetadata := context.WithTimeout(ctx, p.options.MetadataTimeout)
	defer stopMetadata()
	metadataStopped := false
	metadataBytes := 0
	lookup := func(subject update.Subject) (Enrichment, bool) {
		if p.options.Enricher == nil || metadataStopped {
			return Enrichment{}, false
		}
		extra, err := p.options.Enricher.Lookup(metadataContext, subject, flags)
		if err != nil {
			result.MetadataFailures++
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrRateLimited) || errors.Is(err, ErrCollectionBudget) {
				metadataStopped = true
			}
			return Enrichment{}, false
		}
		encoded, err := json.Marshal(extra)
		if err != nil || len(encoded) > min(p.options.MaxBytes, 1<<20)-metadataBytes {
			result.MetadataFailures++
			return Enrichment{}, false
		}
		metadataBytes += len(encoded)
		return extra, true
	}
	prefer := func(enriched, base json.RawMessage) json.RawMessage {
		if len(enriched) > p.queue.Limits().MaxPayloadBytes || budget.reserve(max(0, len(enriched)-len(base))) != nil {
			result.MetadataFailures++
			return base
		}
		return enriched
	}
	for _, item := range items {
		payload := item.payload
		if flags&(Themes|Demographic|Seiyu) != 0 {
			if extra, ok := lookup(update.Subject{Kind: update.SubjectKind(item.kind), ID: strconv.FormatInt(item.entry.Node.ID, 10)}); ok {
				enriched, err := listPayload(item.kind, item.entry, profile, state, extra)
				if err != nil {
					return CheckResult{}, err
				}
				payload = prefer(enriched, payload)
			}
		}
		input.Events = append(input.Events, update.Event{Stream: item.observation.Stream, Kind: update.List, SourceID: item.observation.SourceID, Subject: update.Subject{Kind: update.SubjectKind(item.kind), ID: strconv.FormatInt(item.entry.Node.ID, 10)}, OccurredAt: time.UnixMicro(item.observation.Value).UTC(), Payload: payload})
	}
	subjects := make([]update.Subject, 0, len(changed))
	for subject := range changed {
		subjects = append(subjects, subject)
	}
	slices.SortFunc(subjects, func(a, b update.Subject) int {
		if order := strings.Compare(string(a.Kind), string(b.Kind)); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	for _, subject := range subjects {
		favorite := changed[subject]
		_, added := current[subject]
		payload := favoritePayloads[subject]
		if extra, ok := lookup(subject); ok {
			enriched, err := favoritePayload(favorite, profile, state, added, extra)
			if err != nil {
				return CheckResult{}, err
			}
			payload = prefer(enriched, payload)
		}
		kind := update.FavoriteRemoved
		if added {
			kind = update.FavoriteAdded
		}
		input.Events = append(input.Events, update.Event{Stream: update.Favorites, Kind: kind, SourceID: string(subject.Kind) + ":" + subject.ID, Subject: subject, OccurredAt: fence, Payload: payload})
	}
	if ctx.Err() != nil {
		return CheckResult{}, ctx.Err()
	}
	recorded, err := p.queue.Record(ctx, input)
	if err != nil {
		return CheckResult{}, err
	}
	result.PollResult = recorded
	return result, nil
}

func (p *Provider) profile(parent context.Context, name string, favorites bool) (Profile, error) {
	ctx, cancel := context.WithTimeout(parent, p.options.ProfileTimeout)
	defer cancel()
	return p.client.Profile(ctx, name, favorites)
}
func (p *Provider) checkProfile(ctx context.Context, state store.Snapshot, id int64, flags Features, result *CheckResult) (Profile, error) {
	if validUsername(state.Username) {
		profile, err := p.profile(ctx, state.Username, flags&Favorites != 0)
		if ctx.Err() != nil {
			return Profile{}, ctx.Err()
		}
		if errors.Is(err, ErrCollectionBudget) {
			return Profile{}, err
		}
		if err == nil && profile.ID == id {
			result.Renamed = profile.Username != state.Username
			return profile, nil
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			result.ProfileFailures++
			return Profile{ID: id, Username: state.Username, AvatarURL: avatarURL(id)}, nil
		}
	}
	profile, err := p.recover(ctx, id, flags&Favorites != 0)
	if err != nil {
		return Profile{}, err
	}
	result.Renamed = profile.Username != state.Username
	return profile, nil
}
func (p *Provider) recover(parent context.Context, id int64, favorites bool) (Profile, error) {
	ctx, cancel := context.WithTimeout(parent, p.options.ProfileTimeout)
	name, err := p.client.RecoverUsername(ctx, id)
	cancel()
	if err != nil {
		return Profile{}, errors.Join(ErrRenameRecovery, err)
	}
	profile, err := p.profile(parent, name, favorites)
	if err != nil {
		return Profile{}, errors.Join(ErrRenameRecovery, err)
	}
	if profile.ID != id {
		return Profile{}, ErrRenameRecovery
	}
	return profile, nil
}

func (p *Provider) collect(ctx context.Context, name, kind string, cursor update.Cursor, fence time.Time, flags Features, budget *collectionBudget) ([]Entry, error) {
	if cursor.Value < 0 {
		return nil, ErrMalformed
	}
	offset := 0
	previous := int64(math.MaxInt64)
	seen := map[string]bool{}
	result := []Entry{}
	for {
		page, err := p.client.ListPage(ctx, name, kind, offset, p.options.PageSize, flags)
		if err != nil {
			return nil, err
		}
		older := false
		unseen := 0
		for _, entry := range page.Entries {
			observation, err := entryRevision(kind, entry)
			if err != nil {
				return nil, err
			}
			if observation.Value > previous {
				return nil, ErrMalformed
			}
			previous = observation.Value
			if observation.Value < cursor.Value {
				older = true
				continue
			}
			if observation.Value > fence.UnixMicro() {
				continue
			}
			if budget.records >= p.options.MaxRecords {
				return nil, ErrCollectionBudget
			}
			if err := budget.retain(entry); err != nil {
				return nil, err
			}
			budget.records++
			if seen[observation.SourceID] {
				continue
			}
			seen[observation.SourceID] = true
			unseen++
			result = append(result, entry)
		}
		if older || page.NextOffset == nil {
			return result, nil
		}
		if unseen == 0 && len(page.Entries) > 0 && previous <= fence.UnixMicro() {
			return nil, tracking.ErrPaginationCycle
		}
		offset = *page.NextOffset
	}
}

type budgetKey struct{}
type collectionBudget struct {
	options                  ProviderOptions
	mu                       sync.Mutex
	requests, records, bytes int
}

func takeRequest(ctx context.Context) error {
	budget, _ := ctx.Value(budgetKey{}).(*collectionBudget)
	if budget == nil {
		return nil
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.requests >= budget.options.MaxRequests {
		return ErrCollectionBudget
	}
	budget.requests++
	return nil
}
func (b *collectionBudget) retain(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ErrMalformed
	}
	return b.reserve(len(encoded))
}
func (b *collectionBudget) reserve(bytes int) error {
	if bytes > b.options.MaxBytes-b.bytes {
		return ErrCollectionBudget
	}
	b.bytes += bytes
	return nil
}
func (p *Provider) fence() (time.Time, error) {
	now := p.options.Now().UTC().Truncate(time.Microsecond)
	if now.IsZero() || now.Unix() < 0 || now.Year() > 9999 {
		return time.Time{}, ErrMalformed
	}
	return now, nil
}
func discordID(id string) bool {
	number, err := strconv.ParseUint(id, 10, 64)
	return err == nil && number > 0 && strconv.FormatUint(number, 10) == id
}
func favoriteMap(values []store.Favorite) map[update.Subject]store.Favorite {
	result := make(map[update.Subject]store.Favorite, len(values))
	for _, value := range values {
		result[value.Subject] = value
	}
	return result
}
func favoriteValues(values map[update.Subject]store.Favorite) []store.Favorite {
	result := make([]store.Favorite, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}
func mergeFavorites(values []store.Favorite, previous map[update.Subject]store.Favorite) map[update.Subject]store.Favorite {
	result := make(map[update.Subject]store.Favorite, len(values))
	for _, value := range values {
		if old, ok := previous[value.Subject]; ok {
			if value.Name == "" || value.Name == string(value.Subject.Kind)+" #"+value.Subject.ID {
				value.Name = old.Name
			}
			if value.ImageURL == "" {
				value.ImageURL = old.ImageURL
			}
			var extra map[string]json.RawMessage
			_ = json.Unmarshal(old.Extra, &extra)
			if extra == nil {
				extra = map[string]json.RawMessage{}
			}
			metadata := favoriteMetadata(value.Extra)
			updates := FavoriteMetadata{Format: metadata.Format, Year: metadata.Year, From: metadata.From}
			encoded, _ := json.Marshal(updates)
			var current map[string]json.RawMessage
			_ = json.Unmarshal(encoded, &current)
			for key, field := range current {
				extra[key] = field
			}
			value.Extra, _ = json.Marshal(extra)
		}
		result[value.Subject] = value
	}
	return result
}
