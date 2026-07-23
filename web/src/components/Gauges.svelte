<script lang="ts">
  import { formatDuration, formatETA, formatNumber, formatPercent } from "../lib/format";
  import type { StoreState } from "../lib/store";
  import Sparkline from "./Sparkline.svelte";

  let { data }: { data: StoreState } = $props();

  let reqPerSecHistory = $derived(data.sparkline.map((p) => p.reqPerSec));
  let hitRateHistory = $derived(data.sparkline.map((p) => p.hitRate));
  let inFlightHistory = $derived(data.sparkline.map((p) => p.inFlight));
</script>

<section class="gauges" aria-label="scan telemetry">
  <div class="tile">
    <span class="label">req/s</span>
    <span class="value mono">{formatNumber(data.gauges.reqPerSec)}</span>
    <Sparkline values={reqPerSecHistory} />
  </div>
  <div class="tile">
    <span class="label">hit rate</span>
    <span class="value mono">{formatPercent(data.gauges.hitRate)}</span>
    <Sparkline values={hitRateHistory} color="var(--good)" />
  </div>
  <div class="tile">
    <span class="label">in-flight</span>
    <span class="value mono">{data.gauges.inFlight}</span>
    <Sparkline values={inFlightHistory} color="var(--prov-nmap)" />
  </div>
  <div class="tile">
    <span class="label">requests sent</span>
    <span class="value mono">{formatNumber(data.gauges.reqSent, 0)}</span>
  </div>
  <div class="tile">
    <span class="label">hits</span>
    <span class="value mono">{data.gauges.hits}</span>
  </div>
  <div class="tile">
    <span class="label">frontier</span>
    <span class="value mono">{formatNumber(data.gauges.frontierLen, 0)}</span>
  </div>
  <div class="tile">
    <span class="label">dirs scanning</span>
    <span class="value mono">{data.gauges.dirsScanning}</span>
  </div>
  <div class="tile">
    <span class="label">elapsed</span>
    <span class="value mono">{formatDuration(data.gauges.elapsedMs)}</span>
  </div>
  <div class="tile">
    <span class="label">ETA</span>
    <span class="value mono">{formatETA(data.gauges.etaMs)}</span>
  </div>
</section>

<style>
  .gauges {
    display: flex;
    flex-wrap: wrap;
    gap: 1px;
    background: var(--border);
    border-bottom: 1px solid var(--border);
  }
  .tile {
    flex: 1 1 100px;
    background: var(--surface-1);
    padding: 8px 12px;
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 90px;
  }
  .label {
    font-size: 10px;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--text-muted);
  }
  .value {
    font-size: 16px;
    font-weight: 600;
    color: var(--text-primary);
  }
</style>
