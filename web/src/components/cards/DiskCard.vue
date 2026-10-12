<script setup lang="ts">
import type { HostInfo } from '../../api'
import { bytes, gib } from '../../format'
import { state } from '../../state'
import Meter from '../Meter.vue'

defineProps<{ info: HostInfo | null }>()
const tailPath = (p?: string) => (p && p.length > 34 ? '…' + p.slice(-33) : p ?? '')
</script>

<template>
  <div class="card flex flex-col p-4">
    <div class="text-xs font-medium uppercase tracking-wide text-zinc-500">Disk</div>
    <div class="font-semibold whitespace-nowrap tabular-nums" v-if="state.host?.disk_total">{{ gib(state.host.disk_used) }} / {{ gib(state.host.disk_total) }} GiB</div>
    <div class="font-semibold" v-else>–</div>
    <div class="truncate text-xs text-zinc-500" :title="`filesystem of ${state.host?.disk_path}`">{{ tailPath(state.host?.disk_path) }}</div>
    <div class="mt-3" v-if="state.host?.disk_total">
      <Meter :value="state.host.disk_used" :max="state.host.disk_total" />
      <div class="mt-1 text-[11px] text-zinc-500 tabular-nums">{{ gib(state.host.disk_free) }} GiB free</div>
    </div>
    <div class="mt-auto border-t border-zinc-200 pt-3 text-xs dark:border-zinc-800" :class="'mt-4'">
      <div v-if="info?.disk" class="grid grid-cols-2 gap-x-4 gap-y-1 tabular-nums">
        <span class="text-zinc-500">images</span><span class="text-right">{{ bytes(info.disk.images_size) }}</span>
        <span class="text-zinc-500">volumes</span><span class="text-right">{{ bytes(info.disk.volumes_size) }}</span>
        <span class="text-zinc-500">build cache</span><span class="text-right">{{ bytes(info.disk.build_cache_size) }}</span>
        <span class="text-zinc-500">reclaimable</span>
        <RouterLink to="/host" class="text-right text-emerald-700 hover:underline dark:text-emerald-400">{{ bytes(info.disk.images_reclaimable + info.disk.build_cache_reclaimable) }}</RouterLink>
      </div>
      <div v-else class="muted">Docker disk usage loading…</div>
    </div>
  </div>
</template>
