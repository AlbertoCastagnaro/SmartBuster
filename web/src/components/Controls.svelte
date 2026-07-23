<script lang="ts">
  // spec §6: buttons/forms map 1:1 to 5a routes. Optimistic UI is fine, but
  // every call reconciles against the ScanStatus the route returns
  // (onStatus, wired by the parent into the store's own lifecycle merge —
  // see store.ts's applyResync). `mode` is shown read-only — inert until
  // Phase 6 (stealth); never a live behavior toggle here.
  import {
    adjustScan,
    boostPattern,
    demotePattern,
    excludePattern,
    injectTerms,
    pauseScan,
    pinPattern,
    resumeScan,
    saveScan,
    stopScan,
  } from "../lib/rest";
  import type { StoreState } from "../lib/store";
  import type { ScanStatus } from "../lib/wire";

  let {
    scanId,
    data,
    prefillPattern = $bindable(""),
    onStatus,
  }: {
    scanId: string;
    data: StoreState;
    prefillPattern?: string;
    onStatus: (status: ScanStatus) => void;
  } = $props();

  let busy = $state(false);
  let lastError = $state("");

  async function run(fn: () => Promise<ScanStatus | { status: string } | unknown>) {
    busy = true;
    lastError = "";
    try {
      const result = await fn();
      if (result && typeof result === "object" && "state" in result) onStatus(result as ScanStatus);
    } catch (e) {
      // A timeout here almost always means the scan already finished: the
      // daemon's controlCh has no reader once Coordinator.Run has returned,
      // so the request hangs server-side with no error (see connection.ts's
      // CONTROL_CALL_TIMEOUT_MS note) — surfaced with that context rather
      // than a bare "signal timed out".
      lastError =
        e instanceof DOMException && e.name === "TimeoutError"
          ? "timed out — the scan may have already finished (finished/stopped scans don't accept control commands)"
          : e instanceof Error
            ? e.message
            : String(e);
    } finally {
      busy = false;
    }
  }

  const scanState = $derived(data.lifecycle.state);

  let rateInput = $state("");
  let concurrencyInput = $state("");
  function submitAdjust() {
    const rate = rateInput.trim() === "" ? undefined : Number(rateInput);
    const concurrency = concurrencyInput.trim() === "" ? undefined : Number(concurrencyInput);
    if (rate === undefined && concurrency === undefined) return;
    run(() => adjustScan(scanId, { rate, concurrency }));
  }

  let factor = $state(2);
  function override(action: "pin" | "exclude" | "boost" | "demote") {
    if (!prefillPattern.trim()) return;
    const req = { pattern: prefillPattern.trim(), factor };
    const fn = { pin: pinPattern, exclude: excludePattern, boost: boostPattern, demote: demotePattern }[action];
    run(() => fn(scanId, req));
  }

  let injectInput = $state("");
  function submitInject() {
    const terms = injectInput
      .split(/[\n,]/)
      .map((t) => t.trim())
      .filter(Boolean);
    if (terms.length === 0) return;
    run(() => injectTerms(scanId, { terms })).then(() => (injectInput = ""));
  }

  let saveNote = $state("");
  async function save() {
    busy = true;
    try {
      const { path } = await saveScan(scanId);
      saveNote = `saved to ${path}`;
    } catch (e) {
      lastError = e instanceof Error ? e.message : String(e);
    } finally {
      busy = false;
      setTimeout(() => (saveNote = ""), 3000);
    }
  }
</script>

<section class="controls">
  <h2>Controls</h2>

  <div class="group lifecycle">
    <button type="button" disabled={busy || scanState !== "running"} onclick={() => run(() => pauseScan(scanId))}>pause</button>
    <button type="button" disabled={busy || scanState !== "paused"} onclick={() => run(() => resumeScan(scanId))}>resume</button>
    <button type="button" disabled={busy || scanState === "stopped" || scanState === "finished"} onclick={() => run(() => stopScan(scanId))}>stop</button>
    <button type="button" disabled={busy} onclick={save}>save session</button>
    {#if saveNote}<span class="save-note">{saveNote}</span>{/if}
    <span class="mode-readout" title="takes effect in stealth builds (Phase 6) — read-only here">
      mode: <span class="mono">{data.lifecycle.mode || "—"}</span>
    </span>
  </div>

  <div class="group adjust">
    <label>rate <input type="number" min="0" step="0.1" placeholder="unchanged" bind:value={rateInput} /></label>
    <label>concurrency <input type="number" min="1" step="1" placeholder="unchanged" bind:value={concurrencyInput} /></label>
    <button type="button" disabled={busy} onclick={submitAdjust}>adjust</button>
  </div>

  <div class="group override">
    <input class="pattern-input mono" placeholder="pattern (glob/prefix) — click a tree/frontier row to fill" bind:value={prefillPattern} />
    <label>factor <input type="number" step="0.1" bind:value={factor} /></label>
    <button type="button" disabled={busy} onclick={() => override("pin")}>pin</button>
    <button type="button" disabled={busy} onclick={() => override("exclude")}>exclude</button>
    <button type="button" disabled={busy} onclick={() => override("boost")}>boost</button>
    <button type="button" disabled={busy} onclick={() => override("demote")}>demote</button>
  </div>

  <div class="group inject">
    <input class="mono" placeholder="inject terms (comma/newline separated)" bind:value={injectInput} />
    <button type="button" disabled={busy} onclick={submitInject}>inject</button>
  </div>

  {#if lastError}
    <p class="error">{lastError}</p>
  {/if}
</section>

<style>
  .controls {
    padding: 8px;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .group {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
    font-size: 11px;
  }
  label {
    display: flex;
    align-items: center;
    gap: 4px;
    color: var(--text-secondary);
  }
  input[type="number"] {
    width: 64px;
  }
  .pattern-input,
  .inject input {
    flex: 1;
    min-width: 160px;
  }
  .save-note {
    color: var(--good);
    font-size: 10px;
  }
  .mode-readout {
    margin-left: auto;
    color: var(--text-muted);
  }
  .error {
    color: var(--critical);
    font-size: 11px;
  }
</style>
