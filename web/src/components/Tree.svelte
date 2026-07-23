<script lang="ts">
  import { fade } from "svelte/transition";
  import { classifyStatus } from "../lib/status";
  import type { StoreState, TreeNode } from "../lib/store";
  import ProvenanceTag from "./ProvenanceTag.svelte";

  let { data, onPick }: { data: StoreState; onPick: (pattern: string) => void } = $props();

  let expanded = $state<Set<string>>(new Set([""]));
  function toggle(path: string) {
    const next = new Set(expanded);
    if (next.has(path)) next.delete(path);
    else next.add(path);
    expanded = next;
  }

  function pathnameOf(url: string): string {
    try {
      return new URL(url).pathname;
    } catch {
      return url;
    }
  }

</script>

{#snippet node(n: TreeNode, depth: number)}
  {@const hasChildren = n.children.size > 0}
  {#if n.path !== ""}
    <div class="dir-row" style="padding-left:{depth * 14}px">
      {#if hasChildren}
        <button type="button" class="caret" onclick={() => toggle(n.path)} aria-label={expanded.has(n.path) ? "collapse" : "expand"}>
          {expanded.has(n.path) ? "▾" : "▸"}
        </button>
      {:else}
        <span class="caret-spacer"></span>
      {/if}
      <button type="button" class="dir-name mono" onclick={() => onPick(n.path)} title="click to prefill an override pattern">
        {n.name}/
      </button>
      <span class="dir-count">{n.findings.length}</span>
    </div>
  {/if}
  {#if n.path === "" || expanded.has(n.path)}
    <ul class="findings" style="padding-left:{(depth + 1) * 14}px">
      {#each n.findings as f (f.url)}
        <li class="finding" class:alias={f.isAlias} transition:fade={{ duration: 200 }}>
          <button type="button" class="url mono" onclick={() => onPick(pathnameOf(f.url))} title={f.url}>
            {pathnameOf(f.url).split("/").pop() || "/"}
          </button>
          <span class="status-badge {classifyStatus(f.status)}">{f.status || "—"}</span>
          <span class="confidence mono">{(f.confidence * 100).toFixed(0)}%</span>
          <ProvenanceTag raw={f.provenance} />
          {#if f.isAlias}<span class="alias-tag">alias</span>{/if}
        </li>
      {/each}
    </ul>
    {#each [...n.children.values()] as child (child.path)}
      {@render node(child, depth + 1)}
    {/each}
  {/if}
{/snippet}

<section class="tree-panel">
  <h2>Discovered paths</h2>
  <div class="tree-scroll">
    {@render node(data.tree, 0)}
    {#if data.tree.findings.length === 0 && data.tree.children.size === 0}
      <p class="empty">no confirmed findings yet</p>
    {/if}
  </div>
</section>

<style>
  .tree-panel {
    display: flex;
    flex-direction: column;
    min-height: 0;
    padding: 8px;
  }
  .tree-scroll {
    overflow-y: auto;
    min-height: 0;
    flex: 1;
  }
  .dir-row {
    display: flex;
    align-items: center;
    gap: 4px;
    padding: 2px 4px;
  }
  .caret,
  .caret-spacer {
    width: 14px;
    flex: none;
    background: none;
    border: none;
    padding: 0;
    color: var(--text-muted);
  }
  .dir-name {
    background: none;
    border: none;
    padding: 0;
    color: var(--text-primary);
    font-weight: 600;
    font-size: 12px;
  }
  .dir-count {
    color: var(--text-muted);
    font-size: 10px;
  }
  .findings {
    list-style: none;
    margin: 0;
    padding-block: 0;
  }
  .finding {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 1px 4px;
    font-size: 11px;
  }
  .finding.alias {
    opacity: 0.5;
  }
  .url {
    background: none;
    border: none;
    padding: 0;
    color: var(--text-primary);
    font-size: 11px;
  }
  .confidence {
    color: var(--text-secondary);
    flex: none;
  }
  .alias-tag {
    color: var(--text-muted);
    font-size: 9px;
    text-transform: uppercase;
  }
  .empty {
    color: var(--text-muted);
    padding: 12px;
    text-align: center;
  }
</style>
