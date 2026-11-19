package mal

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"kotantei/internal/update"
)

func closeServer() (*httptest.Server, <-chan struct{}) {
	closed := make(chan struct{}, 2)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/anime/42" {
			fmt.Fprint(w, `{"data":{"mal_id":42,"title":"Title"}}`)
		} else {
			fmt.Fprint(w, `{"data":[],"paging":{}}`)
		}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	server.Start()
	return server, closed
}

func TestClientCloseReleasesIdleHTTPConnections(t *testing.T) {
	server, closed := closeServer()
	defer server.Close()
	client, err := NewClient(ClientOptions{ClientID: "test-client", APIEndpoint: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Search(context.Background(), "anime", "Title", ""); err != nil {
		t.Fatal(err)
	}
	client.Close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("client close left an idle HTTP connection open")
	}
}

func TestEnricherCloseReleasesIdleHTTPConnections(t *testing.T) {
	server, closed := closeServer()
	defer server.Close()
	enricher, err := NewEnricher(EnricherOptions{Endpoint: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enricher.Lookup(context.Background(), update.Subject{Kind: update.AnimeSubject, ID: "42"}, 0); err != nil {
		t.Fatal(err)
	}
	enricher.Close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("enricher close left an idle HTTP connection open")
	}
}
