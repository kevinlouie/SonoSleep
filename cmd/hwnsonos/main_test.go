package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestCloseHTTPEndsInfiniteStreams checks that shutdown doesn't wait on a
// /stream-like handler that only returns when its request context is done.
func TestCloseHTTPEndsInfiniteStreams(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	streamsCtx, cancelStreams := context.WithCancel(context.Background())
	defer cancelStreams()
	started := make(chan struct{})
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done() // like the stream handler: runs until cancelled
		}),
		BaseContext: func(net.Listener) context.Context { return streamsCtx },
	}
	served := make(chan struct{})
	go func() { _ = srv.Serve(ln); close(served) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	<-started

	begin := time.Now()
	closeHTTP(srv, cancelStreams, 5*time.Second)
	if d := time.Since(begin); d > time.Second {
		t.Errorf("closeHTTP took %v, want well under the 5s timeout", d)
	}
	_, _ = io.Copy(io.Discard, resp.Body) // body ends once the handler returns
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after closeHTTP")
	}
}
