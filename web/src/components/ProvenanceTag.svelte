<script lang="ts">
  import { parseProvenance, styleFor } from "../lib/provenance";

  let { raw }: { raw: string | undefined | null } = $props();
  let tags = $derived(parseProvenance(raw));
</script>

<span class="prov-tag-group" title={raw ?? ""}>
  {#each tags as tag (tag.category + (tag.detail ?? ""))}
    {@const style = styleFor(tag.category)}
    <span class="prov-chip" style="--dot: var({style.colorVar})">
      {style.label}{#if tag.detail && tag.category !== "crawl:html" && tag.category !== "crawl:js"}:{tag.detail}{/if}
      {#if tag.tags.length}<span class="prov-tags">({tag.tags.join(", ")})</span>{/if}
    </span>
  {/each}
  {#if tags.length === 0}
    <span class="prov-chip" style="--dot: var(--prov-unknown)">unknown</span>
  {/if}
</span>

<style>
  .prov-tag-group {
    display: inline-flex;
    gap: 3px;
    flex-wrap: wrap;
    align-items: center;
  }
  .prov-tags {
    color: var(--text-muted);
    margin-left: 2px;
  }
</style>
