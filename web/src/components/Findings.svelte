<script lang="ts">
  // spec §4.7: the flat confirmed-findings list, dedup/alias-aware,
  // sortable, filterable by provenance, with a copy/export affordance.
  //
  // Note: 5a has no REST route that serves a findings export (only the CLI
  // writes to OutDir via internal/output; Burp/markdown exports are
  // explicitly Phase 7/plan-level per spec §1 "Out"). This exports exactly
  // what the live client has accumulated from the WS stream — a client-side
  // snapshot, not a server-authoritative one.
  import { parseProvenance } from "../lib/provenance";
  import type { Finding, StoreState } from "../lib/store";
  import { classifyStatus } from "../lib/status";
  import ProvenanceTag from "./ProvenanceTag.svelte";

  let { data }: { data: StoreState } = $props();

  type SortKey = "confidence" | "time" | "size";
  let sortKey = $state<SortKey>("confidence");
  let showAliases = $state(false);
  let provenanceFilter = $state<string>("all");

  let provenanceOptions = $derived.by(() => {
    const set = new Set<string>();
    for (const f of data.findings) for (const tag of parseProvenance(f.provenance)) set.add(tag.category);
    return ["all", ...set];
  });

  let rows = $derived.by((): Finding[] => {
    let list = data.findings;
    if (!showAliases) list = list.filter((f) => !f.isAlias);
    if (provenanceFilter !== "all") list = list.filter((f) => parseProvenance(f.provenance).some((t) => t.category === provenanceFilter));
    list = [...list];
    if (sortKey === "confidence") list.sort((a, b) => b.confidence - a.confidence);
    else if (sortKey === "size") list.sort((a, b) => b.size - a.size);
    else list.sort((a, b) => (a.time < b.time ? 1 : -1));
    return list;
  });

  let copied = $state(false);
  async function copyJSON() {
    await navigator.clipboard.writeText(JSON.stringify(rows, null, 2));
    copied = true;
    setTimeout(() => (copied = false), 1500);
  }

  function downloadJSON() {
    const blob = new Blob([JSON.stringify(rows, null, 2)], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `${data.lifecycle.id ?? "scan"}-findings.json`;
    a.click();
    URL.revokeObjectURL(url);
  }
</script>

<section class="findings-panel">
  <div class="toolbar">
    <h2>Findings</h2>
    <select bind:value={sortKey} aria-label="sort by">
      <option value="confidence">confidence</option>
      <option value="time">newest first</option>
      <option value="size">size</option>
    </select>
    <select bind:value={provenanceFilter} aria-label="filter by provenance">
      {#each provenanceOptions as opt (opt)}
        <option value={opt}>{opt}</option>
      {/each}
    </select>
    <label class="alias-toggle"><input type="checkbox" bind:checked={showAliases} /> show aliases</label>
    <span class="spacer"></span>
    <button type="button" onclick={copyJSON}>{copied ? "copied" : "copy JSON"}</button>
    <button type="button" onclick={downloadJSON}>download</button>
  </div>
  <ul class="rows">
    {#each rows as f (f.url)}
      <li class="row">
        <a class="url mono" href={f.url} target="_blank" rel="noreferrer">{f.url}</a>
        <span class="status-badge {classifyStatus(f.status)}">{f.status || "—"}</span>
        <span class="size mono">{f.size}B</span>
        <span class="confidence mono">{(f.confidence * 100).toFixed(0)}%</span>
        <ProvenanceTag raw={f.provenance} />
      </li>
    {/each}
    {#if rows.length === 0}
      <li class="empty">no findings match the current filters</li>
    {/if}
  </ul>
</section>

<style>
  .findings-panel {
    display: flex;
    flex-direction: column;
    min-height: 0;
    padding: 8px;
  }
  .toolbar {
    display: flex;
    align-items: center;
    gap: 6px;
    margin-bottom: 6px;
    flex-wrap: wrap;
  }
  .alias-toggle {
    font-size: 11px;
    color: var(--text-secondary);
    display: flex;
    align-items: center;
    gap: 4px;
  }
  .spacer {
    flex: 1;
  }
  .rows {
    list-style: none;
    margin: 0;
    padding: 0;
    overflow-y: auto;
    min-height: 0;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 3px 4px;
    font-size: 11px;
    border-bottom: 1px solid var(--border);
  }
  .url {
    color: var(--text-primary);
    flex: 1 1 auto;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    text-decoration: none;
  }
  .url:hover {
    text-decoration: underline;
  }
  .confidence {
    color: var(--text-secondary);
    flex: none;
  }
  .size {
    color: var(--text-muted);
    flex: none;
    font-size: 10px;
  }
  .empty {
    padding: 12px;
    color: var(--text-muted);
    text-align: center;
  }
</style>
