// wire.ts — the exact JSON shapes internal/daemon and internal/engine put on
// the wire (specs/smartbuster-phase5a-spec.md, as delivered). Two casing
// conventions coexist and must NOT be normalized to one:
//
//   - Go structs with no `json:"..."` tags at all (engine.Config,
//     engine.SessionState, and — less obviously — the Payload sub-structs
//     StatsPayload/SnapshotPayload/SnapshotEntry/ErrorPayload/TechEntry)
//     serialize with their literal PascalCase Go field names.
//   - Go structs the daemon defines with explicit tags (ScanStatus,
//     AdjustRequest, PatternRequest, InjectRequest, SaveRequest,
//     SessionMeta, WarnPayload, HitCoalescedPayload) serialize snake_case
//     (or, for single-word fields, plain lowercase).
//
// `Event` itself is tagged (lowercase top-level keys: type/category/time/
// dir/url/confidence/message/tech/waf/payload) but its `Tech` array elements
// and typed `Payload` bodies are untagged engine structs, so they flip back
// to PascalCase. This is the single easiest thing to get wrong porting this
// file — every payload interface below is commented with which rule it
// follows.

export type EventType =
  | "scan.started"
  | "scan.finished"
  | "calibration.done"
  | "hit"
  | "hit.coalesced"
  | "spa.pivot"
  | "tech.detected"
  | "waf.detected"
  | "trap.detected"
  | "branch.pruned"
  | "stats"
  | "frontier.snapshot"
  | "warning"
  | "error"
  | "throttle";

export type EventCategory =
  | "scan"
  | "calibration"
  | "discovery"
  | "tech"
  | "trap"
  | "telemetry"
  | "warning"
  | "error"
  | "control";

/** engine.TechEntry — untagged, PascalCase. */
export interface TechEntry {
  Name: string;
  Category: string;
  Version: string;
  Confidence: number;
  Layer: "backend" | "edge" | "unknown";
  Sources: string[];
  RuleIDs: string[];
}

/** engine.StatsPayload — untagged, PascalCase. ETAms is -1 when unbounded. */
export interface StatsPayload {
  ReqSent: number;
  Hits: number;
  InFlight: number;
  FrontierLen: number;
  DirsScanning: number;
  ReqPerSec: number;
  HitRate: number;
  ElapsedMs: number;
  ETAms: number;
}

/** engine.SnapshotEntry — untagged, PascalCase. */
export interface SnapshotEntry {
  Path: string;
  Dir: string;
  Provenance: string;
  Score: number;
  Depth: number;
}

/** engine.SnapshotPayload — untagged, PascalCase. */
export interface SnapshotPayload {
  TopK: SnapshotEntry[];
  Total: number;
}

/** engine.ErrorPayload — untagged, PascalCase. */
export interface ErrorPayload {
  URL: string;
  Kind: "timeout" | "connreset" | "tls" | "other" | string;
  Message: string;
}

/** engine.WarnPayload — explicitly tagged `json:"source"`, lowercase. */
export interface WarnPayload {
  source: string;
}

/** daemon.HitCoalescedPayload — explicitly tagged `json:"count"`, lowercase. */
export interface HitCoalescedPayload {
  count: number;
}

/** engine.HitPayload — untagged, PascalCase. Phase 5b/6 gap-fix: the `hit`
 * event previously carried Confidence but no Provenance/Status/Size, even
 * though engine.Finding (the record a hit becomes) always had all three. */
export interface HitPayload {
  Provenance: string;
  Status: number;
  Size: number;
}

/** Maps an EventType to its Payload's decoded shape; undefined for types with no payload. */
export type PayloadOf<T extends EventType> = T extends "stats"
  ? StatsPayload
  : T extends "frontier.snapshot"
    ? SnapshotPayload
    : T extends "warning"
      ? WarnPayload
      : T extends "error"
        ? ErrorPayload
        : T extends "hit.coalesced"
          ? HitCoalescedPayload
          : T extends "hit"
            ? HitPayload
            : undefined;

/**
 * engine.Event as it arrives over the WS stream — tagged, lowercase
 * top-level keys. `payload` is already a decoded JSON value (Go's
 * json.RawMessage just means "don't validate this sub-tree's shape
 * server-side"; on the wire it's ordinary nested JSON), so no second
 * JSON.parse is needed — only a cast via `payloadOf`.
 */
export interface WireEvent<T extends EventType = EventType> {
  type: T;
  category: EventCategory;
  time: string; // RFC3339
  dir?: string;
  url?: string;
  confidence?: number;
  message?: string;
  tech?: TechEntry[];
  waf?: string;
  payload?: PayloadOf<T>;
}

export function payloadOf<T extends EventType>(ev: WireEvent<T>): PayloadOf<T> | undefined {
  return ev.payload;
}

// --- REST: daemon-defined, snake_case ---

export type ScanState = "running" | "paused" | "stopped" | "finished";

/** daemon.ScanStatus — GET /api/scans/{id}'s response body. */
export interface ScanStatus {
  id: string;
  target: string;
  state: ScanState;
  seed: number;
  started_at: string;
  finished_at?: string;
  findings: number;
  mode?: string;
}

/** daemon.AdjustRequest — PATCH /api/scans/{id}'s body; omitted = unchanged. */
export interface AdjustRequest {
  rate?: number;
  concurrency?: number;
  mode?: string;
}

/** daemon.PatternRequest — pin/exclude/boost/demote's shared body. */
export interface PatternRequest {
  pattern: string;
  factor?: number;
}

/** daemon.InjectRequest — POST /api/scans/{id}/inject's body. */
export interface InjectRequest {
  terms: string[];
}

/** daemon.SaveRequest — POST /api/scans/{id}/save's optional body. */
export interface SaveRequest {
  name?: string;
}

/** daemon.SessionMeta — one GET /api/sessions list entry. */
export interface SessionMeta {
  id: string;
  target: string;
  seed: number;
  saved_at: string;
}

// --- engine.Config / engine.SessionState: untagged, PascalCase ---

/** scope.Config — untagged, PascalCase. */
export interface ScopeConfig {
  AllowHosts?: string[];
  ExcludeHosts?: string[];
  ExcludePatterns?: string[];
}

/** engine.ScoreWeights — untagged, PascalCase. */
export interface ScoreWeights {
  WTech: number;
  WSem: number;
  WAssoc: number;
  WConv: number;
}

/**
 * engine.Config — the scan-launch POST body (spec §8). Every
 * `time.Duration` field is int64 **nanoseconds** on the wire; every
 * numeric field follows the engine's own "<=0 means apply the default"
 * convention EXCEPT the bools, which the engine never defaults — a bool
 * left at its Go zero value (false) is a real `false`, not "unset", so
 * the launcher must send explicit values for the ones the CLI itself
 * defaults to true (Robots/Sitemap/Crawl/JSHarvest/FaviconProbe).
 */
export interface EngineConfig {
  Targets: string[];
  Wordlist?: string;
  Concurrency?: number;
  Rate?: number;
  Jitter?: number;
  MaxDepth?: number;
  RequestTO?: number; // ns
  Seed?: number;
  Scope?: ScopeConfig;
  DryRun?: boolean;
  OutDir?: string;

  PerDirBudget?: number;
  TimePerBranch?: number; // ns

  RulesetDir?: string;
  UserRulesDir?: string;
  RulesOff?: string[];
  NmapFile?: string;
  RunNmap?: boolean;
  ActiveProbes?: boolean;
  FaviconProbe?: boolean;

  CorpusDB?: string;
  SecListsPath?: string;
  SourceMap?: string;
  CorpusMax?: number;
  TechBoostW?: number;
  CorpusStream?: boolean;

  Weights?: ScoreWeights;
  MarkovOrder?: number;
  MarkovMinSamples?: number;
  LearnMinConf?: number;
  SubtreeBurst?: number;
  Epsilon?: number;
  ReprioHits?: number;
  ReprioInterval?: number; // ns

  Robots?: boolean;
  Sitemap?: boolean;
  Wayback?: boolean;
  WaybackMax?: number;
  SeedAssets?: boolean;
  WaybackURL?: string;

  Crawl?: boolean;
  JSHarvest?: boolean;
  Headless?: boolean;
  CrawlDepth?: number;

  SavePath?: string;
  Autosave?: number; // ns
}

/** CLI scan defaults (cmd/smartbuster/main.go), mirrored so the launcher's
 * initial form state matches `smartbuster scan`'s own behavior rather than
 * the engine's raw zero-value fallback (which would leave every bool off). */
export const ENGINE_CONFIG_DEFAULTS: Required<
  Pick<
    EngineConfig,
    | "Concurrency"
    | "Rate"
    | "Jitter"
    | "MaxDepth"
    | "RequestTO"
    | "FaviconProbe"
    | "CorpusMax"
    | "TechBoostW"
    | "Robots"
    | "Sitemap"
    | "Wayback"
    | "WaybackMax"
    | "SeedAssets"
    | "Crawl"
    | "JSHarvest"
    | "Headless"
    | "ActiveProbes"
    | "RunNmap"
    | "DryRun"
  >
> = {
  Concurrency: 20,
  Rate: 0,
  Jitter: 0.3,
  MaxDepth: 4,
  RequestTO: 10_000_000_000, // 10s in ns
  FaviconProbe: true,
  CorpusMax: 0,
  TechBoostW: 2.0,
  Robots: true,
  Sitemap: true,
  Wayback: false,
  WaybackMax: 5000,
  SeedAssets: false,
  Crawl: true,
  JSHarvest: true,
  Headless: false,
  ActiveProbes: false,
  RunNmap: false,
  DryRun: false,
};

/** engine.Finding — untagged, PascalCase. Appears inside SessionState.Findings
 * (session download/resume), NOT on the `hit` WireEvent — see the store's
 * handling note on the protocol gap this creates for live-stream hits. */
export interface EngineFinding {
  URL: string;
  Status: number;
  Size: number;
  Confidence: number;
  Provenance: string;
  ContentHash: number;
  Aliases?: string[];
}

/** engine.SessionState — untagged Config/Findings, but tagged (snake_case)
 * at its own top level. GET /api/sessions/{id}'s response body. Large (the
 * whole frontier/baselines/learners); the app should treat it as a heavy,
 * lazily-fetched download per spec §8, not something to poll. */
export interface SessionState {
  version: number;
  target: string;
  config: EngineConfig;
  saved_at: string;
  frontier: unknown[];
  dirs: unknown[];
  baselines: Record<string, unknown>;
  findings: EngineFinding[];
  seen_content: Record<string, string[]>;
  profile?: unknown;
  markov: unknown;
  assoc: unknown;
  dir_ctx: Record<string, unknown>;
  visited_set: string[];
  stats_req_sent: number;
  stats_hits: number;
  waf_ring: unknown[];
  backoff_multiplier: number;
  backoff_until: string;
  nmap_seeds: unknown[];
  spa_mode: boolean;
  root_refined: boolean;
}
