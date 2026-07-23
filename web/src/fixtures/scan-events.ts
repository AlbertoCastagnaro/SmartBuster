// scan-events.ts — the event-stream fixture the 5b spec's build order calls
// for ("record an event-stream fixture from a real scan early... so every
// component develops against realistic data").
//
// `realCapturedEvents` is exactly that: a real `smartbuster serve` daemon,
// a real Coordinator, run against test/fixtures.NewSPATarget() (the same
// fixture internal/engine/harvest_test.go calls "spa_with_api" in its DoD
// comment) — a small wordlist, WS-captured verbatim via
// golang.org/x/net/websocket. Hostnames were normalized from the ephemeral
// httptest port to `target.test`; nothing else was edited. It naturally
// exercises: scan lifecycle, calibration, tech.detected, the SPA-pivot
// warning+event, real hits (root shell, `crawl:js`-harvested API endpoints),
// alias hits paired with branch.pruned (duplicate-content novelty gate),
// and non-empty frontier.snapshot top-K rows.
//
// One event never occurs from a clean local capture: `scan.started` fires
// from the coordinator's Run goroutine, which — on loopback, against a
// trivial fixture — reliably wins the race against the WS client's own
// dial, so it's never seen client-side in this recording. It's prepended
// here as the one synthesized entry in `realCapturedEvents`, timestamped
// just before the first real line.
//
// `syntheticEvents` covers the wire shapes real capture couldn't reach
// hermetically in reasonable time: `error` (a forced request failure),
// `throttle` (WAF/rate-limit backoff), `trap.detected` (wildcard/tarpit
// suspicion — distinct from the branch.pruned pairing above, which came
// from the *duplicate-content* gate, not a trap), and `hit.coalesced`
// (hub-synthesized only when one client's buffer overflows under flood).
import type { WireEvent } from "../lib/wire";

// eslint-disable-next-line import/no-unresolved
import rawCapture from "./scan-events.raw.jsonl?raw";

function parseJSONL(text: string): WireEvent[] {
  return text
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean)
    .map((l) => JSON.parse(l) as WireEvent);
}

const captured = parseJSONL(rawCapture);

const syntheticScanStarted: WireEvent = {
  type: "scan.started",
  category: "scan",
  time: new Date(new Date(captured[0].time).getTime() - 50).toISOString(),
  url: "http://target.test",
};

export const realCapturedEvents: WireEvent[] = [syntheticScanStarted, ...captured];

export const syntheticEvents: WireEvent[] = [
  {
    type: "error",
    category: "error",
    time: "2026-01-01T00:00:10.000Z",
    url: "http://target.test/slow-endpoint",
    dir: "",
    payload: { URL: "http://target.test/slow-endpoint", Kind: "timeout", Message: "context deadline exceeded" },
  },
  {
    type: "throttle",
    category: "control",
    time: "2026-01-01T00:00:11.000Z",
    message: "WAF/rate-limit onset detected; backing off",
  },
  {
    type: "trap.detected",
    category: "trap",
    time: "2026-01-01T00:00:12.000Z",
    dir: "/wildcard-suspect",
    message: "wildcard-suspect: hit-rate too high, recursion stopped for this branch",
  },
  {
    type: "hit.coalesced",
    category: "discovery",
    time: "2026-01-01T00:00:13.000Z",
    payload: { count: 42 },
  },
];

export const allFixtureEvents: WireEvent[] = [...realCapturedEvents, ...syntheticEvents];
