// rest.ts — typed wrappers for every 5a REST route (spec §4, §6, §8), all
// going through connection.ts's apiFetch (Bearer token; Origin is set by the
// browser itself). Kept as one flat module — 15 routes, no grouping worth
// the indirection.
import { apiFetch } from "./connection";
import type {
  AdjustRequest,
  EngineConfig,
  EngineFinding,
  InjectRequest,
  PatternRequest,
  SaveRequest,
  ScanStatus,
  SessionMeta,
  SessionState,
} from "./wire";

const json = (body: unknown): RequestInit => ({ method: "POST", body: JSON.stringify(body) });

export function startScan(config: EngineConfig): Promise<{ id: string }> {
  return apiFetch("/api/scans", json(config));
}

export function listScans(): Promise<ScanStatus[]> {
  return apiFetch("/api/scans");
}

export function getScan(id: string): Promise<ScanStatus> {
  return apiFetch(`/api/scans/${id}`);
}

export function pauseScan(id: string): Promise<ScanStatus> {
  return apiFetch(`/api/scans/${id}/pause`, { method: "POST" });
}

export function resumeScan(id: string): Promise<ScanStatus> {
  return apiFetch(`/api/scans/${id}/resume`, { method: "POST" });
}

export function stopScan(id: string): Promise<ScanStatus> {
  return apiFetch(`/api/scans/${id}/stop`, { method: "POST" });
}

/** GET .../findings — the authoritative findings list (pre-Phase-6 gap fix):
 * lets a WS-reconnect resync rebuild the tree/findings from something more
 * than ScanStatus's bare count. */
export function getFindings(id: string): Promise<EngineFinding[]> {
  return apiFetch(`/api/scans/${id}/findings`);
}

/** PATCH .../{id}: only send the fields that actually changed — the rest
 * (spec §6: "pointer fields = only send what changed") come across as
 * `undefined` and JSON.stringify drops them, matching the Go side's nil
 * pointer / "leave unchanged" convention. */
export function adjustScan(id: string, req: AdjustRequest): Promise<ScanStatus> {
  return apiFetch(`/api/scans/${id}`, { method: "PATCH", body: JSON.stringify(req) });
}

function pattern(id: string, action: "pin" | "exclude" | "boost" | "demote", req: PatternRequest): Promise<{ status: string }> {
  return apiFetch(`/api/scans/${id}/${action}`, json(req));
}
export const pinPattern = (id: string, req: PatternRequest) => pattern(id, "pin", req);
export const excludePattern = (id: string, req: PatternRequest) => pattern(id, "exclude", req);
export const boostPattern = (id: string, req: PatternRequest) => pattern(id, "boost", req);
export const demotePattern = (id: string, req: PatternRequest) => pattern(id, "demote", req);

export function injectTerms(id: string, req: InjectRequest): Promise<{ status: string }> {
  return apiFetch(`/api/scans/${id}/inject`, json(req));
}

export function saveScan(id: string, req: SaveRequest = {}): Promise<{ id: string; path: string }> {
  return apiFetch(`/api/scans/${id}/save`, json(req));
}

export function listSessions(): Promise<SessionMeta[]> {
  return apiFetch("/api/sessions");
}

export function resumeSession(id: string): Promise<{ id: string }> {
  return apiFetch(`/api/sessions/${id}/resume`, { method: "POST" });
}

/** Heavy (whole frontier/baselines/learners) — fetch lazily, never poll (spec §8). */
export function getSession(id: string): Promise<SessionState> {
  return apiFetch(`/api/sessions/${id}`);
}
