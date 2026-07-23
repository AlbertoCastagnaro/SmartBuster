// telemetry.go is dispatchLoop's Phase 5a heartbeat (spec §3): stats and
// frontier.snapshot are both computed and emitted from the coordinator
// goroutine's own select loop, so every field they read (frontier, dirs,
// inFlight, counters) is read without racing its only writer.
package engine

import "time"

func (c *Coordinator) emitStats() {
	elapsed := time.Since(c.scanStart)
	var reqPerSec float64
	if elapsed > 0 {
		reqPerSec = float64(c.statsReqSent) / elapsed.Seconds()
	}
	var hitRate float64
	if c.statsReqSent > 0 {
		hitRate = float64(c.statsHits) / float64(c.statsReqSent)
	}
	etaMs := int64(-1)
	if reqPerSec > 0 {
		etaMs = int64(float64(c.frontier.Len()) / reqPerSec * 1000)
	}
	dirsScanning := 0
	for _, ds := range c.dirs {
		if ds.state == dirScanning {
			dirsScanning++
		}
	}
	c.emit(Event{Type: EventStats, Payload: payloadFor(StatsPayload{
		ReqSent:      c.statsReqSent,
		Hits:         c.statsHits,
		InFlight:     c.inFlight,
		FrontierLen:  c.frontier.Len(),
		DirsScanning: dirsScanning,
		ReqPerSec:    reqPerSec,
		HitRate:      hitRate,
		ElapsedMs:    elapsed.Milliseconds(),
		ETAms:        etaMs,
	})})
}

func (c *Coordinator) emitSnapshot() {
	top := c.frontier.TopK(SnapshotTopK)
	entries := make([]SnapshotEntry, len(top))
	for i, cand := range top {
		entries[i] = SnapshotEntry{
			Path: cand.Path, Dir: cand.ParentDir, Provenance: cand.Provenance,
			Score: cand.Score, Depth: cand.Depth,
		}
	}
	c.emit(Event{Type: EventSnapshot, Payload: payloadFor(SnapshotPayload{TopK: entries, Total: c.frontier.Len()})})
}
