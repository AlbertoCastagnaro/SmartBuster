package httpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientDoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("target reached"))
	}))
	defer srv.Close()

	c := New(Config{})
	resp, _, err := c.Do(context.Background(), srv.URL+"/redirect")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected redirect to be surfaced as 302, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/target" {
		t.Fatalf("expected Location header preserved, got %q", loc)
	}
}

func TestClientPerRequestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{RequestTimeout: 50 * time.Millisecond})
	_, _, err := c.Do(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestClientReturnsElapsedTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{})
	resp, elapsed, err := c.Do(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()
	if elapsed <= 0 {
		t.Fatalf("expected positive elapsed time, got %v", elapsed)
	}
}
