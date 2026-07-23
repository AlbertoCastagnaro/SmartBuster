<script lang="ts">
  import { onDestroy } from "svelte";
  import { connectScanEvents, type ConnState } from "../lib/connection";
  import { getFindings } from "../lib/rest";
  import { createStore } from "../lib/store";
  import Controls from "./Controls.svelte";
  import EventLog from "./EventLog.svelte";
  import Findings from "./Findings.svelte";
  import Frontier from "./Frontier.svelte";
  import Gauges from "./Gauges.svelte";
  import Header from "./Header.svelte";
  import TechPanel from "./TechPanel.svelte";
  import Tree from "./Tree.svelte";

  let { scanId }: { scanId: string } = $props();
  // Snapshotted once: this component is remounted (via `{#key scanId}` in
  // App.svelte) when the user switches scans, rather than reacting to
  // scanId changing under it.
  const id = scanId;

  const store = createStore();
  let connState = $state<ConnState>("connecting");

  const conn = connectScanEvents(id);
  const offEvent = conn.onEvent((ev) => store.applyEvent(ev));
  const offResync = conn.onResync((status, isReconnect) => {
    store.applyResync(status, isReconnect);
    // Fetch on every (re)connect, not just isReconnect: a *first* connect
    // can just as easily be against a scan that already has findings (the
    // dashboard loaded fresh, or the user navigated straight to an
    // in-progress/finished scan's URL) — there's no "the tree is already
    // populated by earlier live events" guarantee to lean on. Cheap and
    // correct either way: an empty list for a brand-new scan, a populated
    // one otherwise (spec §3/§7 gap fix — ScanStatus alone only carries a
    // count, never the findings themselves).
    getFindings(id)
      .then((findings) => store.applyFindingsSnapshot(findings))
      .catch(() => {
        // Best-effort: if this fails, the tree/findings just stay as they
        // were (still correctly reflecting lifecycle via applyResync above).
      });
  });
  const offState = conn.onState((s) => (connState = s));

  onDestroy(() => {
    offEvent();
    offResync();
    offState();
    conn.close();
  });

  let storeState = $state(store.getState());
  store.subscribe((s) => (storeState = s));

  // Shared across Tree/Frontier (click a node -> prefill) and Controls
  // (spec §6: "reachable both from a pattern input and by clicking a
  // tree/frontier node"). Controls lands in a later build step.
  let prefillPattern = $state("");
</script>

<div class="dashboard">
  <Header data={storeState} {connState} />
  <Gauges data={storeState} />
  <Controls scanId={id} data={storeState} bind:prefillPattern onStatus={(status) => store.applyResync(status, false)} />
  <div class="panels">
    <div class="panel tree-col">
      <Tree data={storeState} onPick={(p) => (prefillPattern = p)} />
    </div>
    <div class="panel frontier-col">
      <Frontier data={storeState} onPick={(p) => (prefillPattern = p)} />
    </div>
    <div class="panel log-col">
      <EventLog data={storeState} />
    </div>
  </div>
  <div class="panels lower">
    <div class="panel tech-col">
      <TechPanel data={storeState} />
    </div>
    <div class="panel findings-col">
      <Findings data={storeState} />
    </div>
  </div>
</div>

<style>
  .dashboard {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-height: 0;
  }
  .panels {
    flex: 1;
    min-height: 0;
    display: flex;
    border-top: 1px solid var(--border);
  }
  .panel {
    min-height: 0;
    overflow: hidden;
    display: flex;
  }
  .tree-col {
    flex: 1 1 30%;
    border-right: 1px solid var(--border);
  }
  /* The frontier is the centerpiece (spec §4.4: "don't bury this; it's the
     demo") — it gets the largest, most central share of the layout. */
  .frontier-col {
    flex: 1.4 1 40%;
    border-right: 1px solid var(--border);
    background: var(--surface-1);
  }
  .log-col {
    flex: 1 1 30%;
  }
  .panels.lower {
    flex: 0 0 220px;
  }
  .tech-col {
    flex: 1 1 45%;
    border-right: 1px solid var(--border);
  }
  .findings-col {
    flex: 1 1 55%;
  }
</style>
