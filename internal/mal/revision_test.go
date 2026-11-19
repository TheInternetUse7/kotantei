package mal

import (
	"strings"
	"testing"
	"time"
)

func integer(n int) *int { return &n }
func sampleEntry(id, seconds int64, kind string) Entry {
	status := &ListStatus{Status: "watching", Score: integer(8), UpdatedAt: time.Unix(seconds, 0).UTC(), EpisodesWatched: integer(3)}
	media := &Media{ID: id, Title: "Title", Format: "tv", Status: "finished_airing", Episodes: 12, Picture: Picture{Large: "https://cdn.myanimelist.net/cover.jpg"}}
	if kind == "manga" {
		status.Status = "reading"
		status.ChaptersRead = integer(4)
		status.VolumesRead = integer(2)
		media.Format = "manga"
		media.Status = "finished"
		media.Episodes = 0
		media.Chapters = 100
		media.Volumes = 12
	}
	return Entry{Node: media, ListStatus: status}
}

func TestMALRevisionUsesMicrosecondsAndExcludesDisplayMetadata(t *testing.T) {
	entry := sampleEntry(42, 100, "anime")
	entry.ListStatus.UpdatedAt = entry.ListStatus.UpdatedAt.Add(100 * time.Microsecond)
	first, err := entryRevision("anime", entry)
	if err != nil {
		t.Fatal(err)
	}
	entry.Node.Title = "Renamed title"
	entry.Node.Picture.Large = "https://cdn.myanimelist.net/changed.jpg"
	entry.Node.Genres = []Catalog{{Name: "Drama"}}
	second, err := entryRevision("anime", entry)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first.Value != 100000100 || !strings.HasPrefix(first.SourceID, "42:100000100:") || len(first.SourceID) > 256 {
		t.Fatalf("unstable or imprecise revision: %+v %+v", first, second)
	}
}

func TestMALRevisionChangesForEveryTrackedFieldAtTheSameTimestamp(t *testing.T) {
	base, err := entryRevision("anime", sampleEntry(42, 100, "anime"))
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ListStatus){
		func(s *ListStatus) { s.Status = "completed" }, func(s *ListStatus) { s.Score = integer(9) }, func(s *ListStatus) { s.EpisodesWatched = integer(4) },
		func(s *ListStatus) { s.Rewatching = true }, func(s *ListStatus) { s.TimesRewatched = 1 }, func(s *ListStatus) { s.Tags = []string{"Tag"} },
		func(s *ListStatus) { s.Comments = "Comment" }, func(s *ListStatus) { s.StartDate = "2020-01" }, func(s *ListStatus) { s.FinishDate = "2020" },
	} {
		entry := sampleEntry(42, 100, "anime")
		change(entry.ListStatus)
		changed, err := entryRevision("anime", entry)
		if err != nil {
			t.Fatal(err)
		}
		if changed.SourceID == base.SourceID {
			t.Fatal("tracked change had the same revision")
		}
	}
	manga := sampleEntry(43, 100, "manga")
	before, err := entryRevision("manga", manga)
	if err != nil {
		t.Fatal(err)
	}
	manga.ListStatus.VolumesRead = integer(3)
	after, err := entryRevision("manga", manga)
	if err != nil {
		t.Fatal(err)
	}
	if before.SourceID == after.SourceID {
		t.Fatal("volume-only revision was lost")
	}
}

func TestMALRevisionRejectsIncompleteRequiredListState(t *testing.T) {
	for _, change := range []func(*Entry){
		func(e *Entry) { e.Node = nil }, func(e *Entry) { e.Node.ID = 0 }, func(e *Entry) { e.ListStatus = nil },
		func(e *Entry) { e.ListStatus.Score = nil }, func(e *Entry) { e.ListStatus.Score = integer(11) }, func(e *Entry) { e.ListStatus.EpisodesWatched = nil },
		func(e *Entry) { e.ListStatus.Status = "unknown" }, func(e *Entry) { e.ListStatus.UpdatedAt = time.Time{} }, func(e *Entry) { e.ListStatus.EpisodesWatched = integer(-1) },
	} {
		entry := sampleEntry(42, 100, "anime")
		change(&entry)
		if _, err := entryRevision("anime", entry); err == nil {
			t.Fatal("incomplete list state accepted")
		}
	}
}
