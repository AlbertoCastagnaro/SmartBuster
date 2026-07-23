<script lang="ts">
  // The centerpiece (spec §4.4): the top-25 candidates about to be tried,
  // reordering live as the engine reprioritizes — this is the visualization
  // that makes "smart" legible. `animate:flip` gives every reorder a smooth
  // FLIP transition instead of a jarring reflow (spec §9: "calm under high
  // update rates"); `transition:fade` softens rows entering/leaving the
  // top-K as candidates get promoted in or dispatched out.
  import { flip } from "svelte/animate";
  import { fade } from "svelte/transition";
  import type { StoreState } from "../lib/store";
  import ProvenanceTag from "./ProvenanceTag.svelte";

  let { data, onPick }: { data: StoreState; onPick: (pattern: string) => void } = $props();

  let topScore = $derived(data.frontier.topK[0]?.score ?? 1);

  function barWidth(score: number): number {
    if (topScore <= 0) return 0;
    // Clamp to a visible floor so a single pinned outlier (score 1000,
    // spec §4.1 PinScore) doesn't visually erase every other candidate's bar.
    return Math.max(4, Math.min(100, (score / topScore) * 100));
  }
</script>

<section class="frontier-panel">
  <div class="frontier-head">
    <h2>Priority frontier</h2>
    <span class="total">{data.frontier.total} candidates queued</span>
  </div>
  <ol class="rows">
    {#each data.frontier.topK as row, i (row.dir + "/" + row.path)}
      <li class="row" animate:flip={{ duration: 350 }} transition:fade={{ duration: 200 }}>
        <span class="rank mono">{i + 1}</span>
        <button type="button" class="path mono" onclick={() => onPick((row.dir || "") + "/" + row.path)} title={(row.dir || "") + "/" + row.path}>
          {#if row.dir}<span class="dir-part">{row.dir}/</span>{/if}{row.path}
        </button>
        <ProvenanceTag raw={row.provenance} />
        <span class="depth mono" title="depth">d{row.depth}</span>
        <span class="score-wrap">
          <span class="score-bar" style="width:{barWidth(row.score)}%"></span>
          <span class="score-num mono">{row.score.toFixed(2)}</span>
        </span>
      </li>
    {/each}
    {#if data.frontier.topK.length === 0}
      <li class="empty">frontier is empty — waiting for the first snapshot</li>
    {/if}
  </ol>
</section>

<style>
  .frontier-panel {
    display: flex;
    flex-direction: column;
    min-height: 0;
    padding: 8px;
  }
  .frontier-head {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    margin-bottom: 4px;
  }
  .total {
    font-size: 10px;
    color: var(--text-muted);
  }
  .rows {
    list-style: none;
    margin: 0;
    padding: 0;
    overflow-y: auto;
    min-height: 0;
    flex: 1;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 3px 4px;
    font-size: 11px;
    border-bottom: 1px solid var(--border);
  }
  .rank {
    width: 20px;
    flex: none;
    color: var(--text-muted);
    text-align: right;
  }
  .path {
    background: none;
    border: none;
    padding: 0;
    color: var(--text-primary);
    flex: 1 1 auto;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    text-align: left;
  }
  .dir-part {
    color: var(--text-muted);
  }
  .depth {
    color: var(--text-muted);
    flex: none;
    font-size: 10px;
  }
  .score-wrap {
    position: relative;
    flex: none;
    width: 84px;
    height: 14px;
    background: var(--surface-2);
    border-radius: 2px;
    overflow: hidden;
    display: flex;
    align-items: center;
  }
  .score-bar {
    position: absolute;
    inset: 0 auto 0 0;
    background: var(--accent-bg);
    border-right: 1px solid var(--accent);
  }
  .score-num {
    position: relative;
    margin-left: auto;
    margin-right: 4px;
    font-size: 10px;
    color: var(--text-primary);
  }
  .empty {
    padding: 12px;
    color: var(--text-muted);
    text-align: center;
  }
</style>
