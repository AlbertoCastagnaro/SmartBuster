<script lang="ts">
  // spec §8: session list/save/resume/download. Save lives in Controls.svelte
  // (it's scoped to a running scan, not this always-visible browser).
  // Download is explicitly lazy (spec: "heavy") — only fetched on click, and
  // handed to the browser as a file rather than held in memory/rendered.
  import { onMount } from "svelte";
  import { getSession, listSessions, resumeSession } from "../lib/rest";
  import type { SessionMeta } from "../lib/wire";

  let { onSelect }: { onSelect: (scanId: string) => void } = $props();

  let sessions = $state<SessionMeta[]>([]);
  let loadError = $state("");
  async function refresh() {
    try {
      sessions = await listSessions();
    } catch (e) {
      loadError = e instanceof Error ? e.message : String(e);
    }
  }
  onMount(refresh);

  let busyId = $state("");
  async function resume(id: string) {
    busyId = id;
    try {
      const { id: scanId } = await resumeSession(id);
      onSelect(scanId);
    } catch (e) {
      loadError = e instanceof Error ? e.message : String(e);
    } finally {
      busyId = "";
    }
  }

  async function download(id: string) {
    busyId = id;
    try {
      const state = await getSession(id);
      const blob = new Blob([JSON.stringify(state, null, 2)], { type: "application/json" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `${id}.session.json`;
      a.click();
      URL.revokeObjectURL(url);
    } catch (e) {
      loadError = e instanceof Error ? e.message : String(e);
    } finally {
      busyId = "";
    }
  }
</script>

<section class="sessions">
  <h2>Sessions</h2>
  {#if loadError}<p class="error">{loadError}</p>{/if}
  <ul>
    {#each sessions as s (s.id)}
      <li>
        <span class="mono id">{s.id}</span>
        <span class="mono target">{s.target}</span>
        <span class="saved">{new Date(s.saved_at).toLocaleString()}</span>
        <button type="button" disabled={busyId === s.id} onclick={() => resume(s.id)}>resume</button>
        <button type="button" disabled={busyId === s.id} onclick={() => download(s.id)}>download</button>
      </li>
    {/each}
    {#if sessions.length === 0}
      <li class="empty">no saved sessions</li>
    {/if}
  </ul>
  <button type="button" class="refresh" onclick={refresh}>refresh</button>
</section>

<style>
  .sessions {
    padding: 12px;
    overflow-y: auto;
  }
  ul {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  li {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 11px;
    padding: 4px 0;
    border-bottom: 1px solid var(--border);
    flex-wrap: wrap;
  }
  .id {
    color: var(--text-muted);
  }
  .target {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .saved {
    color: var(--text-muted);
    font-size: 10px;
  }
  .empty {
    color: var(--text-muted);
  }
  .refresh {
    margin-top: 6px;
    font-size: 10px;
  }
  .error {
    color: var(--critical);
    font-size: 11px;
  }
</style>
