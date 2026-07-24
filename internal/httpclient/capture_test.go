// capture_test.go implements Phase 6b's local fidelity harness (spec §8):
// "genuinely hard to test hermetically — do it with a local capture
// server, not a live third party." captureServer terminates TLS itself
// (so it can tee the raw ClientHello before crypto/tls consumes it) and
// then speaks just enough hand-rolled HTTP/2 — connection preface, one
// SETTINGS exchange, one HEADERS frame decoded via hpack in wire order —
// to capture exactly the three fingerprint layers spec §2 bundles
// together: the TLS ClientHello, the HTTP/2 SETTINGS/pseudo-header order
// (the Akamai fingerprint), and the regular header set's wire order.
//
// Deliberately one request per connection: the test's fidelity claims are
// about "does the same BrowserProfile produce the same fingerprint across
// separate on-target requests" (spec §4), not about HTTP/2 stream
// multiplexing — reusing hpack's connection-scoped dynamic table safely
// across multiple streams would add real complexity for no fidelity
// coverage this package's tests need, so callers close the connection
// (CloseIdleConnections) between requests.
package httpclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	tlsclient "github.com/bogdanfinn/tls-client"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// clientHelloInfo is the handful of ClientHello fields JA3 hashes (spec
// §8.1): version, cipher suites, extension types, elliptic curves and
// point formats, each in wire order with GREASE codepoints stripped (RFC
// 8701 GREASE values are randomized per connection by design — real JA3
// strips them for exactly this reason, so a fingerprint stays stable
// across connections from the same profile despite that randomization).
type clientHelloInfo struct {
	version    uint16
	ciphers    []uint16
	extensions []uint16
	curves     []uint16
	points     []uint8
}

func isGreaseU16(v uint16) bool {
	hi, lo := byte(v>>8), byte(v)
	return hi == lo && lo&0x0f == 0x0a
}

// ja3Fingerprint hashes info's fields the same way real JA3 does (version,
// ciphers, extensions, curves, point-formats, dash-joined then
// comma-joined, MD5 hex) — a simplified, non-registry-certified fingerprint
// (this harness only needs it to be stable per profile and to differ from
// a different profile/client, not to match a public JA3 database).
func (info clientHelloInfo) fingerprint() string {
	var ciphers, exts, curves, points []string
	for _, c := range info.ciphers {
		if !isGreaseU16(c) {
			ciphers = append(ciphers, fmt.Sprintf("%d", c))
		}
	}
	for _, e := range info.extensions {
		if !isGreaseU16(e) {
			exts = append(exts, fmt.Sprintf("%d", e))
		}
	}
	for _, c := range info.curves {
		if !isGreaseU16(c) {
			curves = append(curves, fmt.Sprintf("%d", c))
		}
	}
	for _, p := range info.points {
		points = append(points, fmt.Sprintf("%d", p))
	}
	raw := fmt.Sprintf("%d,%s,%s,%s,%s", info.version,
		strings.Join(ciphers, "-"), strings.Join(exts, "-"),
		strings.Join(curves, "-"), strings.Join(points, "-"))
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// parseClientHello reads the first TLS record off raw (a handshake record
// carrying a ClientHello) and extracts exactly the fields ja3Fingerprint
// needs, straight from the wire bytes — no crypto/tls involved, since by
// the time crypto/tls hands back a ClientHelloInfo it's already dropped
// the raw extension/cipher order this fidelity check needs.
func parseClientHello(raw []byte) (clientHelloInfo, error) {
	var info clientHelloInfo
	if len(raw) < 5 || raw[0] != 0x16 {
		return info, fmt.Errorf("not a TLS handshake record")
	}
	recLen := int(binary.BigEndian.Uint16(raw[3:5]))
	if len(raw) < 5+recLen {
		return info, fmt.Errorf("truncated record")
	}
	body := raw[5 : 5+recLen]
	if len(body) < 4 || body[0] != 0x01 {
		return info, fmt.Errorf("not a ClientHello")
	}
	hsLen := int(body[1])<<16 | int(body[2])<<8 | int(body[3])
	if len(body) < 4+hsLen {
		return info, fmt.Errorf("truncated handshake body")
	}
	ch := body[4 : 4+hsLen]

	p := 0
	if len(ch) < p+2+32+1 {
		return info, fmt.Errorf("short ClientHello")
	}
	info.version = binary.BigEndian.Uint16(ch[p:])
	p += 2 + 32 // client_version + random
	sessLen := int(ch[p])
	p += 1 + sessLen

	if len(ch) < p+2 {
		return info, fmt.Errorf("short cipher suites")
	}
	cipherLen := int(binary.BigEndian.Uint16(ch[p:]))
	p += 2
	for i := 0; i+1 < cipherLen && p+i+1 < len(ch); i += 2 {
		info.ciphers = append(info.ciphers, binary.BigEndian.Uint16(ch[p+i:]))
	}
	p += cipherLen

	if len(ch) < p+1 {
		return info, fmt.Errorf("short compression methods")
	}
	compLen := int(ch[p])
	p += 1 + compLen

	if len(ch) < p+2 {
		return info, nil // no extensions block; nothing more to parse
	}
	extTotal := int(binary.BigEndian.Uint16(ch[p:]))
	p += 2
	end := p + extTotal
	if end > len(ch) {
		end = len(ch)
	}
	for p+4 <= end {
		extType := binary.BigEndian.Uint16(ch[p:])
		extLen := int(binary.BigEndian.Uint16(ch[p+2:]))
		p += 4
		info.extensions = append(info.extensions, extType)
		if p+extLen > len(ch) {
			break
		}
		data := ch[p : p+extLen]
		switch extType {
		case 10: // supported_groups (elliptic curves)
			if len(data) >= 2 {
				listLen := int(binary.BigEndian.Uint16(data))
				for i := 2; i+1 < 2+listLen && i+1 < len(data); i += 2 {
					info.curves = append(info.curves, binary.BigEndian.Uint16(data[i:]))
				}
			}
		case 11: // ec_point_formats
			if len(data) >= 1 {
				listLen := int(data[0])
				for i := 1; i < 1+listLen && i < len(data); i++ {
					info.points = append(info.points, data[i])
				}
			}
		}
		p += extLen
	}
	return info, nil
}

// capture is one connection's fully decoded fingerprint (spec §2's three
// bundled layers, minus the values-only header map plain net/http already
// covered in 6a).
type capture struct {
	hello        clientHelloInfo
	settingsIDs  []http2.SettingID
	settings     map[http2.SettingID]uint32
	pseudoOrder  []string // ":method" etc, in wire order
	headerOrder  []string // regular (non-pseudo) header names, lowercased, in wire order
	headerValues map[string]string
	err          error
}

// captureServer is the spec §8 local capture harness: a raw TLS+HTTP/2
// listener good enough to decode a request's fingerprint, not a real web
// server (no routing, no persistent keep-alive multiplexing — see the
// package doc comment above).
type captureServer struct {
	ln   net.Listener
	cert tls.Certificate
	addr string

	mu       sync.Mutex
	captures []capture
	notify   chan struct{}
}

func startCaptureServer(t *testing.T) *captureServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &captureServer{ln: ln, cert: generateSelfSignedCert(t), addr: ln.Addr().String(), notify: make(chan struct{}, 64)}
	go s.acceptLoop()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *captureServer) baseURL() string { return "https://" + s.addr }

func (s *captureServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

// captureConn tees every byte read off the raw connection into buf before
// crypto/tls ever sees it — the ClientHello is the first thing a client
// sends, unencrypted, so parseClientHello only ever needs buf's first TLS
// record (spec §8.1).
type captureConn struct {
	net.Conn
	mu  sync.Mutex
	buf []byte
}

func (c *captureConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.mu.Lock()
		c.buf = append(c.buf, p[:n]...)
		c.mu.Unlock()
	}
	return n, err
}

func (c *captureConn) snapshot() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf...)
}

func (s *captureServer) handleConn(raw net.Conn) {
	defer raw.Close()
	cc := &captureConn{Conn: raw}
	tlsConn := tls.Server(cc, &tls.Config{
		Certificates: []tls.Certificate{s.cert},
		NextProtos:   []string{"h2"},
	})
	// Handshake errors (e.g. a peer that doesn't trust our self-signed
	// cert) are checked *after* parsing: the ClientHello is the first
	// flight a client sends, already captured in cc's buffer well before
	// any later handshake failure, so a fingerprint is still recoverable
	// even from a connection that never finishes negotiating.
	handshakeErr := tlsConn.Handshake()
	hello, parseErr := parseClientHello(cc.snapshot())
	if handshakeErr != nil {
		s.push(capture{hello: hello, err: fmt.Errorf("handshake: %w", handshakeErr)})
		return
	}
	if parseErr != nil {
		s.push(capture{err: fmt.Errorf("parse clienthello: %w", parseErr)})
		return
	}

	tlsConn.SetDeadline(time.Now().Add(5 * time.Second))
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(tlsConn, preface); err != nil {
		s.push(capture{hello: hello, err: fmt.Errorf("preface: %w", err)})
		return
	}

	framer := http2.NewFramer(tlsConn, tlsConn)
	if err := framer.WriteSettings(); err != nil {
		s.push(capture{hello: hello, err: fmt.Errorf("write settings: %w", err)})
		return
	}

	cap := capture{hello: hello, settings: map[http2.SettingID]uint32{}, headerValues: map[string]string{}}
	var streamID uint32
	var fields []hpack.HeaderField
	dec := hpack.NewDecoder(4096, func(f hpack.HeaderField) { fields = append(fields, f) })

	for {
		fr, err := framer.ReadFrame()
		if err != nil {
			s.push(capture{hello: hello, err: fmt.Errorf("read frame: %w", err)})
			return
		}
		switch f := fr.(type) {
		case *http2.SettingsFrame:
			if f.IsAck() {
				continue
			}
			f.ForeachSetting(func(setting http2.Setting) error {
				cap.settingsIDs = append(cap.settingsIDs, setting.ID)
				cap.settings[setting.ID] = setting.Val
				return nil
			})
			if err := framer.WriteSettingsAck(); err != nil {
				s.push(capture{hello: hello, err: fmt.Errorf("ack settings: %w", err)})
				return
			}
		case *http2.HeadersFrame:
			streamID = f.StreamID
			if _, err := dec.Write(f.HeaderBlockFragment()); err != nil {
				s.push(capture{hello: hello, err: fmt.Errorf("hpack: %w", err)})
				return
			}
			if f.HeadersEnded() {
				goto decoded
			}
		case *http2.WindowUpdateFrame, *http2.PingFrame:
			// not relevant to fingerprint fidelity; ignore.
		default:
			// ignore anything else (PRIORITY, etc.)
		}
	}

decoded:
	for _, f := range fields {
		name := strings.ToLower(f.Name)
		if strings.HasPrefix(name, ":") {
			cap.pseudoOrder = append(cap.pseudoOrder, name)
			continue
		}
		cap.headerOrder = append(cap.headerOrder, name)
		cap.headerValues[name] = f.Value
	}

	respBlock := hpackEncodeStatus200()
	_ = framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: respBlock,
		EndHeaders:    true,
		EndStream:     true,
	})
	s.push(cap)
}

func hpackEncodeStatus200() []byte {
	var out []byte
	enc := hpack.NewEncoder(&sliceWriter{&out})
	enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
	return out
}

type sliceWriter struct{ buf *[]byte }

func (w *sliceWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}

func (s *captureServer) push(c capture) {
	s.mu.Lock()
	s.captures = append(s.captures, c)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// waitFor blocks until at least n captures have landed, or fails the test
// after a bounded timeout.
func (s *captureServer) waitFor(t *testing.T, n int) []capture {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		got := len(s.captures)
		s.mu.Unlock()
		if got >= n {
			break
		}
		select {
		case <-s.notify:
		case <-time.After(50 * time.Millisecond):
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.captures) < n {
		t.Fatalf("expected %d capture(s), got %d", n, len(s.captures))
	}
	return append([]capture(nil), s.captures...)
}

func generateSelfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// newTestTLSDoer builds a TLSDoer against profileName with the same
// ClientProfile/redirect behavior NewTLSDoer uses, plus InsecureSkipVerify
// (the capture server's cert is self-signed — production NewTLSDoer never
// skips verification; this is test-only scaffolding in the same package).
func newTestTLSDoer(t *testing.T, profileName string, extraOpts ...tlsclient.HttpClientOption) *TLSDoer {
	t.Helper()
	profile, ok := BrowserProfileFor(profileName)
	if !ok {
		t.Fatalf("unknown profile %q", profileName)
	}
	opts := append([]tlsclient.HttpClientOption{
		tlsclient.WithClientProfile(profile.TLSClient),
		tlsclient.WithNotFollowRedirects(),
		tlsclient.WithInsecureSkipVerify(),
		tlsclient.WithTimeoutSeconds(5),
	}, extraOpts...)
	c, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), opts...)
	if err != nil {
		t.Fatalf("new tls client: %v", err)
	}
	return &TLSDoer{client: c, profile: profile}
}

// TestTLSDoer_JA3DiffersFromNetHTTP is spec §8.1: the fingerprint client's
// ClientHello must not be Go's stock net/http one.
func TestTLSDoer_JA3DiffersFromNetHTTP(t *testing.T) {
	srv := startCaptureServer(t)

	doer := newTestTLSDoer(t, ProfileChrome)
	if _, err := doer.Do(t.Context(), Request{URL: srv.baseURL() + "/"}); err != nil {
		t.Fatalf("tls doer request: %v", err)
	}

	plain := New(Config{})
	// A failed request here (untrusted self-signed cert, etc.) is fine —
	// the ClientHello (the very first bytes on the wire) is already
	// captured well before any of that would matter.
	_, _ = plain.Do(t.Context(), Request{URL: srv.baseURL() + "/"})

	caps := srv.waitFor(t, 2)
	if caps[0].err != nil && caps[1].err != nil {
		t.Fatalf("both captures failed: %v / %v", caps[0].err, caps[1].err)
	}
	if caps[0].hello.version == 0 || caps[1].hello.version == 0 {
		t.Fatalf("expected both ClientHellos to actually parse, got %+v / %+v", caps[0].hello, caps[1].hello)
	}
	fp0, fp1 := caps[0].hello.fingerprint(), caps[1].hello.fingerprint()
	if fp0 == fp1 {
		t.Fatalf("expected the tls-client and stock net/http ClientHellos to fingerprint differently, both got %s", fp0)
	}
}

// TestTLSDoer_HTTP2SettingsAndPseudoHeaderOrderMatchProfile is spec §8.2:
// the SETTINGS frame (values and order) and pseudo-header order (the
// Akamai fingerprint) must match the active BrowserProfile's own bundled
// ClientProfile — asserted against profiles.ClientProfile's own exported
// Get* accessors, the authoritative source for what tls-client claims it
// will send, rather than a guessed expected value.
func TestTLSDoer_HTTP2SettingsAndPseudoHeaderOrderMatchProfile(t *testing.T) {
	srv := startCaptureServer(t)
	doer := newTestTLSDoer(t, ProfileChrome)
	if _, err := doer.Do(t.Context(), Request{URL: srv.baseURL() + "/"}); err != nil {
		t.Fatalf("tls doer request: %v", err)
	}
	caps := srv.waitFor(t, 1)
	cap := caps[0]
	if cap.err != nil {
		t.Fatalf("capture failed: %v", cap.err)
	}

	// tls-client's profiles package types its SettingIDs against its own
	// fhttp/http2 fork; the capture server decodes wire bytes with the
	// stdlib-adjacent golang.org/x/net/http2 — two distinct Go types over
	// the same uint16 protocol codepoints, so comparisons go through that
	// shared underlying type.
	wantOrder := doer.profile.TLSClient.GetSettingsOrder()
	if len(cap.settingsIDs) != len(wantOrder) {
		t.Fatalf("expected %d settings, got %d (%v)", len(wantOrder), len(cap.settingsIDs), cap.settingsIDs)
	}
	for i, id := range wantOrder {
		if uint16(cap.settingsIDs[i]) != uint16(id) {
			t.Fatalf("settings order mismatch at %d: want %v got %v (full: want %v got %v)", i, id, cap.settingsIDs[i], wantOrder, cap.settingsIDs)
		}
	}
	wantSettings := doer.profile.TLSClient.GetSettings()
	for id, want := range wantSettings {
		if got := cap.settings[http2.SettingID(uint16(id))]; got != want {
			t.Errorf("setting %v: want %d got %d", id, want, got)
		}
	}

	wantPseudo := doer.profile.TLSClient.GetPseudoHeaderOrder()
	if len(cap.pseudoOrder) != len(wantPseudo) {
		t.Fatalf("expected pseudo-header order %v, got %v", wantPseudo, cap.pseudoOrder)
	}
	for i, name := range wantPseudo {
		if cap.pseudoOrder[i] != name {
			t.Fatalf("pseudo-header order mismatch at %d: want %v got %v", i, wantPseudo, cap.pseudoOrder)
		}
	}
}

// TestTLSDoer_HeaderOrderMatchesProfile is spec §8.3 (contract D): the
// regular (non-pseudo) header set must reach the wire in exactly the
// active BrowserProfile's declared order, with Referer spliced in at its
// configured position when the caller's request carries one.
func TestTLSDoer_HeaderOrderMatchesProfile(t *testing.T) {
	for _, profileName := range []string{ProfileChrome, ProfileFirefox, ProfileSafari} {
		t.Run(profileName, func(t *testing.T) {
			srv := startCaptureServer(t)
			doer := newTestTLSDoer(t, profileName)
			profile, _ := BrowserProfileFor(profileName)

			referer := "https://example.invalid/parent/"
			reqHeaders := BuildHeaders("", referer) // only Referer matters to TLSDoer.Do (see orderedHeader)
			if _, err := doer.Do(t.Context(), Request{URL: srv.baseURL() + "/", Headers: reqHeaders}); err != nil {
				t.Fatalf("tls doer request: %v", err)
			}
			caps := srv.waitFor(t, 1)
			cap := caps[0]
			if cap.err != nil {
				t.Fatalf("capture failed: %v", cap.err)
			}

			var wantOrder []string
			inserted := false
			for _, kv := range profile.Headers {
				wantOrder = append(wantOrder, kv.Key)
				if kv.Key == profile.refererAfter {
					wantOrder = append(wantOrder, "referer")
					inserted = true
				}
			}
			if !inserted {
				wantOrder = append(wantOrder, "referer")
			}

			if len(cap.headerOrder) != len(wantOrder) {
				t.Fatalf("expected header order %v, got %v", wantOrder, cap.headerOrder)
			}
			for i, name := range wantOrder {
				if cap.headerOrder[i] != name {
					t.Fatalf("header order mismatch at %d: want %v got %v", i, wantOrder, cap.headerOrder)
				}
			}
			if cap.headerValues["referer"] != referer {
				t.Errorf("expected referer value %q, got %q", referer, cap.headerValues["referer"])
			}
			for _, kv := range profile.Headers {
				if got := cap.headerValues[kv.Key]; got != kv.Value {
					t.Errorf("header %q: want %q got %q", kv.Key, kv.Value, got)
				}
			}
		})
	}
}

// TestTLSDoer_ConsistentAcrossSeparateRequests is spec §8.4 (contract C):
// every on-target request — modeled here as three independent connections
// through the same TLSDoer, standing in for a candidate request, a
// profiling fetch, and a harvest fetch — must present an identical
// fingerprint, since a fingerprinting WAF sees them all hit the same host.
func TestTLSDoer_ConsistentAcrossSeparateRequests(t *testing.T) {
	srv := startCaptureServer(t)
	doer := newTestTLSDoer(t, ProfileChrome)

	for i := 0; i < 3; i++ {
		if _, err := doer.Do(t.Context(), Request{URL: srv.baseURL() + "/"}); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		doer.client.CloseIdleConnections() // force the next Do() onto a fresh connection
	}

	caps := srv.waitFor(t, 3)
	var fps []string
	for i, c := range caps {
		if c.err != nil {
			t.Fatalf("capture %d failed: %v", i, c.err)
		}
		fps = append(fps, c.hello.fingerprint())
	}
	for i := 1; i < len(fps); i++ {
		if fps[i] != fps[0] {
			t.Fatalf("expected identical fingerprints across separate connections, got %v", fps)
		}
	}
	// Same for header order/settings — every capture should agree with the
	// very first, byte for byte, on every layer that matters.
	for i := 1; i < len(caps); i++ {
		if len(caps[i].headerOrder) != len(caps[0].headerOrder) {
			t.Fatalf("capture %d header order %v diverged from capture 0 %v", i, caps[i].headerOrder, caps[0].headerOrder)
		}
		for j := range caps[0].headerOrder {
			if caps[i].headerOrder[j] != caps[0].headerOrder[j] {
				t.Fatalf("capture %d header order %v diverged from capture 0 %v", i, caps[i].headerOrder, caps[0].headerOrder)
			}
		}
	}
}

// TestTLSDoer_ProxyRoutesThroughUpstream is spec §8.5: --proxy routes every
// request through the configured upstream; the fingerprint itself doesn't
// depend on whether a proxy is set (spec §5). A minimal CONNECT-only stub
// stands in for a real forward proxy — it only needs to observe the CONNECT
// request, not actually tunnel traffic, to prove the client engaged it.
func TestTLSDoer_ProxyRoutesThroughUpstream(t *testing.T) {
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer proxyLn.Close()

	type connectSeen struct {
		target string
		err    error
	}
	seenCh := make(chan connectSeen, 1)
	go func() {
		conn, err := proxyLn.Accept()
		if err != nil {
			seenCh <- connectSeen{err: err}
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			seenCh <- connectSeen{err: err}
			return
		}
		line := string(buf[:n])
		var target string
		fmt.Sscanf(line, "CONNECT %s", &target)
		seenCh <- connectSeen{target: target}
		// Deliberately not completing the tunnel — this stub only proves
		// the proxy was engaged, not that the whole request succeeds.
	}()

	doer, err := NewTLSDoer(Config{}, ProfileChrome, "http://"+proxyLn.Addr().String())
	if err != nil {
		t.Fatalf("new tls doer: %v", err)
	}
	if got := doer.client.GetProxy(); got != "http://"+proxyLn.Addr().String() {
		t.Fatalf("expected GetProxy to report the configured upstream, got %q", got)
	}

	// The tunnel is never completed, so this request is expected to fail —
	// only the proxy stub's observation matters.
	_, _ = doer.Do(t.Context(), Request{URL: "https://example.invalid/"})

	select {
	case seen := <-seenCh:
		if seen.err != nil {
			t.Fatalf("proxy stub error: %v", seen.err)
		}
		if seen.target != "example.invalid:443" {
			t.Fatalf("expected CONNECT to example.invalid:443, got %q", seen.target)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the proxy to observe a CONNECT request")
	}
}

// TestNewTLSDoer_UnknownProfileFallsBackToChrome documents NewTLSDoer's
// fallback (spec §2: BrowserProfileFor's ok=false case): an unrecognized
// name still yields a usable, coherent bundle rather than an error.
func TestNewTLSDoer_UnknownProfileFallsBackToChrome(t *testing.T) {
	doer, err := NewTLSDoer(Config{}, "not-a-real-profile", "")
	if err != nil {
		t.Fatalf("new tls doer: %v", err)
	}
	if doer.profile.Name != ProfileChrome {
		t.Fatalf("expected an unknown profile name to fall back to chrome, got %q", doer.profile.Name)
	}
}
