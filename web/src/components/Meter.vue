<script setup lang="ts">
import { computed } from 'vue'

// Usage bar: emerald, amber above 75 %, red above 92 %. Optional extra
// segments (stacked) are drawn before the main value.
const props = defineProps<{ value: number; max: number; segments?: { value: number; class: string; title?: string }[]; title?: string }>()
const pct = (v: number) => Math.max(0, Math.min(100, (v / (props.max || 1)) * 100))
const usedPct = computed(() => pct(props.value + (props.segments ?? []).reduce((a, s) => a + s.value, 0)))
const color = computed(() => (usedPct.value > 92 ? 'bg-red-500' : usedPct.value > 75 ? 'bg-amber-500' : 'bg-emerald-500'))
</script>

<template>
  <div class="flex h-2 overflow-hidden rounded-full bg-zinc-200 dark:bg-zinc-800" :title="title" role="meter" :aria-valuenow="Math.round(usedPct)" aria-valuemin="0" aria-valuemax="100">
    <div v-for="(s, i) in segments ?? []" :key="i" class="h-full transition-all duration-500" :class="s.class" :style="{ width: pct(s.value) + '%' }" :title="s.title" />
    <div class="h-full transition-all duration-500" :class="segments?.length ? 'bg-zinc-400 dark:bg-zinc-500' : color" :style="{ width: pct(value) + '%' }" />
  </div>
</template>
