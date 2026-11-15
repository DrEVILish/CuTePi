package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

func TestCloseInheritedFDsClosesOnlyInherited(t *testing.T) {
	var plain, cloexec [2]int
	if err := unix.Pipe2(cloexec[:], unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(cloexec[0])
	defer unix.Close(cloexec[1])
	if err := unix.Pipe2(plain[:], 0); err != nil { // as a C library opens a device
		t.Fatal(err)
	}
	closeInheritedFDs(cloexec[0])
	for _, fd := range plain {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			unix.Close(fd)
			t.Errorf("fd %d without close-on-exec left open", fd)
		}
	}
	for _, fd := range cloexec {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
			t.Errorf("close-on-exec fd %d closed: %v", fd, err)
		}
	}
}
