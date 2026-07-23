import { describe, expect, it } from "vitest";
import { allFixtureEvents, realCapturedEvents, syntheticEvents } from "../fixtures/scan-events";
import { applyFindingsSnapshot, applyResync, createInitialState, reduce } from "./store";
import type { EngineFinding, WireEvent } from "./wire";

function fold(events: WireEvent[]) {
  return events.reduce(reduce, createInitialState());
}

describe("store: full recorded fixture (spec §10 DoD #1)", () => {
  const state = fold(realCapturedEvents);

  it("produces the correct discovered-path tree, nested by directory", () => {
    expect(state.tree.findings.map((f) => f.url)).toContain("http://target.test/app.js");

    const api = state.tree.children.get("api")!;
    expect(api).toBeDefined();
    const v1 = api.children.get("v1")!;
    expect(v1).toBeDefined();
    expect(v1.findings.some((f) => f.url === "http://target.test/api/v1/orders")).toBe(true);
    expect(v1.findings.some((f) => f.url === "http://target.test/api/v1/profile")).toBe(true);

    // alias hits still land in the tree, marked (not silently dropped) —
    // "src/src" nests under the "/src" dir node, not root
    expect(state.tree.children.get("src")!.findings.some((f) => f.isAlias && f.url === "http://target.test/src/src")).toBe(true);
  });

  it("produces the correct flat findings list, alias-aware", () => {
    const canonical = state.findings.filter((f) => !f.isAlias);
    const aliases = state.findings.filter((f) => f.isAlias);
    expect(canonical.length).toBe(4); // app.js, orders, profile, src
    expect(aliases.length).toBe(5); // the five duplicate-content hits captured
  });

  it("produces the correct gauge state (replace-latest stats)", () => {
    expect(state.gauges.reqSent).toBeGreaterThan(0);
    expect(state.gauges.elapsedMs).toBeGreaterThan(0);
  });

  it("produces a non-empty frontier top-K from real snapshots", () => {
    expect(state.frontier.topK.length).toBeGreaterThan(0);
    for (const row of state.frontier.topK) {
      expect(typeof row.path).toBe("string");
      expect(typeof row.provenance).toBe("string");
    }
  });

  it("produces the correct tech state (no crash on an empty Tech payload)", () => {
    expect(state.tech.techs).toEqual([]);
  });

  it("logs calibration/warning/spa.pivot/branch.pruned with Category, never dropped", () => {
    const categories = new Set(state.log.map((l) => l.category));
    expect(categories).toContain("calibration");
    expect(categories).toContain("warning");
    expect(categories).toContain("discovery"); // spa.pivot + branch.pruned's own Category is "trap", spa.pivot's is "discovery"
    expect(categories).toContain("trap");

    const warn = state.log.find((l) => l.category === "warning")!;
    expect(warn.source).toBe("spa"); // WarnPayload.source, never parsed from Message

    expect(state.spaPivot.fired).toBe(true);
    expect(state.spaPivot.url).toBe("http://target.test");
  });

  it("carries real Provenance/Status/Size from the hit payload, not inferred", () => {
    const orders = state.findings.find((f) => f.url === "http://target.test/api/v1/orders")!;
    expect(orders.provenance).toBe("crawl:js");
    expect(orders.status).toBe(200);
    expect(orders.size).toBeGreaterThan(0);
  });
});

describe("store: lossy-stream handling (spec §10 DoD #3)", () => {
  it("hit.coalesced bumps a counter, never fabricates rows", () => {
    const before = createInitialState();
    const after = reduce(before, {
      type: "hit.coalesced",
      category: "discovery",
      time: "2026-01-01T00:00:00Z",
      payload: { count: 7 },
    });
    expect(after.coalesced).toBe(7);
    expect(after.findings.length).toBe(0);
    expect(after.tree.findings.length).toBe(0);

    const again = reduce(after, {
      type: "hit.coalesced",
      category: "discovery",
      time: "2026-01-01T00:00:01Z",
      payload: { count: 3 },
    });
    expect(again.coalesced).toBe(10); // accumulates rather than replacing
  });

  it("stats and frontier.snapshot are replace-latest, not accumulated", () => {
    const s1 = reduce(createInitialState(), {
      type: "stats",
      category: "telemetry",
      time: "t1",
      payload: { ReqSent: 1, Hits: 0, InFlight: 1, FrontierLen: 5, DirsScanning: 1, ReqPerSec: 1, HitRate: 0, ElapsedMs: 100, ETAms: 500 },
    });
    const s2 = reduce(s1, {
      type: "stats",
      category: "telemetry",
      time: "t2",
      payload: { ReqSent: 2, Hits: 1, InFlight: 2, FrontierLen: 4, DirsScanning: 1, ReqPerSec: 2, HitRate: 0.5, ElapsedMs: 200, ETAms: 400 },
    });
    expect(s2.gauges.reqSent).toBe(2);
    expect(s2.sparkline.length).toBe(2); // both points kept for the sparkline ring...

    const snap1 = reduce(createInitialState(), {
      type: "frontier.snapshot",
      category: "telemetry",
      time: "t1",
      payload: { TopK: [{ Path: "a", Dir: "", Provenance: "wordlist", Score: 1, Depth: 1 }], Total: 10 },
    });
    const snap2 = reduce(snap1, {
      type: "frontier.snapshot",
      category: "telemetry",
      time: "t2",
      payload: { TopK: [{ Path: "b", Dir: "", Provenance: "corpus:php", Score: 2, Depth: 1 }], Total: 20 },
    });
    // ...but frontier.snapshot itself replaces wholesale, no merge of old+new rows
    expect(snap2.frontier.topK).toEqual([{ path: "b", dir: "", provenance: "corpus:php", score: 2, depth: 1 }]);
    expect(snap2.frontier.total).toBe(20);
  });

  it("caps the sparkline ring buffer rather than growing unbounded", () => {
    let state = createInitialState();
    for (let i = 0; i < 200; i++) {
      state = reduce(state, {
        type: "stats",
        category: "telemetry",
        time: `t${i}`,
        payload: { ReqSent: i, Hits: 0, InFlight: 0, FrontierLen: 0, DirsScanning: 0, ReqPerSec: 0, HitRate: 0, ElapsedMs: i * 400, ETAms: -1 },
      });
    }
    expect(state.sparkline.length).toBeLessThanOrEqual(120);
    expect(state.sparkline[state.sparkline.length - 1].t).toBe(199 * 400);
  });
});

describe("store: hit payload (spec/6 gap-fix — Provenance/Status/Size on `hit`)", () => {
  it("reads Provenance/Status/Size straight from HitPayload", () => {
    const state = reduce(createInitialState(), {
      type: "hit",
      category: "discovery",
      time: "t1",
      dir: "/admin",
      url: "http://target.test/admin/config.php",
      confidence: 0.9,
      payload: { Provenance: "corpus:php+wordpress", Status: 200, Size: 512 },
    });
    const f = state.findings[0];
    expect(f.provenance).toBe("corpus:php+wordpress");
    expect(f.status).toBe(200);
    expect(f.size).toBe(512);
  });

  it("defaults gracefully when a hit somehow carries no payload", () => {
    const state = reduce(createInitialState(), {
      type: "hit",
      category: "discovery",
      time: "t1",
      dir: "",
      url: "http://target.test/x",
      confidence: 0.9,
    });
    const f = state.findings[0];
    expect(f.provenance).toBe("");
    expect(f.status).toBe(0);
    expect(f.size).toBe(0);
  });
});

describe("store: REST resync (spec §3/§7)", () => {
  it("updates lifecycle without touching findings/tree", () => {
    const state = applyResync(createInitialState(), { id: "abc", target: "http://x", state: "running", seed: 1, started_at: "t", findings: 0 }, false);
    expect(state.lifecycle.id).toBe("abc");
    expect(state.resync).toBeUndefined();
  });
});

describe("store: findings-list resync rebuild (spec §3/§7 gap fix)", () => {
  const findings: EngineFinding[] = [
    { URL: "http://target.test/app.js", Status: 200, Size: 50, Confidence: 0.8, Provenance: "crawl:html", ContentHash: 1 },
    {
      URL: "http://target.test/api/v1/orders",
      Status: 200,
      Size: 10,
      Confidence: 0.8,
      Provenance: "crawl:js",
      ContentHash: 2,
      Aliases: ["http://target.test/api/v1/orders/src"],
    },
  ];

  it("rebuilds the tree and flat findings list wholesale from an authoritative list", () => {
    const state = applyFindingsSnapshot(createInitialState(), findings);
    expect(state.tree.findings.some((f) => f.url === findings[0].URL)).toBe(true);
    const v1 = state.tree.children.get("api")!.children.get("v1")!;
    expect(v1.findings.some((f) => f.url === "http://target.test/api/v1/orders")).toBe(true);
    // the alias "orders/src" lives one level deeper than "orders" itself —
    // its own dir is /api/v1/orders, not /api/v1
    const ordersDir = v1.children.get("orders")!;
    expect(ordersDir.findings.some((f) => f.url === "http://target.test/api/v1/orders/src" && f.isAlias)).toBe(true);
    expect(state.findings).toHaveLength(3); // 2 canonical + 1 alias
  });

  it("links an alias to its canonical URL — something the live WS stream can never do", () => {
    const state = applyFindingsSnapshot(createInitialState(), findings);
    const alias = state.findings.find((f) => f.isAlias)!;
    expect(alias.canonicalUrl).toBe("http://target.test/api/v1/orders");
  });

  it("sets an informational resync note, not a discrepancy warning", () => {
    const state = applyFindingsSnapshot(createInitialState(), findings);
    expect(state.resync).toBeDefined();
    expect(state.resync!.rebuiltFindings).toBe(3);
  });

  it("replaces rather than merges — a prior live-stream reconstruction is discarded", () => {
    let state = reduce(createInitialState(), {
      type: "hit",
      category: "discovery",
      time: "t1",
      dir: "",
      url: "http://target.test/stale-from-before-reconnect",
      confidence: 0.5,
      payload: { Provenance: "wordlist", Status: 200, Size: 1 },
    });
    state = applyFindingsSnapshot(state, findings);
    expect(state.findings.some((f) => f.url === "http://target.test/stale-from-before-reconnect")).toBe(false);
  });
});

describe("store: full type coverage", () => {
  it("folds every event type in the combined fixture without throwing", () => {
    expect(() => fold(allFixtureEvents)).not.toThrow();
  });

  it("the synthetic supplement covers error/throttle/trap.detected/hit.coalesced", () => {
    const types = new Set(syntheticEvents.map((e) => e.type));
    expect(types).toEqual(new Set(["error", "throttle", "trap.detected", "hit.coalesced"]));
  });
});
