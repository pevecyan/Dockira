<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { api, type AuditEntry } from '../api'
import { ago, dateTime } from '../format'
import { toast } from '../state'

const entries = ref<AuditEntry[]>([])
const q = ref('')
const more = ref(true)
let timer: number | undefined

async function load(append = false) {
  try {
    const before = append && entries.value.length ? entries.value[entries.value.length - 1].id : undefined
    const r = await api.audit({ limit: 100, before, q: q.value || undefined })
    entries.value = append ? [...entries.value, ...r.entries] : r.entries
    more.value = r.entries.length === 100
  } catch (e: any) {
    toast('bad', e.message)
  }
}
onMounted(() => load())
watch(q, () => {
  window.clearTimeout(timer)
  timer = window.setTimeout(() => load(), 300)
})

function details(d: string) {
  try {
    const o = JSON.parse(d)
    if (!o || typeof o !== 'object' || !Object.keys(o).length) return ''
    return Object.entries(o)
      .filter(([, v]) => v !== null && v !== '' && !(Array.isArray(v) && !v.length))
      .map(([k, v]) => `${k}: ${typeof v === 'object' ? JSON.stringify(v) : v}`)
      .join(' · ')
  } catch {
    return d
  }
}
</script>

<template>
  <div class="card">
    <div class="card-h">
      Audit log
      <input v-model="q" class="input min-w-0 !py-1 text-xs font-normal sm:!w-56" placeholder="Filter (action, stack, IP)…" aria-label="Filter audit log" />
    </div>
    <div v-if="!entries.length" class="muted px-4 py-8 text-center text-sm">Nothing recorded{{ q ? ' for this filter' : ' yet' }}.</div>
    <div class="relative overflow-x-auto">
      <table v-if="entries.length" class="w-full min-w-[40rem] text-sm">
        <thead>
          <tr class="border-b border-zinc-200 text-left text-xs text-zinc-500 dark:border-zinc-800">
            <th class="px-4 py-2 font-medium">When</th>
            <th class="px-2 py-2 font-medium">Action</th>
            <th class="px-2 py-2 font-medium">Target</th>
            <th class="px-2 py-2 font-medium">Details</th>
            <th class="px-4 py-2 font-medium">From</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="e in entries" :key="e.id" class="border-b border-zinc-100 align-top last:border-0 dark:border-zinc-800/70">
            <td class="px-4 py-2 text-xs whitespace-nowrap text-zinc-500" :title="dateTime(e.time)">{{ ago(e.time) }}</td>
            <td class="px-2 py-2 font-mono text-xs">
              {{ e.action }}
              <div v-if="e.result !== 'ok'" class="font-sans text-red-600 dark:text-red-400">{{ e.result }}</div>
            </td>
            <td class="px-2 py-2 text-xs break-all">{{ e.target }}</td>
            <td class="px-2 py-2 text-xs break-all text-zinc-500">{{ details(e.details) }}</td>
            <td class="px-4 py-2 font-mono text-xs text-zinc-500">{{ e.ip }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <div v-if="more && entries.length" class="border-t border-zinc-200 px-4 py-2 text-center dark:border-zinc-800">
      <button class="btn btn-sm" @click="load(true)">Older entries</button>
    </div>
  </div>
</template>
