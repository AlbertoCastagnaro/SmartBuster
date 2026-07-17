package calibration

import (
	"regexp"
	"strings"
)

// MaxBody is the body-read cap applied before normalization (spec §7 step 1).
const MaxBody = 256 * 1024 // 256 KiB

// minStrippableTokenLen is the shortest requested-path token normalization
// will strip from a body. It backstops the \b-delimited match in Normalize
// against extremely common short standalone words (e.g. "a", "is") that
// word-boundary matching alone wouldn't filter out. Eval-tunable.
const minStrippableTokenLen = 3

// normPlaceholder replaces every volatile substring stripped during
// normalization, so two bodies that differ only in nonces/timestamps/ids
// normalize to identical text.
const normPlaceholder = "~norm~"

var (
	reCSRF       = regexp.MustCompile(`(?i)(csrf|nonce|token|_token|authenticity_token)["'=:\s]+[a-z0-9\-_]+`)
	reHexBlob    = regexp.MustCompile(`[a-f0-9]{16,}`)
	reB64Blob    = regexp.MustCompile(`[A-Za-z0-9+/]{24,}={0,2}`)
	reUUID       = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	reISOTime    = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[t\s][\d:.\-+z]+`)
	reEpoch      = regexp.MustCompile(`\b\d{10,13}\b`)
	reWhitespace = regexp.MustCompile(`\s+`)
)

// Normalize runs the body normalization pipeline (spec §7): cap, lowercase,
// strip the reflected request token, strip volatile substrings, collapse
// whitespace. requestedToken is the path segment that was requested (e.g.
// "admin" or the random probe token); passing "" is a harmless no-op for
// the reflected-path step.
func Normalize(body []byte, requestedToken string) string {
	if len(body) > MaxBody {
		body = body[:MaxBody]
	}
	text := strings.ToLower(string(body))

	// Strip the token as a delimited word/path segment, not a raw substring:
	// a raw strings.ReplaceAll of "test" would also eat the "test" inside
	// "latest" or "testing", corrupting unrelated content. \b anchors on the
	// transition between a word char ([0-9A-Za-z_]) and a non-word char, and
	// RE2 treats "/", quotes, and whitespace as non-word — exactly the
	// characters that actually delimit a reflected path segment or attribute
	// value — so "/test" or " test " match but "latest"/"testing" don't.
	// The length guard is a second, independent safeguard: even boundary-
	// matched, a 1-2 char token like "a" would still strip every standalone
	// occurrence of an extremely common short word, which \b alone can't
	// prevent. Reflected-path tokens in practice are never this short.
	if len(requestedToken) >= minStrippableTokenLen {
		tokenRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(strings.ToLower(requestedToken)) + `\b`)
		text = tokenRe.ReplaceAllString(text, "")
	}

	text = reCSRF.ReplaceAllString(text, normPlaceholder)
	text = reHexBlob.ReplaceAllString(text, normPlaceholder)
	text = reB64Blob.ReplaceAllString(text, normPlaceholder)
	text = reUUID.ReplaceAllString(text, normPlaceholder)
	text = reISOTime.ReplaceAllString(text, normPlaceholder)
	text = reEpoch.ReplaceAllString(text, normPlaceholder)

	text = reWhitespace.ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}

// Shingles splits normalized text into word 2-shingles for SimHash input
// (spec §7 step 6). Duplicates are preserved intentionally: simhash.SimHash
// relies on repeated shingles to weight bit votes by feature frequency.
func Shingles(normalized string) []string {
	words := strings.Fields(normalized)
	if len(words) < 2 {
		return words
	}
	out := make([]string, 0, len(words)-1)
	for i := 0; i < len(words)-1; i++ {
		out = append(out, words[i]+" "+words[i+1])
	}
	return out
}

// WordCount returns the whitespace-delimited word count of normalized text.
func WordCount(normalized string) int {
	return len(strings.Fields(normalized))
}
