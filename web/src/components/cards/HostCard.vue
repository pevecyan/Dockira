<script setup lang="ts">
import { computed } from 'vue'
import type { HostStats } from '../../api'
import { duration, gib } from '../../format'
import { state } from '../../state'
import Meter from '../Meter.vue'
import Sparkline from '../Sparkline.vue'

// Host RAM (containers / other / free), swap, CPU with a sparkline and the load average.
const props = defineProps<{ host: HostStats }>()

const ram = computed(() => {
  const h = props.host
  const total = h.mem_total || 1
  const containers = Math.min(Object.values(state.cstats).reduce((a, c) => a + c.mem_bytes, 0), total)
  const other = Math.max(0, h.mem_used - containers)
  const free = Math.max(0, total - containers - other)
  return { containers, other, free }
})
const containersCPU = computed(() => Object.values(state.cstats).reduce((a, c) => a + c.cpu_pct, 0))
const swapping = computed(() => props.host.swap_used > 64 * 2 ** 20)
</script>

<template>
  <div class="card p-4">
    <div class="flex items-start justify-between gap-2">
      <div>
        <div class="text-xs font-medium uppercase tracking-wide text-zinc-500">Host</div>
        <div class="font-semibold">{{ host.cores }} cores · {{ gib(host.mem_total) }} GiB RAM</div>
      </div>
      <div class="text-right text-xs text-zinc-500 tabular-nums">
        <div title="load average 1 / 5 / 15 min">load {{ host.load[0].toFixed(2) }}</div>
        <div v-if="host.uptime_sec">up {{ duration(host.uptime_sec) }}</div>
      </div>
    </div>
    <div class="mt-3">
      <div class="mb-1 flex justify-between text-xs tabular-nums">
        <span class="text-zinc-500">RAM</span>
        <span>{{ gib(host.mem_used) }} / {{ gib(host.mem_total) }} GiB</span>
      </div>
      <Meter
        :value="ram.other"
        :max="host.mem_total"
        :segments="[{ value: ram.containers, class: host.mem_used / host.mem_total > 0.92 ? 'bg-red-500' : host.mem_used / host.mem_total > 0.75 ? 'bg-amber-500' : 'bg-emerald-500', title: `containers: ${gib(ram.containers)} GiB` }]"
        :title="`other: ${gib(ram.other)} GiB`"
      />
      <div class="mt-1 flex flex-wrap gap-x-3 text-[11px] text-zinc-500 tabular-nums">
        <span class="flex items-center gap-1"><span class="h-2 w-2 rounded-sm bg-emerald-500" />containers {{ gib(ram.containers) }}</span>
        <span class="flex items-center gap-1"><span class="h-2 w-2 rounded-sm bg-zinc-400 dark:bg-zinc-500" />other {{ gib(ram.other) }}</span>
        <span>free {{ gib(ram.free) }}</span>
      </div>
      <div v-if="host.swap_total" class="mt-1 text-[11px] tabular-nums" :class="swapping ? 'font-medium text-amber-600 dark:text-amber-400' : 'text-zinc-500'">
        swap {{ gib(host.swap_used) }} / {{ gib(host.swap_total) }} GiB<template v-if="swapping"> · in use</template>
      </div>
    </div>
    <div class="mt-3 flex items-end justify-between gap-3">
      <div>
        <div class="text-xs whitespace-nowrap text-zinc-500">CPU ({{ Math.round(containersCPU) }}% containers)</div>
        <div class="text-xl font-semibold tabular-nums">{{ Math.round(host.cpu_pct) }}%</div>
      </div>
      <Sparkline :values="state.cpuHistory" />
    </div>
  </div>
</template>
