package daemon

import (
	"fmt"
	"net/http"

	"golang.org/x/net/websocket"
)

// wsTransport adapts a *websocket.Conn to the Transport interface hub.go
// depends on, so hub_internal_test.go can substitute a fake without a real
// network connection.
type wsTransport struct{ conn *websocket.Conn }

func (t *wsTransport) WriteMessage(b []byte) error {
	t.conn.PayloadType = websocket.TextFrame
	_, err := t.conn.Write(b)
	return err
}
func (t *wsTransport) Close() error { return t.conn.Close() }

// wsHandshake is the WS upgrade's security gate (spec §5): both checks run
// BEFORE the 101 Switching Protocols response, so a rejected handshake
// never even reaches Client/Hub. Origin is the DNS-rebinding defense;
// the token arrives via Sec-WebSocket-Protocol (already parsed into
// config.Protocol by the time this runs) because a browser's WebSocket
// constructor can't set an Authorization header on the handshake request —
// this is the "first message or Sec-WebSocket-Protocol" alternative spec §5
// calls for, and Sec-WebSocket-Protocol is what smartbuster uses: the
// browser client passes the token as the second argument to `new
// WebSocket(url, [token])`.
func (sec *Security) wsHandshake(config *websocket.Config, r *http.Request) error {
	if !sec.checkOrigin(r) {
		return fmt.Errorf("origin not allowed")
	}
	if !sec.checkWSProtocolToken(config.Protocol) {
		return fmt.Errorf("missing or invalid token")
	}
	return nil
}

// EventsHandler upgrades to WS and streams hub's events to the new client
// (spec §4: GET /api/scans/{id}/events). The protocol is server->client
// only; any bytes read from the client are discarded, just to notice a
// dead connection via a Read error promptly rather than only on the next
// failed Write.
func (h *Hub) EventsHandler(sec *Security) http.Handler {
	return websocket.Server{
		Handshake: sec.wsHandshake,
		Handler: func(conn *websocket.Conn) {
			cl := NewClient(h, &wsTransport{conn: conn})
			h.Register(cl)
			defer h.Unregister(cl)

			buf := make([]byte, 512)
			for {
				if _, err := conn.Read(buf); err != nil {
					return
				}
			}
		},
	}
}
