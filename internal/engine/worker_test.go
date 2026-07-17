package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/httpclient"
)

func TestWorkerProducesSignatureForNormalResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("<html>hello world this is a page</html>"))
	}))
	defer srv.Close()

	client := httpclient.New(httpclient.Config{})
	workCh := make(chan WorkItem, 1)
	resultsCh := make(chan WorkResult, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RunWorker(ctx, workCh, resultsCh, client)
	workCh <- WorkItem{URL: srv.URL + "/hello", Candidate: Candidate{Path: "hello"}}

	select {
	case res := <-resultsCh:
		if res.Err != nil {
			t.Fatalf("unexpected error: %v", res.Err)
		}
		if res.Signature.Status != http.StatusOK {
			t.Fatalf("expected 200, got %d", res.Signature.Status)
		}
		if res.Signature.ContentType != "text/html" {
			t.Fatalf("expected text/html, got %q", res.Signature.ContentType)
		}
		if res.Signature.WordCount == 0 {
			t.Fatal("expected nonzero word count")
		}
		if res.Signature.SimHash == 0 {
			t.Fatal("expected nonzero simhash for non-empty body")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result")
	}
}

func TestWorkerStripsReflectedPathToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "<html>Page %s not found</html>", r.URL.Path)
	}))
	defer srv.Close()

	client := httpclient.New(httpclient.Config{})
	workCh := make(chan WorkItem, 2)
	resultsCh := make(chan WorkResult, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RunWorker(ctx, workCh, resultsCh, client)
	workCh <- WorkItem{URL: srv.URL + "/aaaaaaaaaa", IsProbe: true}
	workCh <- WorkItem{URL: srv.URL + "/bbbbbbbbbb", IsProbe: true}

	var sigs []ResponseSignature
	for i := 0; i < 2; i++ {
		select {
		case res := <-resultsCh:
			if res.Err != nil {
				t.Fatalf("unexpected error: %v", res.Err)
			}
			sigs = append(sigs, res.Signature)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for result")
		}
	}
	if sigs[0].SimHash != sigs[1].SimHash {
		t.Fatalf("expected identical simhash after reflected-path stripping, got %x vs %x", sigs[0].SimHash, sigs[1].SimHash)
	}
	if !sigs[0].Reflected || !sigs[1].Reflected {
		t.Fatal("expected Reflected=true for probes whose path is echoed in the body")
	}
}

func TestWorkerNormalizesRedirectLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://elsewhere.example/login?from="+r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()

	client := httpclient.New(httpclient.Config{})
	workCh := make(chan WorkItem, 1)
	resultsCh := make(chan WorkResult, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RunWorker(ctx, workCh, resultsCh, client)
	workCh <- WorkItem{URL: srv.URL + "/secret"}

	select {
	case res := <-resultsCh:
		if res.Err != nil {
			t.Fatalf("unexpected error: %v", res.Err)
		}
		if res.Signature.RedirectTo != "/login" {
			t.Fatalf("expected normalized redirect '/login', got %q", res.Signature.RedirectTo)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result")
	}
}

func TestWorkerReportsNetworkError(t *testing.T) {
	client := httpclient.New(httpclient.Config{RequestTimeout: 100 * time.Millisecond})
	workCh := make(chan WorkItem, 1)
	resultsCh := make(chan WorkResult, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RunWorker(ctx, workCh, resultsCh, client)
	workCh <- WorkItem{URL: "http://127.0.0.1:1/unreachable"}

	select {
	case res := <-resultsCh:
		if res.Err == nil {
			t.Fatal("expected error for unreachable host")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result")
	}
}

func TestWorkerExitsOnContextCancellation(t *testing.T) {
	workCh := make(chan WorkItem)
	resultsCh := make(chan WorkResult)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		RunWorker(ctx, workCh, resultsCh, httpclient.New(httpclient.Config{}))
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not exit after context cancellation")
	}
}
