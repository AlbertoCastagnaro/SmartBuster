package daemon

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
)

// blockingTransport simulates a WS client whose TCP write buffer is
// permanently full: WriteMessage never returns (until unblocked by the
// test), never errors. Its sendLoop necessarily pops exactly one message
// off its buffers and blocks forever trying to write it — every test below
// that inspects a blockingTransport client's buffered state accounts for
// that one in-flight item.
type blockingTransport struct {
	unblock chan struct{}
}

func newBlockingTransport() *blockingTransport {
	return &blockingTransport{unblock: make(chan struct{})}
}

func (t *blockingTransport) WriteMessage(b []byte) error {
	<-t.unblock
	return nil
}

func (t *blockingTransport) Close() error { return nil }

// countingTransport is a normal, always-fast client: it counts frames and
// keeps every one written (not just the latest), since a replace-latest
// burst can coalesce several distinct sent events into one drain cycle —
// a synchronization marker sent last can still be written before, not
// after, some other frame from that same cycle, so tests need the full
// history, not just the most recent write.
type countingTransport struct {
	mu       sync.Mutex
	count    int
	messages [][]byte
}

func (t *countingTransport) WriteMessage(b []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.count++
	t.messages = append(t.messages, append([]byte(nil), b...))
	return nil
}
func (t *countingTransport) Close() error { return nil }
func (t *countingTransport) get() (int, [][]byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.count, append([][]byte(nil), t.messages...)
}

// waitForMarker blocks until fastTr has written a frame containing marker
// at some point. Because the hub's fan-out loop (Hub.Run) processes hub.In
// strictly in order and, for each event, calls enqueue on every registered
// client before moving to the next event, a fast client observing the
// marker proves every client — including a stalled sibling — has already
// had enqueue called for every event sent before the marker too.
func waitForMarker(t *testing.T, fastTr *countingTransport, marker []byte) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, msgs := fastTr.get(); containsMarker(msgs, marker) {
			return
		}
		if time.Now().After(deadline) {
			n, _ := fastTr.get()
			t.Fatalf("marker %q never observed by the fast client — hub fan-out did not keep up (count=%d)", marker, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func containsMarker(msgs [][]byte, marker []byte) bool {
	for _, m := range msgs {
		if bytes.Contains(m, marker) {
			return true
		}
	}
	return false
}

// waitForCount blocks until fastTr has written exactly want frames. Unlike
// waitForMarker, this is needed whenever the marker event's own lane
// (important, always highest priority) can jump ahead of lower-priority
// buffered events (hits) still waiting to be drained — observing the
// marker alone doesn't prove everything queued before it has been written
// yet in that case, only that it's been enqueued.
func waitForCount(t *testing.T, fastTr *countingTransport, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if got, _ := fastTr.get(); got == want {
			return
		}
		if time.Now().After(deadline) {
			got, _ := fastTr.get()
			t.Fatalf("fast client wrote %d frames, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestHub_StalledClientNeverBlocksFanout is this phase's first load-bearing
// concurrency assertion (spec §2, §8 DoD #1): a deliberately stalled client
// — one whose Transport.WriteMessage never returns — must never slow the
// hub's own fan-out loop (every hub.In send here is bounded by a short
// per-send deadline) nor cause a second, well-behaved client to lose
// anything. Run under `go test -race`.
func TestHub_StalledClientNeverBlocksFanout(t *testing.T) {
	hub := NewHub()
	ctx, cancel := newTestCtx(t)
	defer cancel()
	go hub.Run(ctx)

	stalledTr := newBlockingTransport()
	defer close(stalledTr.unblock) // let it drain at teardown so its goroutine can exit
	stalled := NewClient(hub, stalledTr)
	hub.Register(stalled)

	fastTr := &countingTransport{}
	fast := NewClient(hub, fastTr)
	hub.Register(fast)

	const n = 2000
	for i := 0; i < n; i++ {
		ev := engine.Event{Type: engine.EventWarning, Message: fmt.Sprintf("warn-%d", i)}
		select {
		case hub.In <- ev:
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("hub.In send #%d blocked >200ms — a stalled client must never slow the fan-out loop", i)
		}
	}
	marker := []byte("the-marker")
	hub.In <- engine.Event{Type: engine.EventWarning, Message: "the-marker"}
	waitForMarker(t, fastTr, marker)

	if got, _ := fastTr.get(); got != n+1 {
		t.Errorf("fast client received %d/%d events; a stalled sibling must not cause drops for it", got, n+1)
	}

	// EventWarning is never-drop (important lane): the stalled client's
	// sendLoop popped exactly one message (the very first) before wedging
	// forever inside the blocked Write, so its buffer should hold every
	// other event — n total sent before the marker, plus the marker itself,
	// minus that one in-flight item.
	stalled.mu.Lock()
	got := len(stalled.important)
	stalled.mu.Unlock()
	if want := n + 1 - 1; got != want {
		t.Errorf("stalled client's important-lane buffer holds %d events, want %d (spec requires warning/error/trap/... to never be dropped, even for a stalled client)", got, want)
	}
}

// TestHub_ReplaceLatestForStatsAndSnapshot verifies stats/frontier.snapshot
// are replace-latest per client (spec §2): only the newest of each survives
// in a stalled client's buffer, however many were sent.
func TestHub_ReplaceLatestForStatsAndSnapshot(t *testing.T) {
	hub := NewHub()
	ctx, cancel := newTestCtx(t)
	defer cancel()
	go hub.Run(ctx)

	stalledTr := newBlockingTransport()
	defer close(stalledTr.unblock)
	stalled := NewClient(hub, stalledTr)
	hub.Register(stalled)

	fastTr := &countingTransport{}
	fast := NewClient(hub, fastTr)
	hub.Register(fast)

	const n = 50
	for i := 0; i < n; i++ {
		hub.In <- engine.Event{Type: engine.EventStats, Payload: mustMarshal(engine.StatsPayload{ReqSent: i})}
		hub.In <- engine.Event{Type: engine.EventSnapshot, Payload: mustMarshal(engine.SnapshotPayload{Total: i})}
	}
	marker := []byte("the-marker")
	hub.In <- engine.Event{Type: engine.EventWarning, Message: "the-marker"}
	waitForMarker(t, fastTr, marker)

	stalled.mu.Lock()
	defer stalled.mu.Unlock()
	if len(stalled.latest) != 2 {
		t.Fatalf("expected exactly one buffered entry per replace-latest type, got %d entries", len(stalled.latest))
	}
	stats := stalled.latest[engine.EventStats]
	var sp engine.StatsPayload
	mustDecode(t, stats.Payload, &sp)
	if sp.ReqSent != n-1 {
		t.Errorf("expected the newest stats event (ReqSent=%d) to have survived, got ReqSent=%d", n-1, sp.ReqSent)
	}
	snap := stalled.latest[engine.EventSnapshot]
	var snp engine.SnapshotPayload
	mustDecode(t, snap.Payload, &snp)
	if snp.Total != n-1 {
		t.Errorf("expected the newest snapshot event (Total=%d) to have survived, got Total=%d", n-1, snp.Total)
	}
}

// TestHub_HitFloodCoalescesForStalledClient verifies a flood of hit events
// past ClientBufCap coalesces into a running dropped-count instead of
// growing unboundedly (spec §2), for a stalled client only — the fast
// sibling registered alongside it must still receive every hit individually.
func TestHub_HitFloodCoalescesForStalledClient(t *testing.T) {
	hub := NewHub()
	ctx, cancel := newTestCtx(t)
	defer cancel()
	go hub.Run(ctx)

	stalledTr := newBlockingTransport()
	defer close(stalledTr.unblock)
	stalled := NewClient(hub, stalledTr)
	hub.Register(stalled)

	fastTr := &countingTransport{}
	fast := NewClient(hub, fastTr)
	hub.Register(fast)

	const n = ClientBufCap + 500
	for i := 0; i < n; i++ {
		hub.In <- engine.Event{Type: engine.EventHit, URL: fmt.Sprintf("/hit-%d", i)}
	}
	marker := []byte("the-marker")
	hub.In <- engine.Event{Type: engine.EventWarning, Message: "the-marker"}
	waitForMarker(t, fastTr, marker)
	// The marker (important lane) can be written before every buffered hit
	// (lower priority) has been drained — observing it only proves they
	// were all enqueued, not yet all written. Wait for the exact count too.
	waitForCount(t, fastTr, n+1)

	stalled.mu.Lock()
	defer stalled.mu.Unlock()
	if len(stalled.hits) != ClientBufCap {
		t.Errorf("stalled client's hit buffer holds %d entries, want exactly ClientBufCap(%d)", len(stalled.hits), ClientBufCap)
	}
	// One hit was popped (and is permanently in-flight inside the blocked
	// Write) before the buffer ever had a chance to fill, so the coalesced
	// count is n minus that one minus whatever's still buffered.
	wantDropped := n - 1 - len(stalled.hits)
	if stalled.hitDropped != wantDropped {
		t.Errorf("hitDropped=%d, want %d", stalled.hitDropped, wantDropped)
	}
	if stalled.hitDropped <= 0 {
		t.Error("expected the hit flood to have exceeded ClientBufCap and triggered coalescing")
	}
}
