<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { api, type HostInfo } from '../api'
import { shortImage } from '../format'
import { state } from '../state'
import DiskCard from '../components/cards/DiskCard.vue'
import DockerCard from '../components/cards/DockerCard.vue'
import HostCard from '../components/cards/HostCard.vue'
import UpdatesCard from '../components/cards/UpdatesCard.vue'
import FeedPanel from '../components/FeedPanel.vue'
import StacksTable from '../components/StacksTable.vue'
import StatusBadge from '../components/StatusBadge.vue'

const info = ref<HostInfo | null>(null)
async function load() {
  try {
    info.value = await api.host()
    if (!state.host) state.host = info.value.host
    if (info.value.containers && !Object.keys(state.cstats).length) state.cstats = info.value.containers
  } catch {
    /* the cards show what they have */
  }
}
onMounted(load)
const timer = window.setInterval(load, 60_000) // docker df and versions change slowly
onBeforeUnmount(() => window.clearInterval(timer))
</script>

<template>
  <div class="space-y-4">
    <div class="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
      <DockerCard :info="info" />
      <UpdatesCard />
      <HostCard v-if="state.host" :host="state.host" />
      <div v-else class="card muted p-4 text-sm">Host figures loading…</div>
      <DiskCard :info="info" />
    </div>

    <StacksTable />

    <div v-if="state.standalone.length" class="card">
      <div class="card-h"><span>Other containers <span class="font-normal text-zinc-500">({{ state.standalone.length }}, not started by compose)</span></span></div>
      <ul class="divide-y divide-zinc-100 text-sm dark:divide-zinc-800/70">
        <li v-for="c in state.standalone" :key="c.id" class="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2">
          <span class="font-mono text-xs">{{ c.name }}</span>
          <StatusBadge :status="c.health && c.state === 'running' ? c.health : c.state" :exit-code="c.exit_code" :title="c.status" />
          <span class="muted min-w-0 flex-1 truncate text-xs" :title="c.image">{{ c.image.startsWith('sha256:') ? c.image.slice(7, 19) : shortImage(c.image) }}</span>
          <span class="muted text-xs">{{ c.status }}</span>
        </li>
      </ul>
    </div>

    <FeedPanel />
  </div>
</template>
