// Package update defines provider-independent observations and notifications.
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Stream string

const (
	Activity     Stream = "activity"
	ReviewStream Stream = "review"
	Anime        Stream = "anime"
	Manga        Stream = "manga"
	Favorites    Stream = "favorites"
)

type Kind string

const (
	List            Kind = "list"
	Review          Kind = "review"
	FavoriteAdded   Kind = "favorite_added"
	FavoriteRemoved Kind = "favorite_removed"
)

type SubjectKind string

const (
	AnimeSubject     SubjectKind = "anime"
	MangaSubject     SubjectKind = "manga"
	CharacterSubject SubjectKind = "character"
	StaffSubject     SubjectKind = "staff"
	StudioSubject    SubjectKind = "studio"
	PersonSubject    SubjectKind = "person"
	CompanySubject   SubjectKind = "company"
)

type Subject struct {
	Kind SubjectKind
	ID   string
}
type Event struct {
	Stream     Stream
	Kind       Kind
	SourceID   string
	Subject    Subject
	OccurredAt time.Time
	Payload    json.RawMessage
}
type Observation struct {
	Stream   Stream
	Value    int64
	SourceID string
}
type Cursor struct {
	Value     int64
	SourceIDs []string
}

var ErrInvalidEvent = errors.New("invalid update event")
var ErrInvalidObservation = errors.New("invalid update observation")
var ErrBoundaryLimit = errors.New("timestamp boundary identity limit exceeded; checkpoint was not advanced")

// Key excludes mutable display metadata. Activity/review source IDs and MAL
// list revisions must be stable upstream identities. Favorites have no such
// IDs, so their keys also include the captured account snapshot generation.
func (e Event) Key(accountID, revision int64) (string, error) {
	if accountID <= 0 || revision < 0 {
		return "", fmt.Errorf("%w: invalid account/generation", ErrInvalidEvent)
	}
	if err := e.Validate(); err != nil {
		return "", err
	}
	parts := []string{"kotantei-event-v1", strconv.FormatInt(accountID, 10), string(e.Stream), string(e.Kind), e.SourceID}
	if e.Stream == Favorites {
		parts = append(parts, strconv.FormatInt(revision, 10))
	}
	encoded, err := json.Marshal(parts)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

func (e Event) Validate() error {
	if !validSourceID(e.SourceID) {
		return fmt.Errorf("%w: invalid source identity", ErrInvalidEvent)
	}
	if err := e.Subject.Validate(); err != nil {
		return err
	}
	media := e.Subject.Kind == AnimeSubject || e.Subject.Kind == MangaSubject
	valid := false
	switch e.Stream {
	case Activity:
		valid = e.Kind == List && media
	case Anime:
		valid = e.Kind == List && e.Subject.Kind == AnimeSubject
	case Manga:
		valid = e.Kind == List && e.Subject.Kind == MangaSubject
	case ReviewStream:
		valid = e.Kind == Review && media
	case Favorites:
		valid = e.Kind == FavoriteAdded || e.Kind == FavoriteRemoved
	}
	if !valid {
		return fmt.Errorf("%w: incompatible stream/type/subject", ErrInvalidEvent)
	}
	if e.OccurredAt.IsZero() || e.OccurredAt.Unix() < 0 || e.OccurredAt.Year() > 9999 {
		return fmt.Errorf("%w: invalid occurrence time", ErrInvalidEvent)
	}
	var object map[string]json.RawMessage
	if !utf8.Valid(e.Payload) || json.Unmarshal(e.Payload, &object) != nil || object == nil {
		return fmt.Errorf("%w: payload must be a JSON object", ErrInvalidEvent)
	}
	return nil
}

func (s Subject) Validate() error {
	switch s.Kind {
	case AnimeSubject, MangaSubject, CharacterSubject, StaffSubject, StudioSubject, PersonSubject, CompanySubject:
	default:
		return fmt.Errorf("%w: invalid subject type", ErrInvalidEvent)
	}
	id, err := strconv.ParseUint(s.ID, 10, 64)
	if err != nil || id == 0 || strconv.FormatUint(id, 10) != s.ID {
		return fmt.Errorf("%w: invalid numeric subject identity", ErrInvalidEvent)
	}
	return nil
}

func (e Event) Observation() (Observation, bool) {
	value := e.OccurredAt.Unix()
	switch e.Stream {
	case Activity, ReviewStream:
	case Anime, Manga:
		value = e.OccurredAt.UnixMicro()
	default:
		return Observation{}, false
	}
	return Observation{Stream: e.Stream, Value: value, SourceID: e.SourceID}, true
}

func (o Observation) Validate() error {
	switch o.Stream {
	case Activity, ReviewStream, Anime, Manga:
	default:
		return ErrInvalidObservation
	}
	if o.Value < 0 || !validSourceID(o.SourceID) {
		return ErrInvalidObservation
	}
	return nil
}

func validSourceID(id string) bool {
	return id != "" && id == strings.TrimSpace(id) && len(id) <= 256 && utf8.ValidString(id)
}

// Normalize coalesces only list notifications, not reviews or favorites.
// Chronology is deterministic, including numeric activity IDs at timestamp ties.
// The input and its payloads are never modified.
func Normalize(events []Event) ([]Event, error) {
	identities := map[[3]string]Event{}
	for _, event := range events {
		if err := event.Validate(); err != nil {
			return nil, err
		}
		event.OccurredAt = event.OccurredAt.UTC().Truncate(time.Microsecond)
		identity := [3]string{string(event.Stream), string(event.Kind), event.SourceID}
		if previous, ok := identities[identity]; ok {
			if previous.Subject != event.Subject {
				return nil, fmt.Errorf("%w: conflicting source subjects", ErrInvalidEvent)
			}
			if compareEvents(previous, event) > 0 {
				continue
			}
		}
		identities[identity] = event
	}
	listUpdates := map[[3]string]Event{}
	result := make([]Event, 0, len(identities))
	for _, event := range identities {
		if event.Kind != List {
			result = append(result, event)
			continue
		}
		subject := [3]string{string(event.Stream), string(event.Subject.Kind), event.Subject.ID}
		if previous, ok := listUpdates[subject]; !ok || compareEvents(previous, event) < 0 {
			listUpdates[subject] = event
		}
	}
	for _, event := range listUpdates {
		result = append(result, event)
	}
	slices.SortFunc(result, compareEvents)
	return result, nil
}

func compareEvents(a, b Event) int {
	if order := a.OccurredAt.Compare(b.OccurredAt); order != 0 {
		return order
	}
	aID, aErr := strconv.ParseUint(a.SourceID, 10, 64)
	bID, bErr := strconv.ParseUint(b.SourceID, 10, 64)
	// Keep numeric and opaque identities in separate ordered groups. Falling
	// back to lexical order only for mixed pairs would make comparison cyclic.
	if (aErr == nil) != (bErr == nil) {
		if aErr == nil {
			return -1
		}
		return 1
	}
	if aErr == nil && bErr == nil && aID != bID {
		if aID < bID {
			return -1
		}
		return 1
	}
	for _, pair := range [][2]string{{a.SourceID, b.SourceID}, {string(a.Stream), string(b.Stream)}, {string(a.Kind), string(b.Kind)}} {
		if order := strings.Compare(pair[0], pair[1]); order != 0 {
			return order
		}
	}
	return 0
}

// Contains is inclusive at the timestamp, but only for identities already
// recorded there. Older timestamps are consumed regardless of their identity.
func (c Cursor) Contains(o Observation) bool {
	return o.Value < c.Value || (o.Value == c.Value && slices.Contains(c.SourceIDs, o.SourceID))
}

func (c Cursor) Advance(observations []Observation, limit int) (Cursor, error) {
	if limit < 1 {
		return Cursor{}, ErrBoundaryLimit
	}
	latest := c.Value
	var stream Stream
	for _, o := range observations {
		if err := o.Validate(); err != nil {
			return Cursor{}, err
		}
		if stream != "" && o.Stream != stream {
			return Cursor{}, ErrInvalidObservation
		}
		stream = o.Stream
		if o.Value > latest {
			latest = o.Value
		}
	}
	seen := map[string]bool{}
	if latest == c.Value {
		for _, id := range c.SourceIDs {
			seen[id] = true
		}
	}
	for _, o := range observations {
		if o.Value == latest {
			seen[o.SourceID] = true
		}
	}
	if len(seen) > limit {
		return Cursor{}, ErrBoundaryLimit
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return Cursor{Value: latest, SourceIDs: ids}, nil
}
