package mal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"kotantei/internal/update"
)

func entryRevision(kind string, entry Entry) (update.Observation, error) {
	if entry.Node == nil || entry.Node.ID <= 0 || entry.ListStatus == nil {
		return update.Observation{}, ErrMalformed
	}
	status := entry.ListStatus
	if status.Score == nil || *status.Score < 0 || *status.Score > 10 || status.UpdatedAt.IsZero() || status.UpdatedAt.Unix() < 0 || status.UpdatedAt.Year() > 9999 || status.TimesRewatched < 0 || status.TimesReread < 0 {
		return update.Observation{}, ErrMalformed
	}
	stream := update.Anime
	progress, volumes, repeating, times := 0, 0, false, 0
	if kind == "anime" {
		if !slices.Contains([]string{"watching", "completed", "on_hold", "dropped", "plan_to_watch"}, status.Status) || status.EpisodesWatched == nil || *status.EpisodesWatched < 0 {
			return update.Observation{}, ErrMalformed
		}
		progress = *status.EpisodesWatched
		repeating = status.Rewatching
		times = status.TimesRewatched
	} else if kind == "manga" {
		if !slices.Contains([]string{"reading", "completed", "on_hold", "dropped", "plan_to_read"}, status.Status) || status.ChaptersRead == nil || status.VolumesRead == nil || *status.ChaptersRead < 0 || *status.VolumesRead < 0 {
			return update.Observation{}, ErrMalformed
		}
		stream = update.Manga
		progress = *status.ChaptersRead
		volumes = *status.VolumesRead
		repeating = status.Rereading
		times = status.TimesReread
	} else {
		return update.Observation{}, ErrMalformed
	}
	if !validDate(status.StartDate) || !validDate(status.FinishDate) {
		return update.Observation{}, ErrMalformed
	}
	tags := slices.Clone(status.Tags)
	slices.Sort(tags)
	tags = slices.Compact(tags)
	if tags == nil {
		tags = []string{}
	}
	encoded, err := json.Marshal(struct {
		Status                          string
		Score, Progress, Volumes        int
		Repeating                       bool
		Times                           int
		Tags                            []string
		Comments, StartDate, FinishDate string
	}{status.Status, *status.Score, progress, volumes, repeating, times, tags, status.Comments, status.StartDate, status.FinishDate})
	if err != nil {
		return update.Observation{}, ErrMalformed
	}
	hash := sha256.Sum256(encoded)
	at := status.UpdatedAt.UTC().Truncate(time.Microsecond).UnixMicro()
	return update.Observation{Stream: stream, Value: at, SourceID: strconv.FormatInt(entry.Node.ID, 10) + ":" + strconv.FormatInt(at, 10) + ":" + hex.EncodeToString(hash[:])}, nil
}

func validDate(value string) bool {
	if value == "" || strings.ReplaceAll(value, "0", "") == "--" || value == "0000" || value == "0000-00" {
		return true
	}
	layout := ""
	switch len(value) {
	case 4:
		layout = "2006"
	case 7:
		layout = "2006-01"
	case 10:
		layout = time.DateOnly
	default:
		return false
	}
	at, err := time.Parse(layout, value)
	return err == nil && at.Year() > 0
}
