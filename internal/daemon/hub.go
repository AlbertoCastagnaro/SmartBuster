// Package daemon implements smartbuster's Phase 5a control plane: the WS
// event hub, the REST control plane, server security, and session
// save/resume, all sitting in front of a single internal/engine.Coordinator
// (spec smartbuster-phase5a-spec.md).
//
// hub.go is the first of this phase's two load-bearing concurrency
// primitives (spec §2): a non-blocking fan-out from the coordinator's
// single emit call to N independently-paced WS clients.
//
//	coordinator --emit(Event)--> hubIn (buffered chan) --> hub goroutine --> per-client send goroutines
//
// The coordinator's daemonEmitter.Emit does a non-blocking send to hubIn —
// if it's full, the event is dropped, never blocked on, because blocking
// the emit call would block the coordinator and stall the scan itself. Each
// connected client then gets its own bounded, independently-paced buffer: a
// client that falls behind only ever loses its own data (stats/snapshot
// replace-latest, hits coalesce into a count), never another client's, and
// never touches the coordinator's throughput.
package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
)

const (
	// HubInCap is the coordinator->hub buffered channel's capacity (spec §7).
	HubInCap = 1024
	// ClientBufCap bounds each client's hit-coalescing buffer (spec §7):
	// the newest ClientBufCap hits are kept verbatim: older ones already
	// waiting are coalesced into a running dropped-count.
	ClientBufCap = 256
)

// EventHitCoalesced is a hub-synthesized event type, never emitted by the
// coordinator itself: when one slow client's hit buffer overflows, the hub
// folds the oldest excess hits into a single running count (spec §2:
// "coalesced into a count + the newest few") and delivers this instead, only
// to that client, once its buffer next has room. Category mirrors "hit"
// (CategoryDiscovery) since it stands in for discovery events that would
// otherwise have been delivered individually.
const EventHitCoalesced engine.EventType = "hit.coalesced"

// HitCoalescedPayload is EventHitCoalesced's payload: how many hit events
// this client didn't receive individually because its buffer was full.
type HitCoalescedPayload struct {
	Count int `json:"count"`
}

// Transport is the minimal interface a Client writes frames to. The real
// server wraps a *websocket.Conn (see ws.go); tests substitute a fake to
// simulate a stalled client without a real network connection.
type Transport interface {
	WriteMessage([]byte) error
	Close() error
}

// Hub owns the client registry and fans out every event it receives on In
// to every registered client. Run must be started exactly once; register/
// unregister/fan-out all happen on Run's own goroutine, so the clients map
// needs no lock (same single-writer discipline the coordinator's own
// channels use).
type Hub struct {
	In         chan engine.Event
	register   chan *Client
	unregister chan *Client
	clients    map[*Client]bool

	// Dropped counts events discarded because In was full when Emit tried
	// to send (diagnostic only — the coordinator never blocks on this
	// either way). ClientsDropped counts individual per-client drops
	// (coalesced hits + any other lossy-lane discard).
	Dropped        atomic.Int64
	ClientsDropped atomic.Int64
}

func NewHub() *Hub {
	return &Hub{
		In:         make(chan engine.Event, HubInCap),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		clients:    make(map[*Client]bool),
	}
}

// Run is the hub's single goroutine: fan-out and (un)registration all
// happen here, so neither needs a lock. Returns when ctx is cancelled,
// closing every still-registered client first.
func (h *Hub) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			for cl := range h.clients {
				delete(h.clients, cl)
				cl.shutdown()
			}
			return
		case cl := <-h.register:
			h.clients[cl] = true
		case cl := <-h.unregister:
			if h.clients[cl] {
				delete(h.clients, cl)
				cl.shutdown()
			}
		case ev := <-h.In:
			for cl := range h.clients {
				if !cl.enqueue(ev) {
					h.ClientsDropped.Add(1)
				}
			}
		}
	}
}

// Register adds cl to the hub and starts its send loop. Safe to call
// concurrently with Run (register is itself a channel hand-off to Run's
// goroutine).
func (h *Hub) Register(cl *Client) {
	go cl.sendLoop()
	h.register <- cl
}

// Unregister removes cl from the hub and stops its send loop.
func (h *Hub) Unregister(cl *Client) {
	h.unregister <- cl
}

// NewEmitter returns an engine.EventEmitter whose sink is this hub (spec §2
// contract B's daemonEmitter): the coordinator's single caller of Emit does
// a non-blocking send to hubIn and moves on — a full buffer means the event
// is dropped, never a coordinator stall.
func (h *Hub) NewEmitter() engine.EventEmitter {
	return engine.EventFunc(func(ev engine.Event) {
		select {
		case h.In <- ev:
		default:
			h.Dropped.Add(1)
		}
	})
}

// lane classifies an event into one of the hub's three per-client dropping
// policies (spec §2).
type lane int

const (
	laneImportant lane = iota // warning/error/trap/branch.pruned/spa.pivot/lifecycle: never dropped
	laneLatest                // stats/frontier.snapshot: replace-latest
	laneHit                   // hit: coalesced into a count + the newest few under flood
)

func classify(ev engine.Event) lane {
	switch ev.Type {
	case engine.EventStats, engine.EventSnapshot:
		return laneLatest
	case engine.EventHit:
		return laneHit
	default:
		return laneImportant
	}
}

// Client is one connected WS client's outbound buffering state. The hub
// goroutine calls enqueue (must never block); a dedicated per-client
// goroutine (sendLoop) drains the buffers and writes to tr, so one slow
// client's blocked/slow Write only ever stalls its own goroutine.
type Client struct {
	hub *Hub
	tr  Transport

	mu         sync.Mutex
	important  []engine.Event
	latest     map[engine.EventType]engine.Event
	hits       []engine.Event
	hitDropped int
	closed     bool

	wake    chan struct{} // cap 1: "there's something to send"
	closeCh chan struct{}
}

func NewClient(hub *Hub, tr Transport) *Client {
	return &Client{
		hub:     hub,
		tr:      tr,
		latest:  make(map[engine.EventType]engine.Event),
		wake:    make(chan struct{}, 1),
		closeCh: make(chan struct{}),
	}
}

// enqueue is the hub goroutine's only interaction with a Client: an O(1)
// mutex-guarded buffer op, so a stalled client's Write never backs up the
// hub's own fan-out loop. Returns false if this particular event was
// dropped for this client (used only for the ClientsDropped diagnostic).
func (cl *Client) enqueue(ev engine.Event) bool {
	cl.mu.Lock()
	dropped := false
	if cl.closed {
		cl.mu.Unlock()
		return false
	}
	switch classify(ev) {
	case laneLatest:
		cl.latest[ev.Type] = ev
	case laneHit:
		if len(cl.hits) >= ClientBufCap {
			cl.hits = cl.hits[1:]
			cl.hitDropped++
			dropped = true
		}
		cl.hits = append(cl.hits, ev)
	default:
		cl.important = append(cl.important, ev)
	}
	cl.mu.Unlock()

	select {
	case cl.wake <- struct{}{}:
	default:
	}
	return !dropped
}

// next pops the single highest-priority pending message: important
// (FIFO) first since these are low-volume and must never be reordered
// behind stale telemetry, then latest stats/snapshot, then a coalesced-hit
// marker if any hits were dropped, then buffered hits.
func (cl *Client) next() ([]byte, bool) {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	if len(cl.important) > 0 {
		ev := cl.important[0]
		cl.important = cl.important[1:]
		return marshalEvent(ev)
	}
	for typ, ev := range cl.latest {
		delete(cl.latest, typ)
		return marshalEvent(ev)
	}
	if cl.hitDropped > 0 {
		n := cl.hitDropped
		cl.hitDropped = 0
		return marshalEvent(engine.Event{
			Type: EventHitCoalesced, Category: engine.CategoryDiscovery,
			Payload: mustMarshal(HitCoalescedPayload{Count: n}),
		})
	}
	if len(cl.hits) > 0 {
		ev := cl.hits[0]
		cl.hits = cl.hits[1:]
		return marshalEvent(ev)
	}
	return nil, false
}

// sendLoop is the one goroutine allowed to call tr.WriteMessage: started by
// Hub.Register, stopped by shutdown. A slow/blocked Write only ever stalls
// this goroutine — enqueue (called from the hub's own goroutine) never
// waits on it.
func (cl *Client) sendLoop() {
	for {
		select {
		case <-cl.closeCh:
			return
		case <-cl.wake:
			for {
				msg, ok := cl.next()
				if !ok {
					break
				}
				if err := cl.tr.WriteMessage(msg); err != nil {
					cl.hub.Unregister(cl)
					return
				}
			}
		}
	}
}

// shutdown stops sendLoop and closes the transport; called only from the
// hub's own goroutine (Run's ctx.Done/unregister branches), so it never
// races a concurrent enqueue's read of cl.closed.
func (cl *Client) shutdown() {
	cl.mu.Lock()
	if cl.closed {
		cl.mu.Unlock()
		return
	}
	cl.closed = true
	cl.mu.Unlock()
	close(cl.closeCh)
	cl.tr.Close()
}

func marshalEvent(ev engine.Event) ([]byte, bool) {
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, false
	}
	return b, true
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
