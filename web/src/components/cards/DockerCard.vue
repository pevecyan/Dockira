<script setup lang="ts">
import { computed, ref } from 'vue'
import { api, type HostInfo } from '../../api'
import { plural } from '../../format'
import { state, toast } from '../../state'

const props = defineProps<{ info: HostInfo | null }>()

const counts = computed(() => {
  let running = 0, stopped = 0, unhealthy = 0, total = 0
  for (const st of state.stacks) {
    for (const sv of st.services) {
      for (const c of sv.containers) {
        total++
        if (c.state === 'running') running++
        else stopped++
        if ((c.state === 'running' && c.health === 'unhealthy') || c.state === 'restarting') unhealthy++
      }
    }
  }
  return { running, stopped, unhealthy, total }
})
const attention = computed(() => state.stacks.filter((s) => ['unhealthy', 'restarting', 'invalid', 'partial'].includes(s.status)).length)
const healthy = computed(() => !state.dockerError && attention.value === 0)
const headline = computed(() => (state.dockerError ? 'Unreachable' : healthy.value ? 'Healthy' : `${attention.value} need attention`))

const busy = ref(false)
async function rescan() {
  busy.value = true
  try {
    const l = await api.rescan()
    state.stacks = l.stacks
    state.standalone = l.standalone
    toast('ok', `Found ${plural(l.stacks.length, 'stack')}`)
  } catch (e: any) {
    toast('bad', e.message)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="card flex flex-col p-4">
    <div class="flex items-start justify-between gap-2">
      <div class="min-w-0">
        <div class="text-xs font-medium uppercase tracking-wide text-zinc-500">Docker</div>
        <div class="mt-0.5 flex items-center gap-2 text-lg font-semibold">
          <span class="h-2.5 w-2.5 shrink-0 rounded-full" :class="state.dockerError ? 'bg-red-500' : healthy ? 'bg-emerald-500' : 'bg-amber-500'" />
          <span class="truncate">{{ headline }}</span>
        </div>
      </div>
      <div class="shrink-0 text-right text-xs text-zinc-500 tabular-nums">
        <div v-if="props.info?.system.version">engine {{ props.info.system.version }}</div>
        <div v-if="props.info?.compose_version">compose {{ props.info.compose_version }}</div>
      </div>
    </div>
    <div class="muted mt-1 text-sm">{{ plural(state.stacks.length, 'stack') }} · {{ plural(counts.total, 'container') }}</div>
    <div v-if="state.dockerError" class="mt-2 text-xs break-words text-red-600 dark:text-red-400">{{ state.dockerError }}</div>
    <div class="mt-auto border-t border-zinc-200 pt-3 dark:border-zinc-800" :class="state.dockerError ? 'mt-3' : 'mt-4'">
      <div class="flex items-end justify-between gap-2">
        <div class="grid grid-cols-3 gap-4 text-sm tabular-nums">
          <div><div class="text-xs text-zinc-500">running</div><div class="font-semibold">{{ counts.running }}</div></div>
          <div><div class="text-xs text-zinc-500">stopped</div><div class="font-semibold">{{ counts.stopped }}</div></div>
          <div>
            <div class="text-xs text-zinc-500">unhealthy</div>
            <div class="font-semibold" :class="counts.unhealthy ? 'text-red-600 dark:text-red-400' : ''">{{ counts.unhealthy }}</div>
          </div>
        </div>
        <button class="btn btn-sm" :disabled="busy" title="Scan apps_root and Docker again" @click="rescan">{{ busy ? 'Scanning…' : 'Rescan' }}</button>
      </div>
    </div>
  </div>
</template>
