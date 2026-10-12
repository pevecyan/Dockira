<script setup lang="ts">
import { computed } from 'vue'

// A tiny line of the last samples (0–max), as on the host cards.
const props = withDefaults(defineProps<{ values: number[]; max?: number; color?: string }>(), { max: 100, color: 'text-sky-500' })
const points = computed(() => {
  const h = props.values.length ? props.values : [0]
  const w = 120
  const step = w / Math.max(h.length - 1, 1)
  const max = Math.max(props.max, 1e-9)
  return h.map((v, i) => `${(i * step).toFixed(1)},${(28 - (Math.min(v, max) / max) * 26).toFixed(1)}`).join(' ')
})
</script>

<template>
  <svg viewBox="0 0 120 30" class="h-8 w-32" :class="color" preserveAspectRatio="none" aria-hidden="true">
    <polyline :points="points" fill="none" stroke="currentColor" stroke-width="1.5" vector-effect="non-scaling-stroke" />
  </svg>
</template>
