<script lang="ts">
  import type { ConnState } from "../lib/connection";
  import { formatDuration } from "../lib/format";
  import type { StoreState } from "../lib/store";

  let { data, connState }: { data: StoreState; connState: ConnState } = $props();

  const stateLabel: Record<string, string> = { running: "running", paused: "paused", stopped: "stopped", finished: "finished" };
  const connLabel: Record<ConnState, string> = { connected: "connected", connecting: "connecting…", reconnecting: "reconnecting…", closed: "closed" };
</script>

<header class="header">
  <div class="row">
    <span class="target mono">{data.lifecycle.target ?? "—"}</span>
    <span class="badge state-{data.lifecycle.state ?? 'unknown'}">{stateLabel[data.lifecycle.state ?? ""] ?? "—"}</span>
    <span class="sep">·</span>
    <span class="field">seed <span class="mono">{data.lifecycle.seed ?? "—"}</span></span>
    <span class="sep">·</span>
    <span class="field">elapsed <span class="mono">{formatDuration(data.gauges.elapsedMs)}</span></span>
    {#if data.lifecycle.mode}
      <span class="sep">·</span>
      <span class="field" title="inert until Phase 6 (stealth) — accepted and echoed back, no behavioral effect yet">
        mode <span class="mono">{data.lifecycle.mode}</span> <span class="reserved">(reserved)</span>
      </span>
    {/if}
    <span class="spacer"></span>
    <span class="conn conn-{connState}">
      <span class="dot"></span>
      {connLabel[connState]}
    </span>
  </div>

  {#if data.resync}
    <div class="resync-note">
      reconnected — resynced {data.resync.rebuiltFindings} finding{data.resync.rebuiltFindings === 1 ? "" : "s"} from the server
    </div>
  {/if}

  {#if data.spaPivot.fired}
    <div class="pivot-banner">
      <strong>Single-page app detected</strong> — pivoting to JS endpoint harvesting
      {#if data.spaPivot.url}<span class="mono">({data.spaPivot.url})</span>{/if}
    </div>
  {/if}
</header>

<style>
  .header {
    border-bottom: 1px solid var(--border);
    background: var(--surface-1);
    padding: 8px 14px;
    font-size: 12px;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .target {
    font-weight: 600;
    color: var(--text-primary);
  }
  .sep {
    color: var(--border-strong);
  }
  .field {
    color: var(--text-secondary);
  }
  .reserved {
    color: var(--text-muted);
    font-style: italic;
  }
  .spacer {
    flex: 1;
  }
  .badge {
    font-family: var(--mono);
    font-size: 11px;
    padding: 1px 6px;
    border-radius: 3px;
    border: 1px solid var(--border-strong);
    color: var(--text-secondary);
  }
  .badge.state-running {
    color: var(--good);
    border-color: var(--good);
  }
  .badge.state-paused {
    color: var(--warning);
    border-color: var(--warning);
  }
  .badge.state-stopped,
  .badge.state-finished {
    color: var(--text-muted);
  }
  .conn {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    color: var(--text-secondary);
  }
  .conn .dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: var(--text-muted);
  }
  .conn-connected .dot {
    background: var(--good);
  }
  .conn-reconnecting .dot,
  .conn-connecting .dot {
    background: var(--warning);
    animation: pulse 1s ease-in-out infinite;
  }
  .conn-closed .dot {
    background: var(--critical);
  }
  @keyframes pulse {
    50% {
      opacity: 0.35;
    }
  }
  .resync-note {
    margin-top: 6px;
    color: var(--good);
    font-size: 11px;
  }
  .pivot-banner {
    margin-top: 8px;
    padding: 8px 12px;
    background: var(--accent-bg);
    border: 1px solid var(--accent);
    border-radius: 4px;
    color: var(--text-primary);
    font-size: 13px;
  }
</style>
