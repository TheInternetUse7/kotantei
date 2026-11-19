package anilist

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientCloseReleasesIdleHTTPConnections(t *testing.T) {
	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"Page":{"pageInfo":{"currentPage":1,"hasNextPage":false},"items":[]}}}`)
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
	defer server.Close()
	client, err := NewClient(ClientOptions{Endpoint: server.URL, HTTPClient: server.Client()})
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
