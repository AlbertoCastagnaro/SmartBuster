package seed

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newStubCDX(t *testing.T, rows [][2]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString(`[["original","timestamp"]`)
		for _, row := range rows {
			fmt.Fprintf(&b, `,["%s","%s"]`, row[0], row[1])
		}
		b.WriteString("]")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(b.String()))
	}))
}

func TestWayback_ParsesCDXAndScopeFilters(t *testing.T) {
	stub := newStubCDX(t, [][2]string{
		{"http://example.test/old/admin/config.php", "20230101000000"},
		{"http://other-host.test/should-be-dropped", "20230101000000"},
	})
	defer stub.Close()

	paced := 0
	w := &Wayback{BaseURL: stub.URL, Pace: func() { paced++ }}
	seeds, err := w.Fetch(context.Background(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(seeds) != 1 || seeds[0].Path != "/old/admin/config.php" {
		t.Fatalf("seeds = %+v, want exactly the in-scope /old/admin/config.php", seeds)
	}
	if seeds[0].Source != "wayback:20230101" {
		t.Errorf("Source = %q, want %q", seeds[0].Source, "wayback:20230101")
	}
	if paced != 1 {
		t.Errorf("Pace called %d times, want 1 (the separate archive.org limiter)", paced)
	}
}

func TestWayback_EmptyResultIsNotAnError(t *testing.T) {
	stub := newStubCDX(t, nil)
	defer stub.Close()

	w := &Wayback{BaseURL: stub.URL}
	seeds, err := w.Fetch(context.Background(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(seeds) != 0 {
		t.Errorf("seeds = %+v, want none", seeds)
	}
}

func TestWayback_TransportFailureIsAnError(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := stub.URL
	stub.Close() // nothing listening: a transport failure, not a graceful 404

	w := &Wayback{BaseURL: base}
	if _, err := w.Fetch(context.Background(), "example.test"); err == nil {
		t.Fatal("expected an error for an unreachable CDX endpoint")
	}
}
