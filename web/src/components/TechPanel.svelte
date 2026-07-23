<script lang="ts">
  // spec §4.5: detected tech with confidence bars, edge vs backend layer
  // separation, and the WAF banner. After a session resume, RuleIDs may be
  // empty (profile.TargetProfile's per-vote rule-id provenance is an
  // unexported field that doesn't round-trip — spec 5a §10) — rendered
  // gracefully below, never assumed present.
  import type { StoreState } from "../lib/store";
  import type { TechEntry } from "../lib/wire";

  let { data }: { data: StoreState } = $props();

  let backend = $derived(data.tech.techs.filter((t) => t.Layer === "backend" || t.Layer === "unknown"));
  let edge = $derived(data.tech.techs.filter((t) => t.Layer === "edge"));

  function confPct(t: TechEntry): number {
    return Math.round(t.Confidence * 100);
  }
</script>

{#snippet techRow(t: TechEntry)}
  <li class="tech-row">
    <div class="tech-head">
      <span class="name">{t.Name}</span>
      {#if t.Version}<span class="version mono">{t.Version}</span>{/if}
      <span class="category">{t.Category}</span>
      <span class="conf-num mono">{confPct(t)}%</span>
    </div>
    <div class="conf-bar-wrap"><div class="conf-bar" style="width:{confPct(t)}%"></div></div>
    <div class="meta">
      {#if t.Sources?.length}<span class="sources">via {t.Sources.join(", ")}</span>{/if}
      {#if t.RuleIDs?.length}<span class="rules mono">{t.RuleIDs.join(", ")}</span>{/if}
    </div>
  </li>
{/snippet}

<section class="tech-panel">
  <h2>Tech profile</h2>
  {#if data.tech.waf}
    <div class="waf-banner">WAF detected: <strong>{data.tech.waf}</strong></div>
  {/if}
  <div class="layers">
    <div class="layer">
      <h3>Backend</h3>
      {#if backend.length === 0}
        <p class="empty">nothing resolved yet</p>
      {:else}
        <ul>{#each backend as t (t.Name + t.Category)}{@render techRow(t)}{/each}</ul>
      {/if}
    </div>
    <div class="layer">
      <h3>Edge</h3>
      {#if edge.length === 0}
        <p class="empty">nothing resolved yet</p>
      {:else}
        <ul>{#each edge as t (t.Name + t.Category)}{@render techRow(t)}{/each}</ul>
      {/if}
    </div>
  </div>
</section>

<style>
  .tech-panel {
    padding: 8px;
    overflow-y: auto;
  }
  .waf-banner {
    background: rgba(var(--critical-rgb), 0.12);
    border: 1px solid var(--critical);
    color: var(--text-primary);
    padding: 6px 10px;
    border-radius: 4px;
    font-size: 12px;
    margin-bottom: 8px;
  }
  .layers {
    display: flex;
    gap: 16px;
  }
  .layer {
    flex: 1;
    min-width: 0;
  }
  h3 {
    font-size: 11px;
    color: var(--text-muted);
    text-transform: uppercase;
    margin-bottom: 4px;
  }
  ul {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .tech-row {
    padding: 4px 0;
    border-bottom: 1px solid var(--border);
  }
  .tech-head {
    display: flex;
    align-items: baseline;
    gap: 6px;
    font-size: 12px;
  }
  .name {
    font-weight: 600;
    color: var(--text-primary);
  }
  .version {
    color: var(--text-secondary);
  }
  .category {
    color: var(--text-muted);
    font-size: 10px;
    margin-left: auto;
  }
  .conf-num {
    color: var(--text-secondary);
    font-size: 10px;
  }
  .conf-bar-wrap {
    height: 4px;
    background: var(--surface-2);
    border-radius: 2px;
    margin: 3px 0;
    overflow: hidden;
  }
  .conf-bar {
    height: 100%;
    background: var(--prov-corpus);
  }
  .meta {
    display: flex;
    gap: 8px;
    font-size: 10px;
    color: var(--text-muted);
  }
  .empty {
    color: var(--text-muted);
    font-size: 11px;
  }
</style>
