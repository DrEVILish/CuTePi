package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDrainServerWaitsForActiveRequest(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	server.Start()
	defer server.Close()
	requestDone := make(chan error, 1)
	go func() {
		resp, err := http.Get(server.URL)
		if err == nil {
			resp.Body.Close()
		}
		requestDone <- err
	}()
	<-started
	cleaned := make(chan struct{})
	go drainServer(server.Config, func() { close(cleaned) })
	select {
	case <-cleaned:
		t.Fatal("cleanup ran while request was active")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-cleaned:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}
