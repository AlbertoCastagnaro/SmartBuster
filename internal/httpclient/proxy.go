// proxy.go is Phase 6b's single opt-in proxy upstream (spec §5): one string,
// passed straight to the fingerprint client at construction — no pool, no
// rotation, no ProxyProvider interface, no Tor control protocol. Real
// reputation evasion needs a residential/mobile/corporate IP the user
// already trusts (or their own Tor SOCKS listener); this just plumbs
// whatever they hand us (http/https/socks5) to tls-client, unchanged.
package httpclient

import tlsclient "github.com/bogdanfinn/tls-client"

// withProxy appends a WithProxyUrl option when proxyURL is set — empty
// stays a direct connection, and the fingerprint itself never depends on
// whether a proxy is configured (spec §5: "one browser identity per
// session," proxy only ever changes the egress IP).
func withProxy(opts []tlsclient.HttpClientOption, proxyURL string) []tlsclient.HttpClientOption {
	if proxyURL == "" {
		return opts
	}
	return append(opts, tlsclient.WithProxyUrl(proxyURL))
}
