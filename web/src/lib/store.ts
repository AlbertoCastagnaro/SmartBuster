// store.ts — the load-bearing data layer (spec §3): a pure reducer over the
// WS event stream, framework-thin (no Svelte import) so it's unit-testable
// against recorded event fixtures with no browser. `reduce` is the part the
// tests exercise directly; `createStore` is a thin subscribe/emit wrapper
// components use at runtime.
//
// The `hit` event now carries a HitPayload{Provenance,Status,Size} (a
// pre-Phase-6 gap fix — it used to carry only Confidence, and the store
// had to opportunistically back-fill provenance from frontier.snapshot
// sightings, which was necessarily partial). reduceHit below reads the
// payload directly; no inference needed.
import type { EngineFinding, EventCategory, EventType, ScanStatus, TechEntry, WireEvent } from "./wire";
import { payloadOf } from "./wire";

export interface GaugeState {
  reqSent: number;
  hits: number;
  inFlight: number;
  frontierLen: number;
  dirsScanning: number;
  reqPerSec: number;
  hitRate: number;
  elapsedMs: number;
  etaMs: number; // -1 = unbounded
}

export interface SparkPoint {
  t: number;
  reqPerSec: number;
  hitRate: number;
  inFlight: number;
}

export interface FrontierRow {
  path: string;
  dir: string;
  provenance: string;
  score: number;
  depth: number;
}

export interface Finding {
  url: string;
  dir: string;
  confidence: number;
  provenance: string;
  status: number;
  size: number;
  isAlias: boolean;
  /** Only known when rebuilt from GET .../findings (engine.Finding.Aliases) —
   * the live WS `hit` stream has no way to link an alias to its canonical
   * URL, only that it *is* one (Message:"alias"). */
  canonicalUrl?: string;
  /** Wall-clock time of the live `hit` event; "" for entries rebuilt from
   * the findings-list endpoint (spec §7 resync), which has no per-finding
   * timestamp. */
  time: string;
}

export interface TreeNode {
  path: string; // "" for root, else "/admin", "/admin/config", ...
  name: string;
  children: Map<string, TreeNode>;
  findings: Finding[];
}

export interface TechState {
  techs: TechEntry[];
  waf?: string;
}

export interface LogEntry {
  seq: number;
  time: string;
  type: EventType;
  category: EventCategory;
  message?: string;
  dir?: string;
  url?: string;
  source?: string; // WarnPayload.source
  kind?: string; // ErrorPayload.Kind
}

export interface Lifecycle {
  id?: string;
  target?: string;
  state?: ScanStatus["state"];
  seed?: number;
  startedAt?: string;
  finishedAt?: string;
  mode?: string;
}

export interface ResyncNote {
  at: string;
  /** How many finding rows (canonical + alias) applyFindingsSnapshot rebuilt
   * the tree/findings from — an informational confirmation, not a
   * discrepancy warning: GET .../findings is authoritative, so this is a
   * real rebuild, not a "may be missing something" hint. */
  rebuiltFindings: number;
}

export interface StoreState {
  lifecycle: Lifecycle;
  gauges: GaugeState;
  sparkline: SparkPoint[];
  frontier: { topK: FrontierRow[]; total: number };
  tree: TreeNode;
  findings: Finding[];
  coalesced: number;
  tech: TechState;
  log: LogEntry[];
  spaPivot: { fired: boolean; url?: string };
  resync?: ResyncNote;
}

const SPARKLINE_CAP = 120; // ~48s at the 400ms stats cadence
const LOG_CAP = 2000;

export function createInitialState(): StoreState {
  return {
    lifecycle: {},
    gauges: { reqSent: 0, hits: 0, inFlight: 0, frontierLen: 0, dirsScanning: 0, reqPerSec: 0, hitRate: 0, etaMs: -1, elapsedMs: 0 },
    sparkline: [],
    frontier: { topK: [], total: 0 },
    tree: { path: "", name: "", children: new Map(), findings: [] },
    findings: [],
    coalesced: 0,
    tech: { techs: [] },
    log: [],
    spaPivot: { fired: false },
  };
}

let logSeq = 0;

function appendLog(state: StoreState, ev: WireEvent): StoreState {
  const entry: LogEntry = {
    seq: logSeq++,
    time: ev.time,
    type: ev.type,
    category: ev.category,
    message: ev.message,
    dir: ev.dir,
    url: ev.url,
  };
  if (ev.type === "warning") entry.source = payloadOf(ev as WireEvent<"warning">)?.source;
  if (ev.type === "error") entry.kind = payloadOf(ev as WireEvent<"error">)?.Kind;

  const log = state.log.length >= LOG_CAP ? [...state.log.slice(state.log.length - LOG_CAP + 1), entry] : [...state.log, entry];
  return { ...state, log };
}

function pathnameOf(url: string | undefined): string | undefined {
  if (!url) return undefined;
  try {
    return new URL(url).pathname;
  } catch {
    return undefined;
  }
}

/** The directory containing pathname, in the engine's own "dir + / + path"
 * convention (root is "", e.g. "/admin/config.php" -> "/admin"). Used to
 * derive `dir` for entries rebuilt from GET .../findings, which (unlike the
 * live `hit` event) has no Dir field of its own — only a full URL. */
function dirOfPathname(pathname: string): string {
  const segments = pathname.split("/").filter(Boolean);
  segments.pop();
  return segments.length ? "/" + segments.join("/") : "";
}

function insertIntoTree(root: TreeNode, dir: string, finding: Finding): TreeNode {
  const segments = dir.split("/").filter(Boolean);
  // Clone the path from root to the target node (structural sharing for
  // everything else) so components relying on reference identity for
  // "did this subtree change" checks still work.
  const newRoot: TreeNode = { ...root, children: new Map(root.children) };
  let cursor = newRoot;
  let acc = "";
  for (const seg of segments) {
    acc += "/" + seg;
    const existing = cursor.children.get(seg);
    const child: TreeNode = existing
      ? { ...existing, children: new Map(existing.children) }
      : { path: acc, name: seg, children: new Map(), findings: [] };
    cursor.children.set(seg, child);
    cursor = child;
  }
  cursor.findings = [...cursor.findings, finding];
  return newRoot;
}

function reduceHit(state: StoreState, ev: WireEvent<"hit">): StoreState {
  const dir = ev.dir ?? "";
  const url = ev.url ?? "";
  const isAlias = ev.message === "alias";
  const p = payloadOf(ev);

  const finding: Finding = {
    url,
    dir,
    confidence: ev.confidence ?? 0,
    provenance: p?.Provenance ?? "",
    status: p?.Status ?? 0,
    size: p?.Size ?? 0,
    isAlias,
    time: ev.time,
  };

  return {
    ...state,
    tree: insertIntoTree(state.tree, dir, finding),
    findings: [...state.findings, finding],
  };
}

/** Rebuilds the tree/findings wholesale from GET .../findings (spec §3/§7
 * resync, closing the gap ScanStatus's bare count left open): each
 * canonical Finding becomes one entry, plus one dimmed entry per alias URL
 * — properly linked via canonicalUrl this time, which the live `hit`
 * stream can never do (an alias event only says Message:"alias", not which
 * finding it's an alias of). This *replaces* rather than merges: it's the
 * server's authoritative state at the moment of the request, superseding
 * whatever the client had reconstructed from the (lossy) WS stream so far. */
export function applyFindingsSnapshot(state: StoreState, findings: EngineFinding[]): StoreState {
  let tree: TreeNode = { path: "", name: "", children: new Map(), findings: [] };
  const flat: Finding[] = [];

  for (const f of findings) {
    const pathname = pathnameOf(f.URL) ?? f.URL;
    const dir = dirOfPathname(pathname);
    const canonical: Finding = {
      url: f.URL,
      dir,
      confidence: f.Confidence,
      provenance: f.Provenance,
      status: f.Status,
      size: f.Size,
      isAlias: false,
      time: "",
    };
    tree = insertIntoTree(tree, dir, canonical);
    flat.push(canonical);

    for (const aliasURL of f.Aliases ?? []) {
      const aliasPathname = pathnameOf(aliasURL) ?? aliasURL;
      const aliasDir = dirOfPathname(aliasPathname);
      const alias: Finding = {
        url: aliasURL,
        dir: aliasDir,
        confidence: f.Confidence,
        provenance: f.Provenance,
        status: f.Status,
        size: f.Size,
        isAlias: true,
        canonicalUrl: f.URL,
        time: "",
      };
      tree = insertIntoTree(tree, aliasDir, alias);
      flat.push(alias);
    }
  }

  return { ...state, tree, findings: flat, resync: { at: new Date().toISOString(), rebuiltFindings: flat.length } };
}

function reduceStats(state: StoreState, ev: WireEvent<"stats">): StoreState {
  const p = payloadOf(ev);
  if (!p) return state;
  const gauges: GaugeState = {
    reqSent: p.ReqSent,
    hits: p.Hits,
    inFlight: p.InFlight,
    frontierLen: p.FrontierLen,
    dirsScanning: p.DirsScanning,
    reqPerSec: p.ReqPerSec,
    hitRate: p.HitRate,
    elapsedMs: p.ElapsedMs,
    etaMs: p.ETAms,
  };
  const point: SparkPoint = { t: p.ElapsedMs, reqPerSec: p.ReqPerSec, hitRate: p.HitRate, inFlight: p.InFlight };
  const sparkline = state.sparkline.length >= SPARKLINE_CAP ? [...state.sparkline.slice(1), point] : [...state.sparkline, point];
  return { ...state, gauges, sparkline };
}

function reduceSnapshot(state: StoreState, ev: WireEvent<"frontier.snapshot">): StoreState {
  const p = payloadOf(ev);
  if (!p) return state;
  const topK: FrontierRow[] = p.TopK.map((e) => ({ path: e.Path, dir: e.Dir, provenance: e.Provenance, score: e.Score, depth: e.Depth }));
  return { ...state, frontier: { topK, total: p.Total } };
}

function reduceCoalesced(state: StoreState, ev: WireEvent<"hit.coalesced">): StoreState {
  const p = payloadOf(ev);
  return { ...state, coalesced: state.coalesced + (p?.count ?? 0) };
}

function reduceTech(state: StoreState, ev: WireEvent): StoreState {
  return { ...state, tech: { ...state.tech, techs: ev.tech ?? [] } };
}

function reduceWaf(state: StoreState, ev: WireEvent): StoreState {
  return { ...state, tech: { ...state.tech, waf: ev.waf } };
}

/** The store's core: pure, one event in, one new state out. Exported
 * directly so tests can fold it over a fixture stream with no Svelte, no
 * connection, no timers. */
export function reduce(state: StoreState, ev: WireEvent): StoreState {
  switch (ev.type) {
    case "hit":
      return appendLog(reduceHit(state, ev as WireEvent<"hit">), ev);
    case "hit.coalesced":
      return reduceCoalesced(state, ev as WireEvent<"hit.coalesced">);
    case "stats":
      return reduceStats(state, ev as WireEvent<"stats">);
    case "frontier.snapshot":
      return reduceSnapshot(state, ev as WireEvent<"frontier.snapshot">);
    case "tech.detected":
      return reduceTech(state, ev);
    case "waf.detected":
      return appendLog(reduceWaf(state, ev), ev);
    case "spa.pivot":
      return appendLog({ ...state, spaPivot: { fired: true, url: ev.url } }, ev);
    case "scan.started":
    case "scan.finished":
    case "calibration.done":
    case "warning":
    case "error":
    case "trap.detected":
    case "branch.pruned":
    case "throttle":
      return appendLog(state, ev);
    default:
      return state;
  }
}

/** Merges a REST ScanStatus resync (spec §3/§7): lifecycle fields only.
 * `isReconnect` is unused here now — it's the caller's cue to also fetch
 * GET .../findings and call applyFindingsSnapshot, which is where the
 * actual tree/findings rebuild (and the resync note) now happens; kept as
 * a parameter so callers don't need two near-identical entry points. */
export function applyResync(state: StoreState, status: ScanStatus, _isReconnect: boolean): StoreState {
  const lifecycle: Lifecycle = {
    id: status.id,
    target: status.target,
    state: status.state,
    seed: status.seed,
    startedAt: status.started_at,
    finishedAt: status.finished_at,
    mode: status.mode,
  };
  return { ...state, lifecycle };
}

export interface Store {
  getState(): StoreState;
  subscribe(run: (s: StoreState) => void): () => void;
  applyEvent(ev: WireEvent): void;
  applyResync(status: ScanStatus, isReconnect: boolean): void;
  applyFindingsSnapshot(findings: EngineFinding[]): void;
}

/** Thin subscribe/emit wrapper around `reduce`/`applyResync` for runtime use
 * — deliberately duck-types Svelte's store contract (`subscribe(run)`)
 * without importing svelte, so this file stays framework-thin. */
export function createStore(): Store {
  let state = createInitialState();
  const listeners = new Set<(s: StoreState) => void>();

  function emit() {
    for (const l of listeners) l(state);
  }

  return {
    getState: () => state,
    subscribe(run) {
      listeners.add(run);
      run(state);
      return () => listeners.delete(run);
    },
    applyEvent(ev) {
      state = reduce(state, ev);
      emit();
    },
    applyResync(status, isReconnect) {
      state = applyResync(state, status, isReconnect);
      emit();
    },
    applyFindingsSnapshot(findings) {
      state = applyFindingsSnapshot(state, findings);
      emit();
    },
  };
}
