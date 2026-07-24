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
	resp, err := c.Do(context.Background(), Request{URL: srv.URL + "/redirect"})
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
	_, err := c.Do(context.Background(), Request{URL: srv.URL})
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
	resp, err := c.Do(context.Background(), Request{URL: srv.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()
	if resp.Elapsed <= 0 {
		t.Fatalf("expected positive elapsed time, got %v", resp.Elapsed)
	}
}

func TestClientAppliesRequestHeaders(t *testing.T) {
	var gotUA, gotReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotReferer = r.Header.Get("Referer")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{})
	headers := http.Header{}
	headers.Set("User-Agent", "test-agent/1.0")
	headers.Set("Referer", "https://example.com/parent")
	resp, err := c.Do(context.Background(), Request{URL: srv.URL, Headers: headers})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	if gotUA != "test-agent/1.0" {
		t.Fatalf("expected the request's Headers to set User-Agent, got %q", gotUA)
	}
	if gotReferer != "https://example.com/parent" {
		t.Fatalf("expected the request's Headers to set Referer, got %q", gotReferer)
	}
}

func TestClientFallsBackToConfiguredUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{UserAgent: "fallback-agent/1.0"})
	resp, err := c.Do(context.Background(), Request{URL: srv.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	if gotUA != "fallback-agent/1.0" {
		t.Fatalf("expected fallback to Client's configured UserAgent when Headers is nil, got %q", gotUA)
	}
}
