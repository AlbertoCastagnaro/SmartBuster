<script lang="ts">
  let { values, color = "var(--accent)" }: { values: number[]; color?: string } = $props();

  let path = $derived.by(() => {
    if (values.length < 2) return "";
    const max = Math.max(...values, 0.0001);
    const w = 100;
    const h = 24;
    const step = w / (values.length - 1);
    return values.map((v, i) => `${i === 0 ? "M" : "L"}${(i * step).toFixed(1)},${(h - (v / max) * h).toFixed(1)}`).join(" ");
  });
</script>

<svg viewBox="0 0 100 24" preserveAspectRatio="none" class="spark">
  {#if path}
    <path d={path} fill="none" stroke={color} stroke-width="1.5" vector-effect="non-scaling-stroke" />
  {/if}
</svg>

<style>
  .spark {
    width: 64px;
    height: 18px;
    display: block;
  }
</style>
