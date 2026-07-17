package calibration

import (
	"strings"
	"testing"
)

func TestNormalizeStripsReflectedToken(t *testing.T) {
	body := []byte(`<html>Page /admin-xyz not found</html>`)
	got := Normalize(body, "admin-xyz")
	if want := "not found"; !strings.Contains(got, want) {
		t.Fatalf("expected reflected token stripped, got %q (want to contain %q)", got, want)
	}
	if strings.Contains(got, "admin-xyz") {
		t.Fatalf("reflected token still present in normalized output: %q", got)
	}
}

func TestNormalizeIsNoOpWhenTokenNotReflected(t *testing.T) {
	body := []byte(`<html>not found</html>`)
	got := Normalize(body, "somethingelse")
	if !strings.Contains(got, "not found") {
		t.Fatalf("expected body preserved when token absent, got %q", got)
	}
}

func TestNormalizeDoesNotStripTokenAsSubstringOfOtherWords(t *testing.T) {
	// Regression: a raw substring strip of "test" would also eat the "test"
	// inside "latest" and "testing", corrupting unrelated content that just
	// happens to contain the token as a fragment.
	body := []byte(`<html>the latest testing results are in</html>`)
	got := Normalize(body, "test")
	if !strings.Contains(got, "latest") {
		t.Fatalf("expected 'latest' preserved (token is a substring, not a delimited word), got %q", got)
	}
	if !strings.Contains(got, "testing") {
		t.Fatalf("expected 'testing' preserved (token is a substring, not a delimited word), got %q", got)
	}
}

func TestNormalizeStripsTokenAsDelimitedPathSegment(t *testing.T) {
	body := []byte(`<html>Page /test not found, see /test/ or "test" for details</html>`)
	got := Normalize(body, "test")
	if strings.Contains(got, "test") {
		t.Fatalf("expected every delimited occurrence of the token stripped, got %q", got)
	}
}

func TestNormalizeCollapsesVolatileTimestamps(t *testing.T) {
	a := Normalize([]byte("error occurred at 2026-07-16T10:00:00.123Z please retry"), "")
	b := Normalize([]byte("error occurred at 2026-07-16T10:00:05.987Z please retry"), "")
	if a != b {
		t.Fatalf("expected timestamps to normalize identically:\n a=%q\n b=%q", a, b)
	}
}

func TestNormalizeCollapsesCSRFTokens(t *testing.T) {
	// The spec's regex targets inline key=value/key:"value" forms (JSON, JS,
	// headers); a separate HTML "value=" attribute is out of scope for it and
	// relies on the noise-floor fallback instead.
	a := Normalize([]byte(`{"csrf_token": "abc123def456"}`), "")
	b := Normalize([]byte(`{"csrf_token": "zzz999yyy888"}`), "")
	if a != b {
		t.Fatalf("expected csrf tokens to normalize identically:\n a=%q\n b=%q", a, b)
	}
}

func TestNormalizeCollapsesUUIDs(t *testing.T) {
	a := Normalize([]byte("session 550e8400-e29b-41d4-a716-446655440000 active"), "")
	b := Normalize([]byte("session 123e4567-e89b-12d3-a456-426614174000 active"), "")
	if a != b {
		t.Fatalf("expected uuids to normalize identically:\n a=%q\n b=%q", a, b)
	}
}

func TestNormalizeCollapsesHexBlobs(t *testing.T) {
	a := Normalize([]byte("hash: deadbeefcafebabe1234567890abcdef done"), "")
	b := Normalize([]byte("hash: 0011223344556677889900aabbccdd done"), "")
	if a != b {
		t.Fatalf("expected hex blobs to normalize identically:\n a=%q\n b=%q", a, b)
	}
}

func TestNormalizeCollapsesWhitespace(t *testing.T) {
	got := Normalize([]byte("a\n\n\tb   c"), "")
	if want := "a b c"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNormalizeCapsBody(t *testing.T) {
	big := make([]byte, MaxBody+1000)
	for i := range big {
		big[i] = 'a'
	}
	got := Normalize(big, "")
	if len(got) > MaxBody {
		t.Fatalf("expected normalized output capped near MaxBody, got len %d", len(got))
	}
}

func TestShinglesTwoWords(t *testing.T) {
	s := Shingles("the quick brown fox")
	want := []string{"the quick", "quick brown", "brown fox"}
	if len(s) != len(want) {
		t.Fatalf("got %v, want %v", s, want)
	}
	for i := range want {
		if s[i] != want[i] {
			t.Fatalf("got %v, want %v", s, want)
		}
	}
}

func TestShinglesSingleWord(t *testing.T) {
	s := Shingles("solo")
	if len(s) != 1 || s[0] != "solo" {
		t.Fatalf("got %v", s)
	}
}

func TestWordCount(t *testing.T) {
	if n := WordCount("the quick brown fox"); n != 4 {
		t.Fatalf("got %d, want 4", n)
	}
}
