// Package tracking stages bounded upstream page collections before committing
// a complete observation batch to the persistent outbox.
package tracking

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"time"

	"kotantei/internal/store"
	"kotantei/internal/update"
)

var ErrPaginationCycle = errors.New("upstream pagination repeated a page token")
var ErrCollectionBudget = errors.New("upstream collection budget exceeded; no checkpoint was advanced")

type Request struct {
	Snapshot store.Snapshot
	Token    string
}
type Page struct {
	Events       []update.Event
	Observations []update.Observation
	Next         string
}
type FetchPage func(context.Context, Request) (Page, error)
type CollectLimits struct {
	MaxPages        int
	MaxRecords      int
	MaxPayloadBytes int
	MaxBytes        int
}

func DefaultCollectLimits() CollectLimits {
	return CollectLimits{MaxPages: 64, MaxRecords: 512, MaxPayloadBytes: 64 << 10, MaxBytes: 4 << 20}
}

// Next is an opaque continuation token. An empty token means the adapter has
// confirmed the end of its stream. Adapters must cap HTTP response bodies too;
// this collector caps retained data, not allocations inside a fetch function.
func Collect(ctx context.Context, state store.Snapshot, fetch FetchPage, limits CollectLimits) (store.Poll, error) {
	if limits == (CollectLimits{}) {
		limits = DefaultCollectLimits()
	}
	if fetch == nil || state.AccountID <= 0 || state.Revision < 0 || limits.MaxPages < 1 || limits.MaxRecords < 1 || limits.MaxPayloadBytes < 1 || limits.MaxBytes < 1 {
		return store.Poll{}, errors.New("invalid page collector arguments/limits")
	}
	result := store.Poll{AccountID: state.AccountID, ExpectedRevision: state.Revision}
	token := ""
	seen := map[string]bool{"": true}
	bytesRead := 0
	for pageNumber := 0; ; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return store.Poll{}, err
		}
		if pageNumber >= limits.MaxPages {
			return store.Poll{}, ErrCollectionBudget
		}
		page, err := fetch(ctx, Request{Snapshot: cloneSnapshot(state), Token: token})
		if err != nil {
			return store.Poll{}, err
		}
		if err := ctx.Err(); err != nil {
			return store.Poll{}, err
		}
		remaining := limits.MaxRecords - len(result.Events) - len(result.Observations)
		if len(page.Events) > remaining || len(page.Observations) > remaining-len(page.Events) {
			return store.Poll{}, ErrCollectionBudget
		}
		for _, event := range page.Events {
			if len(event.Payload) > limits.MaxPayloadBytes {
				return store.Poll{}, ErrCollectionBudget
			}
			bytesRead += len(event.Payload) + len(event.SourceID) + len(event.Subject.ID)
			if bytesRead > limits.MaxBytes {
				return store.Poll{}, ErrCollectionBudget
			}
			if err := event.Validate(); err != nil {
				return store.Poll{}, err
			}
			event.Payload = bytes.Clone(event.Payload)
			result.Events = append(result.Events, event)
		}
		for _, observation := range page.Observations {
			if err := observation.Validate(); err != nil {
				return store.Poll{}, err
			}
			bytesRead += len(observation.SourceID)
			if bytesRead > limits.MaxBytes {
				return store.Poll{}, ErrCollectionBudget
			}
			result.Observations = append(result.Observations, observation)
		}
		if page.Next == "" {
			return result, nil
		}
		if len(page.Next) > 1024 {
			return store.Poll{}, ErrCollectionBudget
		}
		if seen[page.Next] {
			return store.Poll{}, ErrPaginationCycle
		}
		seen[page.Next] = true
		token = page.Next
	}
}

func Check(ctx context.Context, q *store.Outbox, accountID int64, now time.Time, fetch FetchPage, limits CollectLimits) (store.PollResult, error) {
	if q == nil {
		return store.PollResult{}, errors.New("account check requires an outbox")
	}
	state, err := q.Snapshot(ctx, accountID)
	if err != nil {
		return store.PollResult{}, err
	}
	input, err := Collect(ctx, state, fetch, limits)
	if err != nil {
		return store.PollResult{}, err
	}
	input.Now = now
	return q.Record(ctx, input)
}

func cloneSnapshot(state store.Snapshot) store.Snapshot {
	state.Colors = slices.Clone(state.Colors)
	cursors := make(map[update.Stream]update.Cursor, len(state.Cursors))
	for stream, cursor := range state.Cursors {
		cursor.SourceIDs = slices.Clone(cursor.SourceIDs)
		cursors[stream] = cursor
	}
	state.Cursors = cursors
	state.Favorites = slices.Clone(state.Favorites)
	for n := range state.Favorites {
		state.Favorites[n].Extra = bytes.Clone(state.Favorites[n].Extra)
	}
	return state
}
