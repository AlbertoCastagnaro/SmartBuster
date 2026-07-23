<script lang="ts">
  import Launcher from "./components/Launcher.svelte";
  import ScanDashboard from "./components/ScanDashboard.svelte";
  import SessionBrowser from "./components/SessionBrowser.svelte";

  let scanId = $state(new URLSearchParams(window.location.search).get("scan") ?? "");

  function select(id: string) {
    scanId = id;
    const url = new URL(window.location.href);
    url.searchParams.set("scan", id);
    window.history.replaceState({}, "", url);
  }
</script>

<div class="shell">
  <aside class="sidebar">
    <Launcher onSelect={select} />
    <SessionBrowser onSelect={select} />
  </aside>
  <main class="app-main">
    {#if scanId}
      {#key scanId}
        <ScanDashboard {scanId} />
      {/key}
    {:else}
      <div class="placeholder">
        <h1>smartbuster</h1>
        <p>start a scan or resume a session from the sidebar.</p>
      </div>
    {/if}
  </main>
</div>

<style>
  .shell {
    display: flex;
    min-height: 100vh;
  }
  .sidebar {
    width: 280px;
    flex: none;
    border-right: 1px solid var(--border);
    background: var(--surface-1);
    display: flex;
    flex-direction: column;
    overflow-y: auto;
  }
  .app-main {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .placeholder {
    margin: auto;
    text-align: center;
    color: var(--text-secondary);
  }
</style>
