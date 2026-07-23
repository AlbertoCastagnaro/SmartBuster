package harvest

import "regexp"

// rePathLiteral matches quoted string literals shaped like an absolute or
// relative path: a leading '/' or './'/'../', followed only by path-shaped
// characters, closed by the same kind of quote. The restricted character
// class ([a-zA-Z0-9_-./]) IS the noise filter spec §4 asks for: it
// structurally excludes template-literal interpolation ("${...}", "$(...)"
// need '$'/'{'/'('), JS regex syntax (needs '^'/'['/']'/'\'), and anything
// else outside plain path characters. A quoted mime type like
// "application/json" never matches either, since it doesn't start with '/'.
// This also naturally covers "URL arguments to fetch(...)/axios.*(...)/
// XMLHttpRequest.open(...)" (spec §4) without parsing call sites
// specifically: those arguments are themselves quoted path-shaped literals.
var rePathLiteral = regexp.MustCompile("[\"'`](/[a-zA-Z0-9_\\-./]{1,199}|\\.\\.?/[a-zA-Z0-9_\\-./]{1,199})[\"'`]")

// reExtLiteral matches bare quoted filenames with a known endpoint-ish
// extension but no leading path separator (spec §4: "must look like a path
// (leading / or known extension)").
var reExtLiteral = regexp.MustCompile("[\"'`]([a-zA-Z0-9_\\-]+\\.(?:json|php|xml|txt|env|ya?ml|conf|config|asp|aspx|jsp|action|cgi))[\"'`]")

// ExtractJSPaths mines JS (or JSON) source for endpoint-shaped string
// literals (spec §4): LinkFinder-style — quoted absolute or relative paths,
// plus bare known-extension filenames — filtered hard against noise by
// construction (see rePathLiteral). Dedup'd, order-preserving.
func ExtractJSPaths(src []byte) []string {
	text := string(src)
	seen := make(map[string]bool)
	var out []string
	collect := func(re *regexp.Regexp) {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			p := m[1]
			if seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	collect(rePathLiteral)
	collect(reExtLiteral)
	return out
}
