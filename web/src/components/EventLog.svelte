<script lang="ts">
  import type { EventCategory } from "../lib/wire";
  import type { LogEntry, StoreState } from "../lib/store";

  let { data }: { data: StoreState } = $props();

  const ALL_CATEGORIES: EventCategory[] = ["scan", "calibration", "discovery", "tech", "trap", "telemetry", "warning", "error", "control"];

  let activeCategories = $state<Set<EventCategory>>(new Set(ALL_CATEGORIES));

  function toggle(cat: EventCategory) {
    const next = new Set(activeCategories);
    if (next.has(cat)) next.delete(cat);
    else next.add(cat);
    activeCategories = next;
  }

  // Detection -> cut pairing (spec §4.6): a branch.pruned right after the
  // trap.detected that caused it, same dir, rendered as a linked pair.
  // Computed over the *unfiltered* log so filtering never breaks adjacency.
  interface Row {
    entry: LogEntry;
    pairedCut: boolean;
  }
  let rows = $derived.by((): Row[] =>
    data.log.map((entry, i) => {
      const prev = data.log[i - 1];
      const pairedCut = entry.type === "branch.pruned" && prev?.type === "trap.detected" && prev.dir === entry.dir;
      return { entry, pairedCut };
    }),
  );

  let visible = $derived(rows.filter((r) => activeCategories.has(r.entry.category)).slice(-500).reverse());

  let counts = $derived.by(() => {
    const c = new Map<EventCategory, number>();
    for (const entry of data.log) c.set(entry.category, (c.get(entry.category) ?? 0) + 1);
    return c;
  });
</script>

<section class="log-panel">
  <div class="filters">
    {#each ALL_CATEGORIES as cat (cat)}
      {@const n = counts.get(cat) ?? 0}
      <button type="button" class="filter cat-{cat}" class:active={activeCategories.has(cat)} onclick={() => toggle(cat)} disabled={n === 0}>
        {cat} <span class="count">{n}</span>
      </button>
    {/each}
  </div>
  <ul class="rows">
    {#each visible as { entry, pairedCut } (entry.seq)}
      <li class="row cat-{entry.category}" class:paired={pairedCut}>
        <span class="time mono">{new Date(entry.time).toLocaleTimeString()}</span>
        <span class="cat-badge cat-{entry.category}">{entry.category}</span>
        <span class="type mono">{entry.type}</span>
        {#if entry.source}<span class="facet">source={entry.source}</span>{/if}
        {#if entry.kind}<span class="facet">kind={entry.kind}</span>{/if}
        {#if entry.dir}<span class="facet mono">{entry.dir}</span>{/if}
        {#if entry.message}<span class="message">{entry.message}</span>{/if}
        {#if pairedCut}<span class="cut-marker" title="cut following the trap detected just above">↳ cut</span>{/if}
      </li>
    {/each}
    {#if visible.length === 0}
      <li class="empty">no events in the selected categories yet</li>
    {/if}
  </ul>
</section>

<style>
  .log-panel {
    display: flex;
    flex-direction: column;
    min-height: 0;
    height: 100%;
  }
  .filters {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    padding: 6px 8px;
    border-bottom: 1px solid var(--border);
  }
  .filter {
    font-size: 10px;
    padding: 2px 6px;
    background: transparent;
    opacity: 0.45;
  }
  .filter.active {
    opacity: 1;
    background: var(--surface-2);
  }
  .filter .count {
    color: var(--text-muted);
  }
  .rows {
    list-style: none;
    margin: 0;
    padding: 0;
    overflow-y: auto;
    flex: 1;
    min-height: 0;
    font-size: 11px;
  }
  .row {
    display: flex;
    gap: 6px;
    align-items: baseline;
    padding: 3px 8px;
    border-bottom: 1px solid var(--border);
  }
  .row.paired {
    background: rgba(var(--critical-rgb), 0.06);
  }
  .time {
    color: var(--text-muted);
    flex: none;
  }
  .cat-badge {
    flex: none;
    font-size: 9px;
    text-transform: uppercase;
    padding: 1px 4px;
    border-radius: 2px;
    color: var(--text-secondary);
    border: 1px solid var(--border-strong);
  }
  .cat-badge.cat-error,
  .filter.cat-error {
    color: var(--critical);
  }
  .cat-badge.cat-warning,
  .filter.cat-warning {
    color: var(--warning);
  }
  .cat-badge.cat-trap,
  .filter.cat-trap {
    color: var(--serious);
  }
  .type {
    color: var(--text-secondary);
    flex: none;
  }
  .facet {
    color: var(--text-muted);
    flex: none;
  }
  .message {
    color: var(--text-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .cut-marker {
    color: var(--critical);
    flex: none;
    margin-left: auto;
  }
  .empty {
    padding: 12px;
    color: var(--text-muted);
    text-align: center;
  }
</style>
