<script setup lang="ts">
// Time series with a filled area (host CPU and memory history):
// series colours from the palette, a hover line with a tooltip.
import { computed, ref } from 'vue'

const props = withDefaults(
  defineProps<{ title: string; points: { t: number; v: number }[]; max?: number; unit?: string; color?: string; format?: (v: number) => string }>(),
  { color: 'var(--series-1)', unit: '' },
)

const W = 600
const H = 160
const PAD = { l: 40, r: 8, t: 8, b: 22 }
const pw = W - PAD.l - PAD.r
const ph = H - PAD.t - PAD.b

const t0 = computed(() => props.points[0]?.t ?? 0)
const t1 = computed(() => props.points[props.points.length - 1]?.t ?? 1)
const top = computed(() => props.max ?? Math.max(1, ...props.points.map((p) => p.v)) * 1.1)
const x = (t: number) => PAD.l + (t1.value === t0.value ? pw : ((t - t0.value) / (t1.value - t0.value)) * pw)
const y = (v: number) => PAD.t + ph - (Math.min(v, top.value) / top.value) * ph
const line = computed(() => props.points.map((p, i) => `${i ? 'L' : 'M'}${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join(' '))
const area = computed(() => (props.points.length > 1 ? `${line.value} L${x(t1.value)},${PAD.t + ph} L${x(t0.value)},${PAD.t + ph} Z` : ''))
const fmt = (v: number) => (props.format ? props.format(v) : `${v.toFixed(v >= 10 ? 0 : 1)}${props.unit}`)
const ticks = computed(() => [0, 0.25, 0.5, 0.75, 1].map((f) => f * top.value))
const timeTicks = computed(() => {
  if (props.points.length < 2) return []
  return [0, 1 / 3, 2 / 3, 1].map((f) => t0.value + f * (t1.value - t0.value))
})
const clock = (t: number) => new Date(t).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false })

const hover = ref<number | null>(null)
function onMove(e: MouseEvent) {
  const r = (e.currentTarget as SVGSVGElement).getBoundingClientRect()
  const px = ((e.clientX - r.left) / r.width) * W
  let best = 0
  props.points.forEach((p, i) => {
    if (Math.abs(x(p.t) - px) < Math.abs(x(props.points[best].t) - px)) best = i
  })
  hover.value = props.points.length ? best : null
}
const last = computed(() => props.points[props.points.length - 1])
</script>

<template>
  <div class="relative">
    <div class="mb-1 flex items-baseline justify-between text-xs">
      <span class="font-semibold">{{ title }}</span>
      <span v-if="last" class="tabular-nums text-zinc-500">now {{ fmt(last.v) }}</span>
    </div>
    <svg :viewBox="`0 0 ${W} ${H}`" class="h-auto w-full" role="img" :aria-label="title" @mousemove="onMove" @mouseleave="hover = null">
      <g class="text-zinc-400 dark:text-zinc-500">
        <template v-for="tk in ticks" :key="tk">
          <line :x1="PAD.l" :x2="W - PAD.r" :y1="y(tk)" :y2="y(tk)" stroke="currentColor" stroke-opacity="0.2" />
          <text :x="PAD.l - 5" :y="y(tk) + 3.5" text-anchor="end" font-size="10" fill="currentColor">{{ fmt(tk) }}</text>
        </template>
        <text v-for="(tt, i) in timeTicks" :key="tt" :x="x(tt)" :y="H - 6" :text-anchor="i === 0 ? 'start' : i === timeTicks.length - 1 ? 'end' : 'middle'" font-size="10" fill="currentColor">{{ clock(tt) }}</text>
      </g>
      <path v-if="area" :d="area" :fill="color" fill-opacity="0.12" />
      <path v-if="points.length > 1" :d="line" fill="none" :stroke="color" stroke-width="1.75" stroke-linejoin="round" />
      <template v-if="hover !== null">
        <line :x1="x(points[hover].t)" :x2="x(points[hover].t)" :y1="PAD.t" :y2="PAD.t + ph" stroke="currentColor" stroke-opacity="0.3" />
        <circle :cx="x(points[hover].t)" :cy="y(points[hover].v)" r="3.5" :fill="color" stroke="var(--chart-surface)" stroke-width="2" />
      </template>
    </svg>
    <div v-if="!points.length" class="muted absolute inset-0 flex items-center justify-center text-xs">collecting samples…</div>
    <div
      v-if="hover !== null"
      class="pointer-events-none absolute top-5 rounded-md border border-zinc-200 bg-white px-2 py-1 text-xs shadow dark:border-zinc-700 dark:bg-zinc-900"
      :style="{ left: `min(calc(${(x(points[hover].t) / W) * 100}% + 8px), calc(100% - 7rem))` }"
    >
      <b>{{ clock(points[hover].t) }}</b> {{ fmt(points[hover].v) }}
    </div>
  </div>
</template>
