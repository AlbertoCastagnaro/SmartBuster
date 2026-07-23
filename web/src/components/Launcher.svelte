<script lang="ts">
  // spec §8: a launcher form -> POST /api/scans with a PascalCase
  // engine.Config (durations as ns int64), plus a multi-scan list so
  // several can run/switch. Boolean knobs are sent explicitly rather than
  // left to the server's zero-value fallback (see ENGINE_CONFIG_DEFAULTS'
  // doc comment: the engine only defaults <=0 numerics, never bools).
  import { onMount } from "svelte";
  import { listScans, startScan } from "../lib/rest";
  import { ENGINE_CONFIG_DEFAULTS } from "../lib/wire";
  import type { ScanStatus } from "../lib/wire";

  let { onSelect }: { onSelect: (id: string) => void } = $props();

  let scans = $state<ScanStatus[]>([]);
  let loadError = $state("");
  async function refresh() {
    try {
      scans = await listScans();
    } catch (e) {
      loadError = e instanceof Error ? e.message : String(e);
    }
  }
  onMount(refresh);

  let target = $state("");
  let wordlist = $state("");
  let rate = $state(String(ENGINE_CONFIG_DEFAULTS.Rate));
  let concurrency = $state(String(ENGINE_CONFIG_DEFAULTS.Concurrency));
  let maxDepth = $state(String(ENGINE_CONFIG_DEFAULTS.MaxDepth));
  let nmapFile = $state("");
  let wayback = $state(ENGINE_CONFIG_DEFAULTS.Wayback);
  let crawl = $state(ENGINE_CONFIG_DEFAULTS.Crawl);
  let jsHarvest = $state(ENGINE_CONFIG_DEFAULTS.JSHarvest);
  let activeProbes = $state(ENGINE_CONFIG_DEFAULTS.ActiveProbes);
  let robots = $state(ENGINE_CONFIG_DEFAULTS.Robots);
  let sitemap = $state(ENGINE_CONFIG_DEFAULTS.Sitemap);

  let launching = $state(false);
  let launchError = $state("");

  async function launch() {
    if (!target.trim()) return;
    launching = true;
    launchError = "";
    try {
      const { id } = await startScan({
        Targets: [target.trim()],
        Wordlist: wordlist.trim() || undefined,
        Rate: Number(rate),
        Concurrency: Number(concurrency),
        MaxDepth: Number(maxDepth),
        NmapFile: nmapFile.trim() || undefined,
        RunNmap: false,
        ActiveProbes: activeProbes,
        FaviconProbe: ENGINE_CONFIG_DEFAULTS.FaviconProbe,
        Robots: robots,
        Sitemap: sitemap,
        Wayback: wayback,
        Crawl: crawl,
        JSHarvest: jsHarvest,
        Headless: ENGINE_CONFIG_DEFAULTS.Headless,
      });
      await refresh();
      onSelect(id);
    } catch (e) {
      launchError = e instanceof Error ? e.message : String(e);
    } finally {
      launching = false;
    }
  }
</script>

<section class="launcher">
  <h2>New scan</h2>
  <form onsubmit={(e) => (e.preventDefault(), launch())}>
    <label class="full">target <input class="mono" placeholder="http://example.com" bind:value={target} required /></label>
    <label class="full">wordlist path <input class="mono" placeholder="(blank = corpus)" bind:value={wordlist} /></label>
    <div class="row">
      <label>rate <input type="number" min="0" step="0.1" bind:value={rate} /></label>
      <label>concurrency <input type="number" min="1" step="1" bind:value={concurrency} /></label>
      <label>depth <input type="number" min="1" step="1" bind:value={maxDepth} /></label>
    </div>
    <label class="full">nmap -oX file <input class="mono" placeholder="(optional)" bind:value={nmapFile} /></label>
    <div class="toggles">
      <label><input type="checkbox" bind:checked={robots} /> robots</label>
      <label><input type="checkbox" bind:checked={sitemap} /> sitemap</label>
      <label><input type="checkbox" bind:checked={wayback} /> wayback</label>
      <label><input type="checkbox" bind:checked={crawl} /> crawl</label>
      <label><input type="checkbox" bind:checked={jsHarvest} /> js-harvest</label>
      <label><input type="checkbox" bind:checked={activeProbes} /> active-probes</label>
    </div>
    <button type="submit" disabled={launching || !target.trim()}>{launching ? "starting…" : "start scan"}</button>
    {#if launchError}<p class="error">{launchError}</p>{/if}
  </form>

  <h2>Active scans</h2>
  {#if loadError}<p class="error">{loadError}</p>{/if}
  <ul class="scan-list">
    {#each scans as s (s.id)}
      <li>
        <button type="button" class="scan-item" onclick={() => onSelect(s.id)}>
          <span class="mono target-name">{s.target}</span>
          <span class="badge">{s.state}</span>
          <span class="findings-count">{s.findings} findings</span>
        </button>
      </li>
    {/each}
    {#if scans.length === 0}
      <li class="empty">no scans yet</li>
    {/if}
  </ul>
  <button type="button" class="refresh" onclick={refresh}>refresh</button>
</section>

<style>
  .launcher {
    padding: 12px;
    display: flex;
    flex-direction: column;
    gap: 6px;
    overflow-y: auto;
  }
  form {
    display: flex;
    flex-direction: column;
    gap: 6px;
    margin-bottom: 12px;
  }
  label {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 11px;
    color: var(--text-secondary);
  }
  label.full input {
    flex: 1;
  }
  .row {
    display: flex;
    gap: 10px;
  }
  .row input {
    width: 64px;
  }
  .toggles {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
  }
  .scan-list {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .scan-item {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 6px;
    text-align: left;
    font-size: 11px;
    padding: 4px 6px;
  }
  .target-name {
    flex: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .badge {
    font-size: 10px;
    color: var(--text-muted);
  }
  .findings-count {
    font-size: 10px;
    color: var(--text-muted);
  }
  .empty {
    color: var(--text-muted);
    font-size: 11px;
    padding: 4px 6px;
  }
  .refresh {
    align-self: flex-start;
    font-size: 10px;
  }
  .error {
    color: var(--critical);
    font-size: 11px;
  }
</style>
