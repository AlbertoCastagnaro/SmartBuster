// connection.ts — the handshake + transport primitive (spec §3): token from
// the URL fragment, WS via the Sec-WebSocket-Protocol subprotocol, Bearer on
// REST, auto-reconnect with backoff, and a resync hook the store consumes
// after any reconnect (the stream is lossy — never assume continuity).
//
// Framework-thin on purpose (plain TS, no Svelte import) so it can be
// exercised or mocked independently of the component tree, matching
// store.ts's own headless-testability requirement.
import type { ScanStatus, WireEvent } from "./wire";

/** Reads the per-session token from the URL fragment (`#token=<64-hex>`).
 * Never sent to the server as part of the URL — only via Authorization/
 * Sec-WebSocket-Protocol — so it never appears in server logs. */
export function getToken(): string {
  const match = /(?:^|[#&])token=([0-9a-f]+)/.exec(window.location.hash);
  return match?.[1] ?? "";
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

// A control-plane call against a scan whose Coordinator.Run has already
// returned (state finished/stopped) hangs forever server-side: nothing
// reads controlCh once the dispatch loop has exited, so
// Coordinator.SubmitControl blocks on the channel send with no timeout,
// and the HTTP request never completes (confirmed against a real daemon —
// see the phase 5b handoff's protocol-gap note). A client-side timeout is
// the only way this UI avoids hanging in "busy" forever when a user
// pauses/pins/saves a scan that finished moments before the click.
const CONTROL_CALL_TIMEOUT_MS = 10_000;

/** Fetch wrapper adding the Bearer token to every REST call. Origin is set
 * automatically by the browser on same-origin non-GET requests (it's a
 * forbidden header — fetch() can't override it), which is exactly what the
 * daemon's Origin check expects from a real browser client (spec §5). */
export async function apiFetch<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch(path, {
    ...init,
    signal: init.signal ?? AbortSignal.timeout(CONTROL_CALL_TIMEOUT_MS),
    headers: {
      ...(init.body ? { "Content-Type": "application/json" } : {}),
      Authorization: `Bearer ${getToken()}`,
      ...init.headers,
    },
  });
  if (!res.ok) {
    let message = res.statusText;
    try {
      const body = await res.json();
      if (typeof body?.error === "string") message = body.error;
    } catch {
      // non-JSON error body; fall back to statusText
    }
    throw new ApiError(res.status, message);
  }
  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}

export type ConnState = "connecting" | "connected" | "reconnecting" | "closed";

const BACKOFF_BASE_MS = 500;
const BACKOFF_MAX_MS = 10_000;

export interface ScanConnection {
  getState(): ConnState;
  onState(cb: (s: ConnState) => void): () => void;
  onEvent(cb: (ev: WireEvent) => void): () => void;
  /** Fires after every successful (re)connect with the scan's current REST
   * status — `isReconnect` is false only for the very first connection. */
  onResync(cb: (status: ScanStatus, isReconnect: boolean) => void): () => void;
  close(): void;
}

/** Opens (and, on drop, re-opens) the WS event stream for one scan id. */
export function connectScanEvents(scanId: string): ScanConnection {
  const stateListeners = new Set<(s: ConnState) => void>();
  const eventListeners = new Set<(ev: WireEvent) => void>();
  const resyncListeners = new Set<(status: ScanStatus, isReconnect: boolean) => void>();

  let state: ConnState = "connecting";
  let closedByCaller = false;
  let hasConnectedOnce = false;
  let attempt = 0;
  let ws: WebSocket | null = null;
  let retryTimer: ReturnType<typeof setTimeout> | null = null;

  function setState(s: ConnState) {
    state = s;
    for (const cb of stateListeners) cb(s);
  }

  async function resync(isReconnect: boolean) {
    try {
      const status = await apiFetch<ScanStatus>(`/api/scans/${scanId}`);
      for (const cb of resyncListeners) cb(status, isReconnect);
    } catch {
      // Best-effort: a resync failure doesn't tear down the socket — the
      // next status poll (or the user re-opening the tab) will retry.
    }
  }

  function wsURL(): string {
    const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
    return `${proto}//${window.location.host}/api/scans/${scanId}/events`;
  }

  function open() {
    setState(hasConnectedOnce ? "reconnecting" : "connecting");
    const socket = new WebSocket(wsURL(), [getToken()]);
    ws = socket;

    socket.onopen = () => {
      attempt = 0;
      setState("connected");
      void resync(hasConnectedOnce);
      hasConnectedOnce = true;
    };
    socket.onmessage = (msg) => {
      let ev: WireEvent;
      try {
        ev = JSON.parse(msg.data as string);
      } catch {
        return;
      }
      for (const cb of eventListeners) cb(ev);
    };
    socket.onclose = () => {
      if (closedByCaller) {
        setState("closed");
        return;
      }
      scheduleReconnect();
    };
    socket.onerror = () => {
      socket.close();
    };
  }

  function scheduleReconnect() {
    setState("reconnecting");
    const delay = Math.min(BACKOFF_BASE_MS * 2 ** attempt, BACKOFF_MAX_MS);
    attempt++;
    retryTimer = setTimeout(open, delay);
  }

  open();

  return {
    getState: () => state,
    onState(cb) {
      stateListeners.add(cb);
      return () => stateListeners.delete(cb);
    },
    onEvent(cb) {
      eventListeners.add(cb);
      return () => eventListeners.delete(cb);
    },
    onResync(cb) {
      resyncListeners.add(cb);
      return () => resyncListeners.delete(cb);
    },
    close() {
      closedByCaller = true;
      if (retryTimer) clearTimeout(retryTimer);
      ws?.close();
      setState("closed");
    },
  };
}
