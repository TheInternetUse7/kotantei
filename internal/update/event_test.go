package update

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func exampleEvent(source string, at time.Time) Event {
	return Event{Stream: Activity, Kind: List, SourceID: source, Subject: Subject{Kind: AnimeSubject, ID: "42"}, OccurredAt: at, Payload: json.RawMessage(`{"title":"Example"}`)}
}

func TestEventKeyIsStableAcrossMetadataAndPollRevisions(t *testing.T) {
	first := exampleEvent("100", time.Unix(100, 0))
	key, err := first.Key(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.Payload = json.RawMessage(`{"title":"Different title","score":8}`)
	second.OccurredAt = first.OccurredAt.In(time.FixedZone("offset", 3600))
	other, err := second.Key(1, 7)
	if err != nil {
		t.Fatal(err)
	}
	if key != other || len(key) != 64 {
		t.Fatalf("unstable event key: %s / %s", key, other)
	}
	for _, event := range []Event{exampleEvent("101", first.OccurredAt), {Stream: ReviewStream, Kind: Review, SourceID: "100", Subject: first.Subject, OccurredAt: first.OccurredAt, Payload: first.Payload}} {
		different, err := event.Key(1, 0)
		if err != nil {
			t.Fatal(err)
		}
		if different == key {
			t.Fatal("distinct source events share a key")
		}
	}
	accountKey, err := first.Key(2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if accountKey == key {
		t.Fatal("account namespace was omitted")
	}
}

func TestFavoriteKeysIncludeTheSnapshotGeneration(t *testing.T) {
	event := exampleEvent("anime:42", time.Unix(100, 0))
	event.Stream = Favorites
	event.Kind = FavoriteAdded
	first, err := event.Key(1, 5)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := event.Key(1, 5)
	if err != nil {
		t.Fatal(err)
	}
	later, err := event.Key(1, 7)
	if err != nil {
		t.Fatal(err)
	}
	if first != retry || first == later {
		t.Fatal("favorite add/remove/add cycles cannot be distinguished")
	}
}

func TestNormalizeKeepsLatestListUpdatePerTitle(t *testing.T) {
	start := time.Unix(100, 0)
	old := exampleEvent("1", start)
	latest := exampleEvent("3", start.Add(2*time.Second))
	other := exampleEvent("2", start.Add(time.Second))
	other.Subject.ID = "43"
	input := []Event{latest, old, other}
	got, err := Normalize(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SourceID != "2" || got[1].SourceID != "3" {
		t.Fatalf("unexpected consolidation: %+v", got)
	}
	if input[0].SourceID != "3" || input[1].SourceID != "1" {
		t.Fatal("normalization changed caller-owned input")
	}
}

func TestNormalizeUsesNumericSourceOrderForTimestampTies(t *testing.T) {
	at := time.Unix(100, 0)
	got, err := Normalize([]Event{exampleEvent("10", at), exampleEvent("9", at)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SourceID != "10" {
		t.Fatalf("timestamp tie chose wrong activity: %+v", got)
	}
}

func TestNormalizeUsesConsistentOrderForMixedTimestampTieIDs(t *testing.T) {
	at := time.Unix(100, 0)
	events := []Event{exampleEvent("2", at), exampleEvent("10", at), exampleEvent("1a", at)}
	events[1].Subject.ID = "43"
	events[2].Subject.ID = "44"
	for i, first := range events {
		for _, second := range events[i+1:] {
			t.Run(first.SourceID+"/"+second.SourceID, func(t *testing.T) {
				got, err := Normalize([]Event{second, first})
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != 2 || got[0].SourceID != first.SourceID || got[1].SourceID != second.SourceID {
					t.Fatalf("mixed identities do not have a consistent tie order: %+v", got)
				}
			})
		}
	}
}

func TestNormalizeDoesNotMergeReviewsOrDifferentMediaKinds(t *testing.T) {
	at := time.Unix(100, 0)
	review := exampleEvent("10", at)
	review.Stream = ReviewStream
	review.Kind = Review
	another := review
	another.SourceID = "11"
	manga := exampleEvent("12", at)
	manga.Subject.Kind = MangaSubject
	anime := exampleEvent("13", at)
	got, err := Normalize([]Event{review, another, manga, anime, review})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("lost reviews/media or failed to deduplicate: %+v", got)
	}
	for _, event := range got {
		if event.OccurredAt.Location() != time.UTC {
			t.Fatal("event time was not normalized to UTC")
		}
	}
}

func TestNormalizeRejectsConflictingSourceIdentities(t *testing.T) {
	first := exampleEvent("1", time.Unix(100, 0))
	second := first
	second.Subject.ID = "43"
	if _, err := Normalize([]Event{first, second}); err == nil {
		t.Fatal("accepted conflicting subjects for one source event")
	}
}

func TestInvalidEventsAreRejectedWithoutExposingPayloads(t *testing.T) {
	valid := exampleEvent("1", time.Unix(100, 0))
	mutations := []func(*Event){
		func(e *Event) { e.SourceID = "" }, func(e *Event) { e.SourceID = strings.Repeat("x", 257) },
		func(e *Event) { e.SourceID = string([]byte{0xff}) }, func(e *Event) { e.Stream = "unknown" },
		func(e *Event) { e.Kind = "unknown" }, func(e *Event) { e.Subject.ID = "0" },
		func(e *Event) { e.Subject.Kind = "unknown" }, func(e *Event) { e.OccurredAt = time.Time{} },
		func(e *Event) { e.OccurredAt = time.Unix(-1, 0) }, func(e *Event) { e.Payload = json.RawMessage(`null`) },
		func(e *Event) { e.Payload = json.RawMessage(`[]`) }, func(e *Event) { e.Payload = json.RawMessage(`{"secret":"DO-NOT-PRINT"`) },
		func(e *Event) { e.Stream = Anime; e.Subject.Kind = MangaSubject },
	}
	for n, mutate := range mutations {
		event := valid
		mutate(&event)
		if err := event.Validate(); err == nil || strings.Contains(err.Error(), "DO-NOT-PRINT") {
			t.Fatalf("mutation %d: unsafe/missing validation: %v", n, err)
		}
	}
	if _, err := valid.Key(0, 0); err == nil {
		t.Fatal("accepted empty account identity")
	}
	if _, err := valid.Key(1, -1); err == nil {
		t.Fatal("accepted a negative generation")
	}
}

func TestEventObservationUsesProviderCheckpointPrecision(t *testing.T) {
	at := time.Unix(100, 123456789)
	for _, tc := range []struct {
		stream  Stream
		want    int64
		tracked bool
	}{{Activity, 100, true}, {ReviewStream, 100, true}, {Anime, 100123456, true}, {Manga, 100123456, true}, {Favorites, 0, false}} {
		event := exampleEvent("1", at)
		event.Stream = tc.stream
		got, tracked := event.Observation()
		if tracked != tc.tracked || (tracked && (got.Value != tc.want || got.SourceID != "1" || got.Stream != tc.stream)) {
			t.Fatalf("%s: %+v tracked=%v", tc.stream, got, tracked)
		}
	}
}

func TestCursorIncludesOlderAndSeenBoundaryRecords(t *testing.T) {
	cursor := Cursor{Value: 10, SourceIDs: []string{"1"}}
	for _, tc := range []struct {
		value int64
		id    string
		seen  bool
	}{{9, "new", true}, {10, "1", true}, {10, "2", false}, {11, "1", false}} {
		if got := cursor.Contains(Observation{Stream: Activity, Value: tc.value, SourceID: tc.id}); got != tc.seen {
			t.Fatalf("%+v: seen=%v", tc, got)
		}
	}
}

func TestCursorAdvanceKeepsAllIdentitiesAtTheLatestBoundary(t *testing.T) {
	cursor := Cursor{Value: 10, SourceIDs: []string{"1"}}
	got, err := cursor.Advance([]Observation{{Stream: Activity, Value: 10, SourceID: "2"}, {Stream: Activity, Value: 10, SourceID: "1"}}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != 10 || !reflect.DeepEqual(got.SourceIDs, []string{"1", "2"}) {
		t.Fatalf("lost equal-timestamp identities: %+v", got)
	}
	got, err = cursor.Advance([]Observation{{Stream: Activity, Value: 11, SourceID: "next"}, {Stream: Activity, Value: 10, SourceID: "ignored"}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != 11 || !reflect.DeepEqual(got.SourceIDs, []string{"next"}) {
		t.Fatalf("incorrect high-water boundary: %+v", got)
	}
	if !reflect.DeepEqual(cursor.SourceIDs, []string{"1"}) {
		t.Fatal("cursor advance mutated its input")
	}
}

func TestCursorAdvanceRefusesToForgetBoundaryIdentitiesOnOverflow(t *testing.T) {
	cursor := Cursor{Value: 10, SourceIDs: []string{"1"}}
	if _, err := cursor.Advance([]Observation{{Stream: Activity, Value: 10, SourceID: "2"}}, 1); err == nil {
		t.Fatal("boundary overflow silently dropped identities")
	}
	if !reflect.DeepEqual(cursor.SourceIDs, []string{"1"}) {
		t.Fatal("failed advance changed the cursor")
	}
	if _, err := cursor.Advance([]Observation{{Stream: Activity, Value: -1, SourceID: "bad"}}, 10); err == nil {
		t.Fatal("invalid observation accepted")
	}
}
