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
)

var (
	ErrNotFound     = errors.New("MAL public account or item was not found")
	ErrNotPublic    = errors.New("MAL list is private or the public request was blocked")
	ErrUnauthorized = errors.New("MAL client authentication was rejected")
	ErrUnavailable  = errors.New("MAL public service is unavailable")
	ErrRateLimited  = errors.New("MAL request was rate limited; retry after its cooldown")
)

type HTTPError struct {
	Cause      error
	Status     int
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string { return e.Cause.Error() }
func (e *HTTPError) Unwrap() error { return e.Cause }

type ClientOptions struct {
	ClientID         string
	APIEndpoint      string
	ProfileEndpoint  string
	HTTPClient       *http.Client
	RequestInterval  time.Duration
	RequestTimeout   time.Duration
	MaxResponseBytes int64
	Now              func() time.Time
	Wait             func(context.Context, time.Duration) error
}
type Client struct {
	options ClientOptions
	api     *httpRoute
	website *httpRoute
	pacer   *requestPacer
	http    *http.Client
}

// Close releases idle connections after all tracking/search work has joined.
func (c *Client) Close() { c.http.CloseIdleConnections() }

type ListPage struct {
	Entries    []Entry
	NextOffset *int
}

func NewClient(options ClientOptions) (*Client, error) {
	if options.APIEndpoint == "" {
		options.APIEndpoint = "https://api.myanimelist.net/v2"
	}
	if options.ProfileEndpoint == "" {
		options.ProfileEndpoint = "https://myanimelist.net"
	}
	if options.RequestInterval == 0 {
		options.RequestInterval = 3 * time.Second
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = 15 * time.Second
	}
	if options.MaxResponseBytes == 0 {
		options.MaxResponseBytes = 1 << 20
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Wait == nil {
		options.Wait = waitFor
	}
	api, err := parseEndpoint(options.APIEndpoint)
	if err != nil {
		return nil, err
	}
	website, err := parseEndpoint(options.ProfileEndpoint)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(options.ClientID) == "" || options.ClientID != strings.TrimSpace(options.ClientID) || strings.ContainsAny(options.ClientID, "\r\n") || len(options.ClientID) > 256 || options.RequestInterval <= 0 || options.RequestTimeout <= 0 || options.MaxResponseBytes < 1 || options.MaxResponseBytes > 16<<20 {
		return nil, errors.New("invalid MAL client ID or request limits")
	}
	return &Client{options: options, api: &httpRoute{base: api, credential: options.ClientID}, website: &httpRoute{base: website}, pacer: newPacer(), http: publicHTTPClient(options.HTTPClient, options.RequestTimeout)}, nil
}

func (c *Client) settings() requestSettings {
	return requestSettings{interval: c.options.RequestInterval, timeout: c.options.RequestTimeout, maxBytes: c.options.MaxResponseBytes, now: c.options.Now, wait: c.options.Wait}
}

func (c *Client) Profile(ctx context.Context, username string, favorites bool) (Profile, error) {
	if !validUsername(username) {
		return Profile{}, ErrMalformed
	}
	target := endpointURL(c.website.base, "profile/"+username, nil)
	for redirects := 0; redirects <= 3; redirects++ {
		response, err := getHTTP(ctx, c.http, c.pacer, c.website, c.settings(), target, true)
		if err != nil {
			return Profile{}, err
		}
		if response.status < 300 || response.status >= 400 {
			return parseProfile(response.body, favorites)
		}
		if redirects == 3 {
			return Profile{}, ErrMalformed
		}
		location, err := url.Parse(response.header.Get("Location"))
		if err != nil || response.header.Get("Location") == "" {
			return Profile{}, ErrMalformed
		}
		next := target.ResolveReference(location)
		prefix := strings.TrimRight(c.website.base.Path, "/") + "/profile/"
		if !sameOrigin(next, c.website.base) || next.User != nil || next.RawQuery != "" || next.Fragment != "" || !strings.HasPrefix(next.Path, prefix) || !validUsername(strings.TrimPrefix(next.Path, prefix)) {
			return Profile{}, ErrMalformed
		}
		target = next
	}
	return Profile{}, ErrMalformed
}

func (c *Client) RecoverUsername(ctx context.Context, id int64) (string, error) {
	if id <= 0 {
		return "", ErrMalformed
	}
	target := endpointURL(c.website.base, "comments.php", url.Values{"id": {strconv.FormatInt(id, 10)}})
	response, err := getHTTP(ctx, c.http, c.pacer, c.website, c.settings(), target, true)
	if err != nil {
		return "", err
	}
	if response.status >= 300 {
		return "", ErrMalformed
	}
	return parseComments(response.body)
}

func (c *Client) ListPage(ctx context.Context, username, kind string, offset, limit int, flags Features) (ListPage, error) {
	if !validUsername(username) || username == "." || username == ".." || (kind != "anime" && kind != "manga") || offset < 0 || limit < 1 || limit > 1000 {
		return ListPage{}, ErrMalformed
	}
	target := endpointURL(c.api.base, "users/"+username+"/"+kind+"list", url.Values{"fields": {listFields(kind, flags)}, "sort": {"list_updated_at"}, "nsfw": {"true"}, "limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}})
	response, err := getHTTP(ctx, c.http, c.pacer, c.api, c.settings(), target, false)
	if err != nil {
		return ListPage{}, err
	}
	if response.status >= 300 {
		return ListPage{}, ErrMalformed
	}
	var data struct {
		Data   []Entry `json:"data"`
		Paging *struct {
			Next string `json:"next"`
		} `json:"paging"`
		Error string `json:"error"`
	}
	if json.Unmarshal(response.body, &data) != nil || data.Data == nil || data.Paging == nil || data.Error != "" || len(data.Data) > limit {
		return ListPage{}, ErrMalformed
	}
	for _, entry := range data.Data {
		if _, err := entryRevision(kind, entry); err != nil {
			return ListPage{}, err
		}
	}
	result := ListPage{Entries: data.Data}
	if data.Paging.Next != "" {
		if len(data.Data) == 0 {
			return ListPage{}, ErrMalformed
		}
		next, err := url.Parse(data.Paging.Next)
		if err != nil {
			return ListPage{}, ErrMalformed
		}
		next = target.ResolveReference(next)
		if !sameOrigin(next, target) || next.User != nil || next.Path != target.Path || next.Fragment != "" {
			return ListPage{}, ErrMalformed
		}
		query, err := url.ParseQuery(next.RawQuery)
		if err != nil {
			return ListPage{}, ErrMalformed
		}
		for key, values := range query {
			if len(values) != 1 {
				return ListPage{}, ErrMalformed
			}
			if key != "offset" && (target.Query().Get(key) == "" || values[0] != target.Query().Get(key)) {
				return ListPage{}, ErrMalformed
			}
		}
		number, err := strconv.Atoi(query.Get("offset"))
		if err != nil || number <= offset || number-offset != limit {
			return ListPage{}, ErrMalformed
		}
		result.NextOffset = &number
	}
	return result, nil
}

func listFields(kind string, flags Features) string {
	status := "status,score,updated_at,tags,comments,start_date,finish_date"
	fields := "id,title,main_picture,media_type,status,mean"
	if kind == "anime" {
		status += ",num_episodes_watched,is_rewatching,num_times_rewatched"
		fields += ",num_episodes"
		if flags&Studio != 0 {
			fields += ",studios"
		}
	} else {
		status += ",num_chapters_read,num_volumes_read,is_rereading,num_times_reread"
		fields += ",num_chapters,num_volumes"
		if flags&Mangakas != 0 {
			fields += ",authors{first_name,last_name}"
		}
	}
	if flags&Genres != 0 {
		fields += ",genres"
	}
	if flags&Synopsis != 0 {
		fields += ",synopsis"
	}
	return fields + ",list_status{" + status + "}"
}
