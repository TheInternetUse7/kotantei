package bot

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/diamondburned/arikawa/v3/api"
	sdkrate "github.com/diamondburned/arikawa/v3/api/rate"
	"github.com/diamondburned/arikawa/v3/utils/httputil/httpdriver"
	"golang.org/x/time/rate"
)

type discordRateBucket struct {
	limiter         *sdkrate.Limiter
	gate            chan struct{}
	users           int
	used, keepUntil time.Time
}
type discordRateLimits struct {
	mu      sync.Mutex
	buckets map[string]*discordRateBucket
	max     int
	global  time.Time
	pace    *rate.Limiter
}

func newDiscordRateLimits(maxBuckets int) *discordRateLimits {
	return &discordRateLimits{buckets: map[string]*discordRateBucket{}, max: maxBuckets, pace: rate.NewLimiter(rate.Every(25*time.Millisecond), 1)}
}

func (l *discordRateLimits) acquire(request httpdriver.Request) error {
	ctx := request.GetContext()
	trace, _ := ctx.Value(traceKey{}).(*requestTrace)
	if trace == nil {
		return ErrDiscordMalformed
	}
	if err := l.pace.Wait(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrDiscordRateLimited
	}
	l.mu.Lock()
	global := l.global
	l.mu.Unlock()
	if delay := time.Until(global); delay > 0 {
		trace.retryAfter = max(trace.retryAfter, delay)
		if err := waitDiscordRate(ctx, delay); err != nil {
			return err
		}
	}
	native, ok := request.(*httpdriver.DefaultRequest)
	if !ok {
		return ErrDiscordMalformed
	}
	key := (*http.Request)(native).Method + " " + sdkrate.ParseBucketKey(strings.TrimPrefix(request.GetPath(), api.Path))
	now := time.Now()
	l.mu.Lock()
	for key, bucket := range l.buckets {
		if bucket.users == 0 && !bucket.keepUntil.After(now) && now.Sub(bucket.used) >= 15*time.Minute {
			delete(l.buckets, key)
		}
	}
	bucket := l.buckets[key]
	if bucket == nil {
		if len(l.buckets) >= l.max {
			oldest := ""
			var used time.Time
			for key, candidate := range l.buckets {
				if candidate.users == 0 && !candidate.keepUntil.After(now) && (oldest == "" || candidate.used.Before(used)) {
					oldest, used = key, candidate.used
				}
			}
			if oldest == "" {
				trace.retryAfter = time.Second
				for _, candidate := range l.buckets {
					if delay := candidate.keepUntil.Sub(now); delay > 0 {
						trace.retryAfter = max(trace.retryAfter, delay)
					}
				}
				l.mu.Unlock()
				return ErrDiscordRateLimited
			}
			delete(l.buckets, oldest)
		}
		bucket = &discordRateBucket{limiter: sdkrate.NewLimiter(""), gate: make(chan struct{}, 1), used: now}
		l.buckets[key] = bucket
	}
	bucket.users++
	trace.bucket = bucket
	l.mu.Unlock()
	select {
	case bucket.gate <- struct{}{}:
		trace.held = true
	case <-ctx.Done():
		return ctx.Err()
	}
	err := bucket.limiter.Acquire(ctx, "/route")
	// Arikawa calls response cleanup even after a failed request option. Only
	// release a lock this request owns, not another request waiting on this route.
	trace.acquired = err == nil || errors.Is(err, sdkrate.ErrTimedOutEarly)
	if errors.Is(err, sdkrate.ErrTimedOutEarly) {
		l.mu.Lock()
		trace.retryAfter = max(trace.retryAfter, time.Until(bucket.keepUntil))
		l.mu.Unlock()
		return ErrDiscordRateLimited
	}
	return err
}

func (l *discordRateLimits) release(request httpdriver.Request, header http.Header) error {
	if header == nil {
		// Failed request options can still have a pipe-backed SDK JSON body;
		// close it so its encoder cannot remain blocked without a transport.
		if native, ok := request.(*httpdriver.DefaultRequest); ok && (*http.Request)(native).Body != nil {
			(*http.Request)(native).Body.Close()
		}
	}
	trace, _ := request.GetContext().Value(traceKey{}).(*requestTrace)
	if trace == nil || trace.bucket == nil {
		return nil
	}
	bucket := trace.bucket
	var err error
	if trace.acquired {
		err = bucket.limiter.Release("/route", header)
	}
	l.mu.Lock()
	if trace.acquired && header != nil {
		until := discordHeaderReset(header)
		if until.After(bucket.keepUntil) {
			bucket.keepUntil = until
		}
		if header.Get("X-RateLimit-Global") == "true" && until.After(l.global) {
			l.global = until
		}
	}
	bucket.used = time.Now()
	bucket.users--
	l.mu.Unlock()
	if trace.held {
		<-bucket.gate
	}
	trace.bucket = nil
	trace.held = false
	trace.acquired = false
	return err
}

func waitDiscordRate(ctx context.Context, delay time.Duration) error {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < delay {
		return ErrDiscordRateLimited
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func discordHeaderReset(header http.Header) time.Time {
	now := time.Now()
	until := now
	if seconds, err := strconv.ParseFloat(header.Get("Retry-After"), 64); err == nil && seconds > 0 && seconds < float64((7*24*time.Hour)/time.Second) {
		until = now.Add(time.Duration(seconds * float64(time.Second)))
	}
	if epoch, err := strconv.ParseFloat(header.Get("X-RateLimit-Reset"), 64); err == nil && epoch > 0 && !math.IsInf(epoch, 0) && epoch < float64(math.MaxInt64)/float64(time.Second) {
		candidate := time.Unix(0, int64(epoch*float64(time.Second))).Add(sdkrate.ExtraDelay)
		if candidate.After(until) {
			until = candidate
		}
	}
	return until
}

// The SDK expects integer Retry-After and does not read a 429 body's global
// flag. Normalize those bounded protocol fields without changing body errors.
func normalizeDiscordLimits(response *http.Response, body []byte) {
	header := response.Header
	after, _ := strconv.ParseFloat(header.Get("Retry-After"), 64)
	if response.StatusCode == 429 {
		var limit struct {
			After  float64 `json:"retry_after"`
			Global bool    `json:"global"`
		}
		if json.Unmarshal(body, &limit) == nil {
			after = max(after, limit.After)
			if limit.Global {
				header.Set("X-RateLimit-Global", "true")
			}
		}
		if header.Get("X-RateLimit-Scope") == "global" {
			header.Set("X-RateLimit-Global", "true")
		}
	}
	if after > 0 && !math.IsInf(after, 0) && after < float64((7*24*time.Hour)/time.Second) {
		header.Set("Retry-After", strconv.FormatInt(int64(math.Ceil(after)), 10))
	}
	if seconds, err := strconv.ParseFloat(header.Get("X-RateLimit-Reset-After"), 64); err == nil && seconds > 0 && seconds < float64((7*24*time.Hour)/time.Second) {
		header.Set("X-RateLimit-Reset", strconv.FormatFloat(float64(time.Now().UnixNano())/float64(time.Second)+seconds, 'f', 6, 64))
	}
}
